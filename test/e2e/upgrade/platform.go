// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
		if p.OS != "linux" || p.Architecture == "" {
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
			data, err := run(ctx, h.config.Skopeo, "--override-os", want.OS, "--override-arch", want.Architecture,
				"inspect", "--config", "docker://"+image)
			if err != nil {
				return fmt.Errorf("inspect image %s for %s/%s: %w", image, want.OS, want.Architecture, err)
			}
			var actual platform
			if json.Unmarshal(data, &actual) != nil || actual != want {
				return fmt.Errorf("image %s does not supply %s/%s", image, want.OS, want.Architecture)
			}
		}
	}
	toolVersion, err := run(ctx, h.config.Skopeo, "--version")
	if err != nil {
		return err
	}
	return h.writeJSON("platforms.json", map[string]any{"platforms": platforms, "images": images,
		"skopeoVersion": strings.TrimSpace(string(toolVersion))})
}
