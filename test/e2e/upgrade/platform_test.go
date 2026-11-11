// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

const (
	testAMD64Platform    = "linux/amd64"
	testARM64Platform    = "linux/arm64"
	testInvalidImageID   = "full image ID"
	testPlatformMismatch = "does not supply linux/arm64"
)

func TestInspectPlatformRejectsFailures(t *testing.T) {
	for _, tc := range []struct {
		name, pullOutput, inspectOutput, wantError string
		pullError, inspectError                    error
	}{
		{name: "pull fails", pullError: errors.New("registry unavailable"), wantError: "pull image"},
		{name: "empty ID", wantError: testInvalidImageID},
		{name: "short ID", pullOutput: "abc123", wantError: testInvalidImageID},
		{name: "invalid ID", pullOutput: strings.Repeat("z", 64), wantError: testInvalidImageID},
		{name: "multiple IDs", pullOutput: strings.Repeat("a", 64) + "\n" + strings.Repeat("b", 64),
			wantError: testInvalidImageID},
		{name: "inspect fails", pullOutput: strings.Repeat("a", 64), inspectError: errors.New("image missing"),
			wantError: "inspect image"},
		{name: "wrong architecture", pullOutput: strings.Repeat("a", 64), inspectOutput: testAMD64Platform,
			wantError: testPlatformMismatch},
		{name: "wrong OS", pullOutput: strings.Repeat("a", 64), inspectOutput: "windows/arm64",
			wantError: testPlatformMismatch},
		{name: "empty inspect", pullOutput: strings.Repeat("a", 64), wantError: testPlatformMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := inspectPlatform(t.Context(), "podman", testImage("api", "a"), platform{linuxOS, "arm64"},
				func(_ context.Context, _ string, args ...string) ([]byte, error) {
					calls++
					if args[0] == podmanPull {
						return []byte(tc.pullOutput), tc.pullError
					}
					return []byte(tc.inspectOutput), tc.inspectError
				})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got %v, want %s", err, tc.wantError)
			}
			if (tc.pullError != nil || tc.wantError == testInvalidImageID) && calls != 1 {
				t.Fatal("inspection continued after failed or malformed pull")
			}
		})
	}
}

func TestPlatformPreflightInspectsPulledIDsForEveryNodePlatform(t *testing.T) {
	h := fakeHarness(t)
	h.config.Podman = "/tools/podman"
	h.config.Catalog = testImage("catalog", "a")
	// Duplicate references and node architectures must not cause extra pulls.
	h.pair.From.Images = map[string]string{"operator": h.config.Catalog}
	for _, name := range []string{"amd64", "arm64", "arm64-second"} {
		arch := strings.Split(name, "-")[0]
		_, err := h.clients.kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: linuxOS, Architecture: arch}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]string{
		testAMD64Platform: strings.Repeat("a", 64), testARM64Platform: "sha256:" + strings.Repeat("b", 64),
	}
	var pulled, inspected []string
	err := h.validatePlatforms(t.Context(), nil, func(_ context.Context, tool string, args ...string) ([]byte, error) {
		if tool != h.config.Podman {
			t.Fatalf("ignored Podman executable override: %s", tool)
		}
		switch args[0] {
		case "--version":
			return []byte("podman version 6.0.2\n"), nil
		case podmanPull:
			if len(args) != 6 || !slices.Equal(args[:4], []string{podmanPull, "--quiet", "--policy=always", "--platform"}) ||
				args[5] != h.config.Catalog || ids[args[4]] == "" {
				t.Fatalf("pull must select the target platform and check the registry: %v", args)
			}
			pulled = append(pulled, args[4])
			return []byte(ids[args[4]] + "\n"), nil
		case "image":
			if len(pulled) == 0 || !slices.Equal(args, []string{
				"image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", ids[pulled[len(pulled)-1]],
			}) {
				t.Fatalf("must inspect the returned ID, not the multi-platform registry reference: %v", args)
			}
			target := pulled[len(pulled)-1]
			inspected = append(inspected, target)
			return []byte(target + "\n"), nil
		default:
			t.Fatalf("unexpected command: %v", args)
			return nil, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(inspected)
	if len(pulled) != 2 || !slices.Equal(inspected, []string{testAMD64Platform, testARM64Platform}) {
		t.Fatalf("platform coverage or deduplication lost: pulled %v, inspected %v", pulled, inspected)
	}
	data, err := os.ReadFile(filepath.Join(h.config.ArtifactDir, "platforms.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		PodmanVersion string `json:"podmanVersion"`
	}
	if json.Unmarshal(data, &evidence) != nil || evidence.PodmanVersion != "podman version 6.0.2" {
		t.Fatal("missing Podman version evidence")
	}
}

func TestPlatformPreflightRequiresPodman(t *testing.T) {
	h := fakeHarness(t)
	err := h.validatePlatforms(t.Context(), nil, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if !slices.Equal(args, []string{"--version"}) {
			t.Fatalf("continued after missing tool: %v", args)
		}
		return nil, os.ErrNotExist
	})
	if err == nil || !strings.Contains(err.Error(), "podman is required") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing prerequisite was not reported: %v", err)
	}
	if len(h.clients.kube.(*kubefake.Clientset).Actions()) != 0 {
		t.Fatal("cluster was accessed before checking the tool")
	}
}
