/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package registrationapi

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	registrationCRDName = "managementclusterregistrations.hyperfleet.redhat.com"
	defaultOverlay      = "config/default"
	repositoryRoot      = "../.."
)

// TestRegistrationPackaging ships the owned API and narrow role without creating trust records.
func TestRegistrationPackaging(t *testing.T) {
	for _, overlay := range []string{defaultOverlay, "config/samples", "config/manifests/bases", "config/manifests/prod"} {
		t.Run(overlay, func(t *testing.T) {
			cmd := exec.Command("go", "tool", "-modfile=tools/go.mod", "kustomize", "build", overlay)
			cmd.Dir = repositoryRoot
			data, err := cmd.Output()
			if err != nil {
				t.Fatalf("render %s: %v", overlay, err)
			}
			includeAPI := overlay != "config/samples"
			includeCSV := overlay == "config/manifests/bases" || overlay == "config/manifests/prod"
			var crdFound, adminRoleFound, csvFound bool
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
			for {
				var obj unstructured.Unstructured
				if err := decoder.Decode(&obj); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch obj.GetKind() {
				case "CustomResourceDefinition":
					crdFound = crdFound || obj.GetName() == registrationCRDName
				case "ClusterRole":
					adminRoleFound = adminRoleFound || obj.GetName() == "hyperfleet-operator-managementclusterregistration-admin-role"
				case "ManagementClusterRegistration":
					t.Error("installation must not create example registrations")
				case "ClusterRoleBinding", "RoleBinding":
					role, _, err := unstructured.NestedString(obj.Object, "roleRef", "name")
					if err != nil {
						t.Fatal(err)
					}
					if role == "hyperfleet-operator-managementclusterregistration-admin-role" {
						t.Error("installation must not automatically bind the registration administrator role")
					}
				case "ClusterServiceVersion":
					csvFound = true
					checkCSV(t, obj)
				}
			}
			if csvFound != includeCSV {
				t.Errorf("unexpected CSV presence: got %t, want %t", csvFound, includeCSV)
			}
			if crdFound != includeAPI || adminRoleFound != includeAPI {
				t.Errorf("unexpected packaging: crd=%t adminRole=%t includeAPI=%t",
					crdFound, adminRoleFound, includeAPI)
			}
		})
	}
}

// checkCSV requires exactly one owned registration API, never an external provider dependency.
func checkCSV(t *testing.T, obj unstructured.Unstructured) {
	t.Helper()
	owned := 0
	for _, category := range []string{"owned", "required"} {
		entries, _, err := unstructured.NestedSlice(obj.Object, "spec", "customresourcedefinitions", category)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			description := entry.(map[string]any)
			if description["name"] != registrationCRDName {
				continue
			}
			if category == "owned" {
				owned++
				if description["kind"] != "ManagementClusterRegistration" || description["version"] != "v1alpha1" {
					t.Error("owned registration API must match the bundled CRD kind/version")
				}
			}
			if category == "required" {
				t.Error("registration must be owned, not an external API dependency")
			}
		}
	}
	if owned != 1 {
		t.Errorf("expected one owned registration API, got %d", owned)
	}
}
