// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/version"
)

const bundleObjectProperty = "olm.bundle.object"

type property struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}
type channelEntry struct {
	Name      string   `json:"name"`
	Replaces  string   `json:"replaces"`
	Skips     []string `json:"skips"`
	SkipRange string   `json:"skipRange"`
}
type catalogObject struct {
	Schema     string         `json:"schema"`
	Name       string         `json:"name"`
	Package    string         `json:"package"`
	Image      string         `json:"image"`
	Entries    []channelEntry `json:"entries"`
	Properties []property     `json:"properties"`
}

type release struct {
	Bundle string
	CSV    operatorsv1alpha1.ClusterServiceVersion
	Images map[string]string
}
type releasePair struct {
	From, To   release
	OPMVersion string
}

type commandFunc func(context.Context, string, ...string) ([]byte, error)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Never run through a shell; registry authentication stays in opm's normal
	// credential files. Its stderr is intentionally not copied into CI artifacts.
	cmd := exec.CommandContext(ctx, name, args...)
	// Output otherwise embeds stderr in ExitError, which Gomega can print even
	// when Error() omits it. Never retain unredacted command diagnostics.
	cmd.Stderr = io.Discard
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return data, nil
}

func preflight(ctx context.Context, c Config, run commandFunc) (releasePair, error) {
	var pair releasePair
	for _, item := range []struct {
		image string
		into  *release
	}{{c.FromBundle, &pair.From}, {c.ToBundle, &pair.To}} {
		data, err := run(ctx, c.OPM, "render", item.image, "--output=json")
		if err != nil {
			return pair, fmt.Errorf("render bundle: %w", err)
		}
		*item.into, err = parseBundle(data, item.image)
		if err != nil {
			return pair, err
		}
	}
	if err := validatePair(pair, c.Scenario); err != nil {
		return pair, err
	}
	data, err := run(ctx, c.OPM, "render", c.Catalog, "--output=json")
	if err != nil {
		return pair, fmt.Errorf("render catalog: %w", err)
	}
	if err = validateCatalog(data, c.Channel, pair); err != nil {
		return pair, err
	}
	// opm validates the entire graph, including schemas/invariants outside the
	// selected edge. Store only in a private temporary directory, not artifacts.
	dir, err := os.MkdirTemp("", "hyperfleet-upgrade-catalog-")
	if err != nil {
		return pair, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // Temporary metadata only; no cluster resources.
	if err = os.WriteFile(filepath.Join(dir, "catalog.json"), data, 0600); err != nil {
		return pair, err
	}
	if _, err = run(ctx, c.OPM, "validate", dir); err != nil {
		return pair, fmt.Errorf("catalog validation: %w", err)
	}
	toolVersion, err := run(ctx, c.OPM, "version")
	if err != nil {
		return pair, fmt.Errorf("record opm version: %w", err)
	}
	pair.OPMVersion = strings.TrimSpace(string(toolVersion))
	return pair, nil
}

func catalogObjects(data []byte) ([]catalogObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var result []catalogObject
	for {
		var obj catalogObject
		if err := decoder.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("opm output is not a JSON object stream")
		}
		result = append(result, obj)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("opm returned no catalog objects")
	}
	return result, nil
}

func parseBundle(data []byte, image string) (release, error) {
	r := release{Bundle: image, Images: map[string]string{}}
	objects, err := catalogObjects(data)
	if err != nil {
		return r, err
	}
	count := 0
	for _, obj := range objects {
		if obj.Schema != bundleSchema {
			continue
		}
		count++
		if obj.Package != packageName || obj.Image != image {
			return r, fmt.Errorf("bundle identity does not match requested image/package")
		}
		found := false
		for _, prop := range obj.Properties {
			if prop.Type != bundleObjectProperty {
				continue
			}
			var value struct {
				Data []byte `json:"data"`
			}
			if json.Unmarshal(prop.Value, &value) != nil {
				return r, fmt.Errorf("invalid olm.bundle.object property")
			}
			var meta metav1.TypeMeta
			if json.Unmarshal(value.Data, &meta) != nil {
				return r, fmt.Errorf("invalid bundle manifest")
			}
			if meta.Kind != csvKind {
				continue
			}
			document, err := decodeCSV(value.Data)
			if err != nil {
				return r, err
			}
			if found {
				return r, fmt.Errorf("bundle contains multiple CSVs")
			}
			found, r.CSV = true, document
		}
		if !found || r.CSV.Name != obj.Name {
			return r, fmt.Errorf("bundle CSV is missing or disagrees with catalog identity")
		}
	}
	if count != 1 {
		return r, fmt.Errorf("expected exactly one rendered bundle")
	}
	return r, r.validate()
}

// decodeCSV preserves the required version check before the official type can
// turn an absent spec.version into the zero-value version 0.0.0.
func decodeCSV(data []byte) (operatorsv1alpha1.ClusterServiceVersion, error) {
	var csv operatorsv1alpha1.ClusterServiceVersion
	var obj unstructured.Unstructured
	if err := json.Unmarshal(data, &obj.Object); err != nil {
		return csv, fmt.Errorf("invalid CSV document")
	}
	v, _, err := unstructured.NestedString(obj.Object, fieldSpec, "version")
	if err != nil {
		return csv, fmt.Errorf("invalid CSV version")
	}
	if _, err = version.ParseSemantic(v); err != nil {
		return csv, fmt.Errorf("invalid or missing CSV version")
	}
	if err = json.Unmarshal(data, &csv); err != nil {
		return csv, fmt.Errorf("invalid CSV document")
	}
	return csv, nil
}

func (r *release) validate() error {
	allNamespaces := false
	for _, mode := range r.CSV.Spec.InstallModes {
		if mode.Type == operatorsv1alpha1.InstallModeTypeAllNamespaces && mode.Supported {
			allNamespaces = true
		}
	}
	if !allNamespaces {
		return fmt.Errorf("CSV must support AllNamespaces for the cluster-scoped singleton")
	}
	if len(r.CSV.Spec.CustomResourceDefinitions.Owned) != 1 ||
		r.CSV.Spec.CustomResourceDefinitions.Owned[0].Name != hfcCRD {
		return fmt.Errorf("upgrade harness supports bundles owning only %s", hfcCRD)
	}
	for _, image := range r.CSV.Spec.RelatedImages {
		if err := pinnedImage(image.Image); err != nil {
			return fmt.Errorf("CSV related image %s: %w", image.Name, err)
		}
		name := normalizeImageName(image.Name)
		if _, exists := r.Images[name]; exists || name == "" {
			return fmt.Errorf("duplicate or empty related image name")
		}
		r.Images[name] = image.Image
	}
	if len(r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs) == 0 {
		return fmt.Errorf("CSV has no operator deployments")
	}
	foundAPI := false
	for _, dep := range r.CSV.Spec.InstallStrategy.StrategySpec.DeploymentSpecs {
		for _, container := range podImages(dep.Spec.Template.Spec) {
			if err := pinnedImage(container.Image); err != nil {
				return fmt.Errorf("operator container: %w", err)
			}
			listed := false
			for _, image := range r.Images {
				listed = listed || image == container.Image
			}
			if !listed {
				return fmt.Errorf("operator container image missing from relatedImages")
			}
			for _, env := range container.Env {
				if !strings.HasPrefix(env.Name, "RELATED_IMAGE_") {
					continue
				}
				if env.ValueFrom != nil || r.Images[normalizeImageName(strings.TrimPrefix(env.Name,
					"RELATED_IMAGE_"))] != env.Value {
					return fmt.Errorf("%s disagrees with relatedImages", env.Name)
				}
				foundAPI = foundAPI || env.Name == "RELATED_IMAGE_HYPERFLEET_API"
			}
		}
	}
	if !foundAPI {
		return fmt.Errorf("CSV is missing RELATED_IMAGE_HYPERFLEET_API")
	}
	return nil
}

func normalizeImageName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

func validatePair(pair releasePair, s Scenario) error {
	if s.To.HashContract == nil || s.To.HashContract.Bundle != pair.To.Bundle {
		return fmt.Errorf("target hash contract must match the exact target bundle digest")
	}
	if err := s.validateTargetHashContract(); err != nil {
		return err
	}
	from, err := version.ParseSemantic(pair.From.CSV.Spec.Version.String())
	if err != nil {
		return fmt.Errorf("invalid previous CSV version")
	}
	to, err := version.ParseSemantic(pair.To.CSV.Spec.Version.String())
	if err != nil {
		return fmt.Errorf("invalid target CSV version")
	}
	if from.PreRelease() != "" || !strings.HasPrefix(to.PreRelease(),
		"rc") || from.Major() != to.Major() || !to.GreaterThan(from) {
		return fmt.Errorf("expected a previous final release and newer RC in the same major version")
	}
	for _, item := range []struct {
		release  release
		expected ReleaseExpectations
	}{{pair.From, s.From}, {pair.To, s.To}} {
		for _, c := range item.expected.Components {
			if item.release.Images[normalizeImageName(c.RelatedImage)] == "" {
				return fmt.Errorf("component %s refers to an image absent from its CSV", c.Name)
			}
		}
	}
	oldImage := pair.From.Images[normalizeImageName(s.From.api().RelatedImage)]
	newImage := pair.To.Images[normalizeImageName(s.To.api().RelatedImage)]
	oldRef, oldErr := reference.ParseNormalizedNamed(oldImage)
	newRef, newErr := reference.ParseNormalizedNamed(newImage)
	oldDigest, oldOK := oldRef.(reference.Digested)
	newDigest, newOK := newRef.(reference.Digested)
	if oldErr != nil || newErr != nil || !oldOK || !newOK {
		return fmt.Errorf("release pair API images must be pinned by digest")
	}
	if oldDigest.Digest() == newDigest.Digest() {
		return fmt.Errorf("release pair must change the API image digest to prove an API image transition")
	}
	return nil
}

func validateCatalog(data []byte, channel string, pair releasePair) error {
	objects, err := catalogObjects(data)
	if err != nil {
		return err
	}
	bundles := map[string]string{}
	var entries []channelEntry
	channels := 0
	for _, obj := range objects {
		if obj.Package != packageName {
			continue
		}
		if obj.Schema == bundleSchema {
			if _, ok := bundles[obj.Name]; ok {
				return fmt.Errorf("duplicate catalog bundle")
			}
			bundles[obj.Name] = obj.Image
		}
		if obj.Schema == "olm.channel" && obj.Name == channel {
			entries = obj.Entries
			channels++
		}
	}
	from, to := pair.From.CSV.Name, pair.To.CSV.Name
	if channels != 1 || bundles[from] != pair.From.Bundle || bundles[to] != pair.To.Bundle {
		return fmt.Errorf("catalog must contain the exact bundle pair in one selected channel")
	}
	foundFrom, foundTo := false, false
	for _, e := range entries {
		foundFrom = foundFrom || e.Name == from
		if e.Name == to {
			if e.Replaces != from {
				return fmt.Errorf("target must directly replace the previous release")
			}
			foundTo = true
		}
		if e.Replaces == to {
			return fmt.Errorf("target is not the channel head")
		}
		for _, skip := range e.Skips {
			if skip == from || skip == to {
				return fmt.Errorf("selected pair must not be skipped")
			}
		}
		if e.SkipRange != "" {
			return fmt.Errorf("selected channel must use explicit replaces edges, not skipRange")
		}
	}
	if !foundFrom || !foundTo {
		return fmt.Errorf("both CSVs must be installable in the selected channel")
	}
	return nil
}
