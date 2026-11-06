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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestRegistrationPatterns uses the generated schema, not a second copy of its regexes.
func TestRegistrationPatterns(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot, "config/crd/bases",
		"hyperfleet.redhat.com_managementclusterregistrations.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	properties := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	type patternCase struct {
		Pattern string `json:"pattern"`
		Value   string `json:"value"`
		Want    bool   `json:"want"`
	}
	var cases []patternCase
	for field, values := range map[string][]string{
		"issuerURL": {"https://issuer.example.com", "https://issuer.example.com/oidc/",
			"https://issuer.internal:6443/oidc/"},
		"audience": {"hyperfleet-desire-store", "https://desire-store.internal"},
	} {
		pattern := properties[field].Pattern
		if pattern == "" || strings.Contains(pattern, "[:") {
			t.Fatalf("%s must have a portable pattern, got %q", field, pattern)
		}
		for _, value := range values {
			cases = append(cases, patternCase{pattern, value, true})
		}
		prefix := "desire"
		if field == "issuerURL" {
			prefix = "https://issuer.example.com/"
			for _, value := range []string{"http://issuer.example.com", "https://user@issuer.example.com",
				"https://issuer.example.com?query", "https://issuer.example.com/#fragment", "https:///path"} {
				cases = append(cases, patternCase{pattern, value, false})
			}
		}
		for char := rune(0); char <= 127; char++ {
			if char <= 32 || char == 127 {
				cases = append(cases, patternCase{pattern, prefix + string(char) + "store", false})
			}
		}
	}
	t.Run("RE2", func(t *testing.T) {
		for _, tc := range cases {
			if got := regexp.MustCompile(tc.Pattern).MatchString(tc.Value); got != tc.Want {
				t.Errorf("pattern %q value %q: got %t, want %t", tc.Pattern, tc.Value, got, tc.Want)
			}
		}
	})
	t.Run("ECMA-262", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("Node.js is required for the optional ECMA-262 cross-dialect check")
		}
		payload, err := json.Marshal(cases)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(node, "-e", `
const cases = JSON.parse(require('fs').readFileSync(0, 'utf8'));
for (const tc of cases) {
  if (new RegExp(tc.pattern).test(tc.value) !== tc.want) {
    throw new Error(JSON.stringify(tc));
  }
}
`)
		cmd.Stdin = bytes.NewReader(payload)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ECMA-262 validation: %v\n%s", err, output)
		}
	})
}
