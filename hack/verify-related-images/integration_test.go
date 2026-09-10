// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Exercise the bundle patching entrypoint with images that differ from the
// source CSV. This models the Konflux build boundary without registry access.
func TestBundleBuildImageUpdate(t *testing.T) {
	yq, err := exec.LookPath("yq")
	if err != nil {
		t.Skip("yq is required for the bundle build integration test")
	}

	t.Run("updates image references", func(t *testing.T) {
		csvPath := writeCSV(t, validCSV())
		output, err := runBundleUpdate(t, yq, csvPath, extraImage, operatorImage)
		if err != nil {
			t.Fatalf("%v: %s", err, output)
		}
		data, err := os.ReadFile(csvPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyCSV(data); err != nil {
			t.Fatal(err)
		}

		assertBundleImageReferences(t, data, extraImage, operatorImage)
		if strings.Contains(string(data), apiImage) {
			t.Fatal("old source digest survived bundle build")
		}
	})

	t.Run("rejects missing runtime override", func(t *testing.T) {
		missing := strings.Replace(validCSV(), "RELATED_IMAGE_HYPERFLEET_API", "UNRELATED_SETTING", 1)
		csvPath := writeCSV(t, missing)
		output, err := runBundleUpdate(t, yq, csvPath, extraImage, operatorImage)
		if err == nil {
			t.Fatalf("bundle build accepted missing API override: %s", output)
		}
		if got := string(output); !strings.Contains(got, "CSV is missing exactly one RELATED_IMAGE_HYPERFLEET_API runtime override") {
			t.Fatalf("missing API override error = %q", got)
		}
	})

	t.Run("rejects tagged manager image", func(t *testing.T) {
		csvPath := writeCSV(t, validCSV())
		rejectedPullspec := "registry.example.com/operator:latest"
		output, err := runBundleUpdate(t, yq, csvPath, rejectedPullspec, apiImage)
		if err == nil {
			t.Fatalf("bundle build accepted tagged manager image: %s", output)
		}
		if got := string(output); !strings.Contains(got, rejectedPullspec) {
			t.Fatalf("tagged image error = %q, want rejected pullspec %q", got, rejectedPullspec)
		}
	})
}

func assertBundleImageReferences(t *testing.T, data []byte, operatorPullspec, apiPullspec string) {
	t.Helper()
	var csv csvDocument
	if err := yaml.Unmarshal(data, &csv); err != nil {
		t.Fatalf("parse updated CSV: %v", err)
	}
	var metadata struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("parse updated CSV metadata: %v", err)
	}

	var managerImage, apiOverride string
	for _, deployment := range csv.Spec.Install.Spec.Deployments {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name != "manager" {
				continue
			}
			managerImage = container.Image
			for _, variable := range container.Env {
				if variable.Name == "RELATED_IMAGE_HYPERFLEET_API" {
					apiOverride = variable.Value
				}
			}
		}
	}
	var operatorRelatedImage, apiRelatedImage string
	for _, image := range csv.Spec.RelatedImages {
		switch image.Name {
		case "hyperfleet-operator":
			operatorRelatedImage = image.Image
		case "hyperfleet-api":
			apiRelatedImage = image.Image
		}
	}

	for _, check := range []struct {
		location string
		got      string
		want     string
	}{
		{"manager image", managerImage, operatorPullspec},
		{"metadata.annotations.containerImage", metadata.Metadata.Annotations["containerImage"], operatorPullspec},
		{"related image hyperfleet-operator", operatorRelatedImage, operatorPullspec},
		{"RELATED_IMAGE_HYPERFLEET_API", apiOverride, apiPullspec},
		{"related image hyperfleet-api", apiRelatedImage, apiPullspec},
	} {
		if check.got != check.want {
			t.Errorf("%s = %q, want requested pullspec %q", check.location, check.got, check.want)
		}
	}
}

func writeCSV(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.csv")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runBundleUpdate(t *testing.T, yq, csvPath, operatorPullspec, apiPullspec string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "../bundle/update_bundle.sh")
	cmd.Env = append(os.Environ(), "YQ="+yq, "CSV_FILE="+csvPath,
		"HYPERFLEET_OPERATOR_IMAGE_PULLSPEC="+operatorPullspec,
		"HYPERFLEET_API_IMAGE_PULLSPEC="+apiPullspec)
	return cmd.CombinedOutput()
}
