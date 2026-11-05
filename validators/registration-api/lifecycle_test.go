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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// TestRemovalTargetsPreserveRegistration checks the actual kubectl deletion inputs without a cluster.
func TestRemovalTargetsPreserveRegistration(t *testing.T) {
	for _, tc := range []struct{ target, installOverlay string }{
		{"uninstall", "config/crd"},
		{"undeploy", defaultOverlay},
		{"undeploy", "config/overlays/googlepubsub"},
	} {
		t.Run(tc.target+"/"+tc.installOverlay, func(t *testing.T) {
			cmd := exec.Command("go", "tool", "-modfile=tools/go.mod", "kustomize", "build", tc.installOverlay)
			cmd.Dir = repositoryRoot
			installed, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			want := resourceIdentities(t, installed)
			registration := "apiextensions.k8s.io/v1/CustomResourceDefinition//" + registrationCRDName
			if !want[registration] {
				t.Fatal("installation must include the registration CRD")
			}
			delete(want, registration)

			dir := t.TempDir()
			capture := filepath.Join(dir, "deletion.yaml")
			t.Setenv("REGISTRATION_TEST_CAPTURE", capture)
			stub := filepath.Join(dir, "kubectl.sh")
			// Refuse file-based deletion so dist/install.yaml can never bypass the retention overlay.
			script := "set -eu\n[ \"$1\" = delete ]\n[ \"$3\" = -f ]\n[ \"$4\" = - ]\ncat > \"$REGISTRATION_TEST_CAPTURE\"\n"
			if err := os.WriteFile(stub, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			// Skip regeneration: this test verifies committed artifacts and never contacts Kubernetes.
			cmd = exec.Command("make", "-o", "manifests", tc.target, "KUBECTL=sh "+strconv.Quote(stub))
			cmd.Dir = repositoryRoot
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", tc.target, err, output)
			}
			deleted, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			got := resourceIdentities(t, deleted)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("deletion must contain exactly the installed resources except registration CRD:\ngot %v\nwant %v", got, want)
			}
		})
	}
}

// resourceIdentities compares deletion scope independently of broker-specific Deployment configuration.
func resourceIdentities(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	identities := make(map[string]bool)
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); errors.Is(err, io.EOF) {
			return identities
		} else if err != nil {
			t.Fatal(err)
		}
		if obj.Object != nil {
			identities[obj.GetAPIVersion()+"/"+obj.GetKind()+"/"+obj.GetNamespace()+"/"+obj.GetName()] = true
		}
	}
}
