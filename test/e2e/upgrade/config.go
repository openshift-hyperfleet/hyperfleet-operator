// Copyright 2026. Licensed under the Apache License, Version 2.0.

// Package upgrade tests published operator releases through OLM Classic. It does
// not import the current controller: expectations describe the selected releases.
package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

const (
	packageName      = "hyperfleet-operator"
	hfcName          = "cluster"
	hashAnnotation   = "hyperfleet.redhat.com/config-hash"
	runLabel         = "hyperfleet.redhat.com/upgrade-run"
	catalogName      = "upgrade-catalog"
	subscriptionName = "upgrade"
	lockName         = "hyperfleet-operator-upgrade"
)

// Config identifies immutable artifacts and an explicitly selected cluster.
type Config struct {
	Kubeconfig, Catalog, FromBundle, ToBundle, Channel, Namespace, ArtifactDir, OPM string
	Skopeo                                                                          string
	Timeout, PollInterval                                                           time.Duration
	Scenario                                                                        Scenario
}

// Scenario is a release-pair contract, not a new public operator API. Paths are
// relative to the scenario file. Fixtures are created in the run namespace.
type Scenario struct {
	Fixtures         string              `json:"fixtures"`
	HyperFleetConfig map[string]any      `json:"hyperFleetConfig"`
	From             ReleaseExpectations `json:"from"`
	To               ReleaseExpectations `json:"to"`
	// MaxUnavailable bounds aggregate unavailability caused by added components.
	// Existing components must remain available regardless of this bound.
	MaxUnavailable string      `json:"maxUnavailable"`
	DataProbe      ProbeConfig `json:"dataProbe"`
	SettleDuration string      `json:"settleDuration"`
}

// ReleaseExpectations enumerates components shipped by one release. Older
// releases without status.components can explicitly set ComponentStatus=false.
type ReleaseExpectations struct {
	ComponentStatus bool        `json:"componentStatus"`
	Components      []Component `json:"components"`
}

// Component maps a workload to the related image in its release's CSV.
type Component struct {
	Name               string `json:"name"`
	Deployment         string `json:"deployment"`
	Container          string `json:"container"`
	RelatedImage       string `json:"relatedImage"`
	MigrationContainer string `json:"migrationContainer,omitempty"`
	// HashChange is required on To for components present in both releases.
	HashChange *bool `json:"hashChange,omitempty"`
}

// ProbeConfig describes a non-sensitive API record used to prove data survives.
// A fresh port-forward is opened for each phase, so an old pod cannot mask failure.
type ProbeConfig struct {
	Service    string         `json:"service"`
	Port       int            `json:"port"`
	Collection string         `json:"collection"`
	Body       map[string]any `json:"body"`
	TokenFile  string         `json:"tokenFile,omitempty"`
	CAFile     string         `json:"caFile,omitempty"`
	ServerName string         `json:"serverName,omitempty"`
}

// LoadConfig fails closed on missing inputs. No default kubeconfig, image tags,
// release pair or component inventory is inferred from the developer's machine.
func LoadConfig() (Config, error) {
	c := Config{Kubeconfig: os.Getenv(kubeconfigEnv), Catalog: os.Getenv(catalogEnv),
		FromBundle: os.Getenv(fromBundleEnv), ToBundle: os.Getenv("UPGRADE_TO_BUNDLE"),
		Channel: os.Getenv(channelEnv), Namespace: os.Getenv("UPGRADE_NAMESPACE"),
		ArtifactDir: os.Getenv("ARTIFACT_DIR"), OPM: os.Getenv("OPM"),
		Timeout: 15 * time.Minute, PollInterval: 2 * time.Second}
	if c.OPM == "" {
		c.OPM = "opm"
	}
	c.Skopeo = os.Getenv("SKOPEO")
	if c.Skopeo == "" {
		c.Skopeo = "skopeo"
	}
	if c.Namespace == "" {
		c.Namespace = "hyperfleet-upgrade"
	}
	for name, value := range map[string]string{kubeconfigEnv: c.Kubeconfig, "ARTIFACT_DIR": c.ArtifactDir,
		channelEnv: c.Channel, "UPGRADE_SCENARIO": os.Getenv("UPGRADE_SCENARIO")} {
		if strings.TrimSpace(value) == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	for name, image := range map[string]string{catalogEnv: c.Catalog,
		fromBundleEnv: c.FromBundle, "UPGRADE_TO_BUNDLE": c.ToBundle} {
		if err := pinnedImage(image); err != nil {
			return c, fmt.Errorf("%s: %w", name, err)
		}
	}
	if c.FromBundle == c.ToBundle {
		return c, fmt.Errorf("the two bundles must differ")
	}
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 || strings.HasPrefix(c.Namespace, "kube-") {
		return c, fmt.Errorf("UPGRADE_NAMESPACE must be a non-system DNS label")
	}
	if value := os.Getenv("UPGRADE_TIMEOUT"); value != "" {
		var err error
		c.Timeout, err = time.ParseDuration(value)
		if err != nil || c.Timeout <= 0 {
			return c, fmt.Errorf("UPGRADE_TIMEOUT must be a positive duration")
		}
	}
	path, err := filepath.Abs(os.Getenv("UPGRADE_SCENARIO"))
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read scenario: %w", err)
	}
	if err = yaml.UnmarshalStrict(data, &c.Scenario); err != nil {
		return c, fmt.Errorf("scenario is not valid YAML or contains unknown fields")
	}
	base := filepath.Dir(path)
	for _, p := range []*string{&c.Scenario.Fixtures, &c.Scenario.DataProbe.TokenFile, &c.Scenario.DataProbe.CAFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, c.Scenario.validate()
}

func pinnedImage(image string) error {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return fmt.Errorf("a valid digest-pinned image is required")
	}
	if _, ok := ref.(reference.Digested); !ok {
		return fmt.Errorf("image must be pinned by digest")
	}
	return nil
}

func (s Scenario) validate() error {
	if s.Fixtures == "" {
		return fmt.Errorf("scenario.fixtures is required")
	}
	hfc := &unstructured.Unstructured{Object: s.HyperFleetConfig}
	if hfc.GetAPIVersion() != "hyperfleet.redhat.com/v1alpha1" || hfc.GetKind() != "HyperFleetConfig" ||
		hfc.GetName() != hfcName || hfc.GetNamespace() != "" {
		return fmt.Errorf("scenario must contain the cluster-scoped HyperFleetConfig/cluster")
	}
	if hfc.GetUID() != "" || hfc.GetResourceVersion() != "" || len(hfc.GetOwnerReferences()) != 0 {
		return fmt.Errorf("HyperFleetConfig must be a new resource")
	}
	if _, found := hfc.Object["status"]; found {
		return fmt.Errorf("HyperFleetConfig must not contain status")
	}
	for _, release := range []ReleaseExpectations{s.From, s.To} {
		if err := release.validate(); err != nil {
			return err
		}
	}
	for _, old := range s.From.Components {
		found := false
		for _, next := range s.To.Components {
			if old.Name == next.Name {
				found = true
				if next.HashChange == nil || old.Deployment != next.Deployment {
					return fmt.Errorf("to component %s requires hashChange and the original deployment", next.Name)
				}
			}
		}
		if !found {
			return fmt.Errorf("component removal is outside this healthy rolling-upgrade scenario: %s", old.Name)
		}
	}
	if s.To.api().MigrationContainer == "" {
		return fmt.Errorf("target API migrationContainer is required")
	}
	if _, err := s.gap(); err != nil {
		return err
	}
	if _, err := s.settle(); err != nil {
		return err
	}
	p := s.DataProbe
	if len(validation.IsDNS1123Label(p.Service)) != 0 || p.Port < 1 || p.Port > 65535 ||
		!strings.HasPrefix(p.Collection, "/api/") || strings.ContainsAny(p.Collection, "?#") ||
		strings.Contains(p.Collection, "..") || len(p.Body) == 0 {
		return fmt.Errorf("dataProbe requires a Service, port, API collection path and non-empty body")
	}
	if (p.CAFile == "") != (p.ServerName == "") {
		return fmt.Errorf("dataProbe CAFile and serverName must be set together")
	}
	return nil
}

func (r ReleaseExpectations) validate() error {
	seenNames, seenDeployments := map[string]bool{}, map[string]bool{}
	for _, c := range r.Components {
		if c.Name == "" || c.Container == "" || c.RelatedImage == "" ||
			len(validation.IsDNS1123Subdomain(c.Deployment)) != 0 {
			return fmt.Errorf("every component needs name, deployment, container and relatedImage")
		}
		if seenNames[c.Name] || seenDeployments[c.Deployment] {
			return fmt.Errorf("duplicate component or deployment")
		}
		seenNames[c.Name], seenDeployments[c.Deployment] = true, true
	}
	if !seenNames[apiComponentName] {
		return fmt.Errorf("each release must include the api component")
	}
	return nil
}

func (r ReleaseExpectations) api() Component {
	for _, c := range r.Components {
		if c.Name == apiComponentName {
			return c
		}
	}
	return Component{}
}

func (s Scenario) gap() (time.Duration, error) {
	if s.MaxUnavailable == "" {
		return 0, fmt.Errorf("maxUnavailable is required (use 0s for continuous availability)")
	}
	d, err := time.ParseDuration(s.MaxUnavailable)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("maxUnavailable must be a non-negative duration")
	}
	return d, nil
}

func (s Scenario) settle() (time.Duration, error) {
	if s.SettleDuration == "" {
		return 15 * time.Second, nil
	}
	d, err := time.ParseDuration(s.SettleDuration)
	if err != nil || d < 5*time.Second {
		return 0, fmt.Errorf("settleDuration must be at least 5s")
	}
	return d, nil
}

// podImages includes init containers; migrations are part of the shipped payload.
func podImages(spec corev1.PodSpec) []corev1.Container {
	return append(append([]corev1.Container{}, spec.Containers...), spec.InitContainers...)
}

// Shared OLM and JSON field names used in dynamic resource contracts.
const (
	bundleSchema         = "olm.bundle"
	csvKind              = "ClusterServiceVersion"
	subscriptionKind     = "Subscription"
	approvalManual       = "Manual"
	phaseFailed          = "Failed"
	apiComponentName     = "api"
	fieldKind            = "kind"
	fieldName            = "name"
	fieldNamespace       = "namespace"
	fieldSpec            = "spec"
	fieldPhase           = "phase"
	fieldPath            = "path"
	fieldValue           = "value"
	fieldImage           = "image"
	fieldChannel         = "channel"
	fieldUID             = "uid"
	fieldSourceNamespace = "sourceNamespace"
	appsGroup            = "apps"
	conditionAvailable   = "Available"
)

const (
	kubeconfigEnv   = "KUBECONFIG"
	channelEnv      = "UPGRADE_CHANNEL"
	catalogEnv      = "UPGRADE_CATALOG"
	fromBundleEnv   = "UPGRADE_FROM_BUNDLE"
	systemNamespace = "kube-system"
)
