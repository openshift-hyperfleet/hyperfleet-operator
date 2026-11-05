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

func TestRegistrationPackaging(t *testing.T) {
	for _, overlay := range []string{"config/default", "config/manifests/bases", "config/manifests/prod"} {
		t.Run(overlay, func(t *testing.T) {
			cmd := exec.Command("go", "tool", "-modfile=tools/go.mod", "kustomize", "build", overlay)
			cmd.Dir = "../.."
			data, err := cmd.Output()
			if err != nil {
				t.Fatalf("render %s: %v", overlay, err)
			}
			olm := overlay != "config/default"
			var crdFound, adminRoleFound, requiredFound bool
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
					requiredFound = checkCSV(t, obj)
				}
			}
			if crdFound != !olm || adminRoleFound != !olm || requiredFound != olm {
				t.Errorf("unexpected packaging: crd=%t adminRole=%t required=%t olm=%t",
					crdFound, adminRoleFound, requiredFound, olm)
			}
		})
	}
}

func checkCSV(t *testing.T, obj unstructured.Unstructured) bool {
	t.Helper()
	var requiredFound bool
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
			if description["kind"] != "ManagementClusterRegistration" || description["version"] != "v1alpha1" {
				t.Errorf("unexpected required registration API: %v", description)
			}
			requiredFound = category == "required"
		}
	}
	return requiredFound
}
