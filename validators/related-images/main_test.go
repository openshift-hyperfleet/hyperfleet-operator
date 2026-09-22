// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	operatorImage = "registry.example.com/hyperfleet-operator@sha256:" + strings.Repeat("1", 64)
	apiImage      = "registry.example.com/hyperfleet-api@sha256:" + strings.Repeat("2", 64)
	extraImage    = "registry.example.com/extra@sha256:" + strings.Repeat("3", 64)
)

func validCSV() string {
	return `apiVersion: operators.coreos.com/v1alpha1
kind: ClusterServiceVersion
spec:
  install:
    spec:
      deployments:
      - name: hyperfleet-operator-controller-manager
        spec:
          template:
            spec:
              containers:
              - name: manager
                image: ` + operatorImage + `
                env:
                - name: RELATED_IMAGE_HYPERFLEET_API
                  value: ` + apiImage + `
  relatedImages:
  - name: hyperfleet-operator
    image: ` + operatorImage + `
  - name: hyperfleet-api
    image: ` + apiImage + `
`
}

func TestVerifyBuiltBundleCSV(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"valid bundle", func(s string) string { return s }, ""},
		{"different bundle", func(s string) string { return strings.ReplaceAll(s, apiImage, extraImage) }, ""},
		{"stale related image", func(s string) string {
			return strings.Replace(s, "    image: "+apiImage, "    image: "+extraImage, 1)
		}, "not found in spec.relatedImages"},
		{"tagged bundle image", func(s string) string {
			return strings.ReplaceAll(s, apiImage, "registry.example.com/api:latest")
		}, "not a sha256 digest"},
		{"URL-style bundle image", func(s string) string {
			return strings.ReplaceAll(s, apiImage, "https://registry.example.com/api@sha256:"+strings.Repeat("4", 64))
		}, "not a sha256 digest"},
		{"duplicate relatedImage", func(s string) string {
			return s + "  - name: extra\n    image: " + apiImage + "\n"
		}, "duplicate value of image"},
		{"same env in two containers is valid", func(s string) string {
			sidecar := "\n              - name: sidecar\n" +
				"                image: " + operatorImage + "\n" +
				"                env:\n" +
				"                - name: RELATED_IMAGE_HYPERFLEET_API\n" +
				"                  value: " + apiImage
			return strings.Replace(s, "  relatedImages:", sidecar+"\n  relatedImages:", 1)
		}, ""},
		{"duplicate env with unknown value", func(s string) string {
			dup := "\n                - name: RELATED_IMAGE_HYPERFLEET_API\n" +
				"                  value: " + extraImage
			return strings.Replace(s, "  relatedImages:", dup+"\n  relatedImages:", 1)
		}, "duplicate env var"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateCSVData([]byte(tt.mutate(validCSV())))
			if tt.want == "" {
				if len(result.Errors) > 0 {
					t.Fatalf("expected no errors, got: %v", result.Errors)
				}
			} else {
				found := false
				for _, e := range result.Errors {
					if strings.Contains(e.Detail, tt.want) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected error containing %q, got: %v", tt.want, result.Errors)
				}
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		result := validate("/nonexistent/path")
		if len(result.Errors) != 1 {
			t.Fatalf("expected 1 error, got %d", len(result.Errors))
		}
		if !strings.Contains(result.Errors[0].Detail, "failed to find CSV") {
			t.Fatalf("unexpected error: %s", result.Errors[0].Detail)
		}
	})

	t.Run("no CSV in manifests", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o755); err != nil {
			t.Fatal(err)
		}
		result := validate(dir)
		if len(result.Errors) != 1 {
			t.Fatalf("expected 1 error, got %d", len(result.Errors))
		}
		if !strings.Contains(result.Errors[0].Detail, "no CSV found") {
			t.Fatalf("unexpected error: %s", result.Errors[0].Detail)
		}
	})

	t.Run("valid bundle directory", func(t *testing.T) {
		dir := t.TempDir()
		manifests := filepath.Join(dir, "manifests")
		if err := os.MkdirAll(manifests, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(manifests, "test.clusterserviceversion.yaml"),
			[]byte(validCSV()), 0o644,
		); err != nil {
			t.Fatal(err)
		}
		result := validate(dir)
		if len(result.Errors) > 0 {
			t.Fatalf("expected no errors, got: %v", result.Errors)
		}
	})
}

func TestFindCSV(t *testing.T) {
	t.Run("returns error for missing dir", func(t *testing.T) {
		got, err := findCSV("/nonexistent")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got != "" {
			t.Fatalf("expected empty path, got %q", got)
		}
	})

	t.Run("returns error when no CSV exists", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(dir, "manifests", "crd.yaml"), []byte("x"), 0o644,
		); err != nil {
			t.Fatal(err)
		}
		got, err := findCSV(dir)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no CSV found") {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Fatalf("expected empty path, got %q", got)
		}
	})

	t.Run("finds CSV file", func(t *testing.T) {
		dir := t.TempDir()
		manifests := filepath.Join(dir, "manifests")
		if err := os.MkdirAll(manifests, 0o755); err != nil {
			t.Fatal(err)
		}
		csvFile := filepath.Join(manifests, "my-operator.clusterserviceversion.yaml")
		if err := os.WriteFile(csvFile, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := findCSV(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != csvFile {
			t.Fatalf("expected %q, got %q", csvFile, got)
		}
	})
}
