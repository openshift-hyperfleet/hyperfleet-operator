// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestArchitectureMismatchFailsBeforeInstalling(t *testing.T) {
	h := fakeHarness(t)
	h.config.Catalog = testImage("catalog", "a")
	_, err := h.clients.kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "arm64"}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = h.validatePlatforms(t.Context(), nil, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if strings.Join(args[:4], " ") != "--override-os linux --override-arch arm64" {
			t.Fatalf("wrong target platform: %v", args)
		}
		return []byte(`{"os":"linux","architecture":"amd64"}`), nil
	})
	if err == nil || !strings.Contains(err.Error(), "does not supply linux/arm64") {
		t.Fatalf("architecture mismatch accepted: %v", err)
	}
}
