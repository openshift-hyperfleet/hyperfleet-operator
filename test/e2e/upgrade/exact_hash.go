// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// oracleSecret contains only a Secret's role and metadata resourceVersion. The
// test never needs its data to independently calculate the release's v1 hash.
type oracleSecret struct {
	role            string
	present         bool
	resourceVersion string
}

// configHashV1 is a test-side implementation of the release-qualified hash
// contract. Do not call the controller's computeConfigHash: published releases
// may use different source code, and the assertion needs an independent oracle.
func configHashV1(configValues []string, secrets []oracleSecret) string {
	config := strings.Join(configValues, "\x00")
	ordered := slices.Clone(secrets)
	slices.SortFunc(ordered, func(a, b oracleSecret) int { return strings.Compare(a.role, b.role) })
	h := sha256.New()
	writeHashField(h, []byte(config))
	for _, secret := range ordered {
		writeHashField(h, []byte(secret.role))
		if !secret.present {
			_, _ = h.Write([]byte{0})
			continue
		}
		_, _ = h.Write([]byte{1})
		writeHashField(h, []byte(secret.resourceVersion))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeHashField(h hash.Hash, value []byte) {
	_, _ = h.Write([]byte(strconv.Itoa(len(value))))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write(value)
	_, _ = h.Write([]byte{0})
}

type hashSourceVersion struct {
	Kind            string    `json:"kind"`
	Name            string    `json:"name"`
	KeyOrRole       string    `json:"keyOrRole"`
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
}

// exactHashSnapshot exports only identities and digests; it never contains
// rendered ConfigMap content or Secret payloads.
type exactHashSnapshot struct {
	Component            string              `json:"component"`
	Expected             string              `json:"expected"`
	Actual               string              `json:"actual"`
	DeploymentUID        types.UID           `json:"deploymentUID"`
	DeploymentGeneration int64               `json:"deploymentGeneration"`
	Sources              []hashSourceVersion `json:"sources"`
}

func (s exactHashSnapshot) stable(next exactHashSnapshot) bool {
	return s.Component == next.Component && s.Expected == next.Expected && s.Actual == next.Actual &&
		s.DeploymentUID == next.DeploymentUID && s.DeploymentGeneration == next.DeploymentGeneration &&
		slices.Equal(s.Sources, next.Sources)
}

func (s exactHashSnapshot) inputIdentities() string {
	identities := make([]string, 0, len(s.Sources))
	for _, source := range s.Sources {
		identities = append(identities, fmt.Sprintf("%s/%s[%s]@%s", source.Kind, source.Name,
			source.KeyOrRole, source.ResourceVersion))
	}
	return strings.Join(identities, ", ")
}

func (h *harness) readExactHashSnapshot(ctx context.Context, component Component) (exactHashSnapshot, error) {
	snapshot := exactHashSnapshot{Component: component.Name}
	if err := component.HashInputs.validate(); err != nil {
		return snapshot, fmt.Errorf("component %s exact hash inputs: %w", component.Name, err)
	}
	deployment, err := h.clients.kube.AppsV1().Deployments(h.config.Namespace).Get(ctx,
		component.Deployment, metav1.GetOptions{})
	if err != nil {
		return snapshot, fmt.Errorf("read %s Deployment for exact hash: %w", component.Name, err)
	}
	snapshot.DeploymentUID, snapshot.DeploymentGeneration = deployment.UID, deployment.Generation
	snapshot.Actual = deployment.Spec.Template.Annotations[hashAnnotation]
	if snapshot.Actual == "" {
		return snapshot, fmt.Errorf("component %s Deployment has no config hash", component.Name)
	}
	values := make([]string, 0, len(component.HashInputs.ConfigMaps))
	for _, entry := range component.HashInputs.ConfigMaps {
		cm, readErr := h.clients.kube.CoreV1().ConfigMaps(h.config.Namespace).Get(ctx, entry.Name,
			metav1.GetOptions{})
		if readErr != nil {
			return snapshot, fmt.Errorf("read %s ConfigMap %s for exact hash: %w", component.Name, entry.Name, readErr)
		}
		value, present := cm.Data[entry.Key]
		if !present {
			return snapshot, fmt.Errorf("component %s ConfigMap %s lacks hash input key %s",
				component.Name, entry.Name, entry.Key)
		}
		values = append(values, value)
		snapshot.Sources = append(snapshot.Sources, hashSourceVersion{Kind: "ConfigMap", Name: entry.Name,
			KeyOrRole: entry.Key, UID: cm.UID, ResourceVersion: cm.ResourceVersion})
	}
	secrets := make([]oracleSecret, 0, len(component.HashInputs.Secrets))
	for _, entry := range component.HashInputs.Secrets {
		secret, readErr := h.clients.metadata.Resource(secretResource).Namespace(h.config.Namespace).Get(ctx,
			entry.Name, metav1.GetOptions{})
		if readErr != nil {
			return snapshot, fmt.Errorf("read %s Secret metadata %s for exact hash: %w",
				component.Name, entry.Name, readErr)
		}
		secrets = append(secrets, oracleSecret{role: entry.Role, present: true,
			resourceVersion: secret.ResourceVersion})
		snapshot.Sources = append(snapshot.Sources, hashSourceVersion{Kind: "Secret", Name: entry.Name,
			KeyOrRole: entry.Role, UID: secret.UID, ResourceVersion: secret.ResourceVersion})
	}
	snapshot.Expected = configHashV1(values, secrets)
	return snapshot, nil
}

// verifyExactTargetHashes compares two complete passes across the target
// components. A changing source is retried within one stage-wide deadline; a
// stable mismatch is a real failure rather than a mere hash-change assertion.
func (h *harness) verifyExactTargetHashes(ctx context.Context, components []Component) (
	map[string]exactHashSnapshot, error,
) {
	ctx, cancel := context.WithTimeout(ctx, h.config.Timeout)
	defer cancel()
	var result map[string]exactHashSnapshot
	err := h.poll(ctx, "exact target config hashes", func(ctx context.Context) (bool, error) {
		first, err := h.readAllExactHashSnapshots(ctx, components)
		if err != nil {
			return false, err
		}
		second, err := h.readAllExactHashSnapshots(ctx, components)
		if err != nil {
			return false, err
		}
		for _, component := range components {
			if !first[component.Name].stable(second[component.Name]) {
				return false, nil
			}
		}
		for _, component := range components {
			snapshot := second[component.Name]
			if snapshot.Expected != snapshot.Actual {
				return false, fmt.Errorf("component %s config hash differs from applied inputs %s: expected %s, got %s",
					component.Name, snapshot.inputIdentities(), snapshot.Expected, snapshot.Actual)
			}
		}
		result = second
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (h *harness) readAllExactHashSnapshots(ctx context.Context, components []Component) (
	map[string]exactHashSnapshot, error,
) {
	snapshots := make(map[string]exactHashSnapshot, len(components))
	for _, component := range components {
		snapshot, err := h.readExactHashSnapshot(ctx, component)
		if err != nil {
			return nil, err
		}
		snapshots[component.Name] = snapshot
	}
	return snapshots, nil
}
