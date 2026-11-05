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

const registrationCRDName = "managementclusterregistrations.hyperfleet.redhat.com"

// TestRegistrationPackaging keeps trust records opt-in and their API outside OLM ownership and resolution.
func TestRegistrationPackaging(t *testing.T) {
	for _, overlay := range []string{"config/default", "config/samples", "config/manifests/bases", "config/manifests/prod"} {
		t.Run(overlay, func(t *testing.T) {
			cmd := exec.Command("go", "tool", "-modfile=tools/go.mod", "kustomize", "build", overlay)
			cmd.Dir = "../.."
			data, err := cmd.Output()
			if err != nil {
				t.Fatalf("render %s: %v", overlay, err)
			}
			includeBootstrap := overlay == "config/default"
			var crdFound, adminRoleFound bool
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
				case "ClusterServiceVersion":
					checkCSV(t, obj)
				}
			}
			if crdFound != includeBootstrap || adminRoleFound != includeBootstrap {
				t.Errorf("unexpected packaging: crd=%t adminRole=%t includeBootstrap=%t",
					crdFound, adminRoleFound, includeBootstrap)
			}
		})
	}
}

// checkCSV rejects both implicit RBAC delegation and an unsatisfiable external API dependency.
func checkCSV(t *testing.T, obj unstructured.Unstructured) {
	t.Helper()
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
				t.Error("OLM must not own the registration CRD: ownership enables generic RBAC aggregation")
			}
			if category == "required" {
				t.Error("OLM must not require the registration CRD: external bootstrap is not a provider bundle")
			}
		}
	}
}
