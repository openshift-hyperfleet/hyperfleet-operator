// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	linuxOS    = "linux"
	podmanPull = "pull"
)

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

func clusterPlatforms(nodes []corev1.Node) ([]platform, error) {
	var result []platform
	for _, node := range nodes {
		if node.Spec.Unschedulable {
			continue
		}
		p := platform{OS: node.Status.NodeInfo.OperatingSystem, Architecture: node.Status.NodeInfo.Architecture}
		if p.OS != linuxOS || p.Architecture == "" {
			return nil, fmt.Errorf("target must have known Linux node architectures")
		}
		if !slices.Contains(result, p) {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("target has no schedulable nodes")
	}
	return result, nil
}

func (h *harness) validatePlatforms(ctx context.Context, fixtures []unstructured.Unstructured, run commandFunc) error {
	toolVersion, err := run(ctx, h.config.Podman, "--version")
	if err != nil {
		return fmt.Errorf("podman is required for image platform preflight: %w", err)
	}
	nodes, err := h.clients.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	platforms, err := clusterPlatforms(nodes.Items)
	if err != nil {
		return err
	}
	images := []string{h.config.Catalog}
	for _, release := range []release{h.pair.From, h.pair.To} {
		for _, image := range release.Images {
			images = append(images, image)
		}
	}
	for _, obj := range fixtures {
		if obj.GetKind() != "Deployment" && obj.GetKind() != "StatefulSet" {
			continue
		}
		spec, _, _ := unstructured.NestedMap(obj.Object, fieldSpec, "template", fieldSpec)
		pod, err := fromUnstructured[corev1.PodSpec](&unstructured.Unstructured{Object: spec})
		if err != nil {
			return err
		}
		for _, c := range podImages(pod) {
			images = append(images, c.Image)
		}
	}
	slices.Sort(images)
	images = slices.Compact(images)
	for _, image := range images {
		for _, want := range platforms {
			if err := inspectPlatform(ctx, h.config.Podman, image, want, run); err != nil {
				return err
			}
		}
	}
	return h.writeJSON("platforms.json", map[string]any{"platforms": platforms, "images": images,
		"podmanVersion": strings.TrimSpace(string(toolVersion))})
}

func inspectPlatform(ctx context.Context, podman, image string, want platform, run commandFunc) error {
	target := want.OS + "/" + want.Architecture
	data, err := run(ctx, podman, podmanPull, "--quiet", "--policy=always", "--platform", target, image)
	if err != nil {
		return fmt.Errorf("pull image %s for %s: %w", image, target, err)
	}
	// Inspect the returned ID: pulling another platform can change the local
	// image associated with the same registry reference. Never trust only the
	// requested platform; a single-image manifest can contain a different one.
	id := strings.TrimSpace(string(data))
	decoded, err := hex.DecodeString(strings.TrimPrefix(id, "sha256:"))
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("pull image %s for %s did not return a full image ID", image, target)
	}
	data, err = run(ctx, podman, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", id)
	if err != nil {
		return fmt.Errorf("inspect image %s for %s: %w", image, target, err)
	}
	if strings.TrimSpace(string(data)) != target {
		return fmt.Errorf("image %s does not supply %s", image, target)
	}
	return nil
}
