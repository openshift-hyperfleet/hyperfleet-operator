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

package main

import (
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

func validInventory() []imageEntry {
	return []imageEntry{
		{Name: "hyperfleet-operator", Image: operatorImage},
		{Name: "hyperfleet-api", Image: apiImage},
	}
}

func TestVerifyCSV(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{name: "valid", mutate: func(s string) string { return s }},
		{name: "missing inventory entry", mutate: func(s string) string { return s }, wantErr: "CSV image sources has undeclared entry \"hyperfleet-api\""},
		{name: "stale related image", mutate: func(s string) string {
			return strings.Replace(s, "  - name: hyperfleet-api\n    image: "+apiImage, "  - name: extra\n    image: "+extraImage, 1)
		}, wantErr: "CSV spec.relatedImages has undeclared entry \"extra\""},
		{name: "duplicate related image", mutate: func(s string) string {
			return s + "  - name: extra\n    image: " + apiImage + "\n"
		}, wantErr: "CSV spec.relatedImages has duplicate image"},
		{name: "mutable image", mutate: func(s string) string {
			return strings.ReplaceAll(s, apiImage, "registry.example.com/hyperfleet-api:latest")
		}, wantErr: "mutable or malformed"},
		{name: "mismatched related image", mutate: func(s string) string {
			return strings.Replace(s, "  - name: hyperfleet-api\n    image: "+apiImage, "  - name: hyperfleet-api\n    image: "+extraImage, 1)
		}, wantErr: "CSV spec.relatedImages entry \"hyperfleet-api\""},
		{name: "missing operand env", mutate: func(s string) string {
			env := "                env:\n                - name: RELATED_IMAGE_HYPERFLEET_API\n                  value: " + apiImage + "\n"
			return strings.Replace(s, env, "", 1)
		}, wantErr: "CSV image sources is missing deployable image inventory entry \"hyperfleet-api\""},
		{name: "undeclared CSV operand", mutate: func(s string) string {
			return strings.Replace(s, "RELATED_IMAGE_HYPERFLEET_API", "RELATED_IMAGE_EXTRA", 1)
		}, wantErr: "CSV image sources has undeclared entry \"extra\""},
		{name: "mutable manager image", mutate: func(s string) string {
			return strings.Replace(s, operatorImage, "registry.example.com/hyperfleet-operator:latest", 2)
		}, wantErr: "mutable or malformed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := validInventory()
			if tt.name == "missing inventory entry" {
				inventory = inventory[:1]
			}
			err := verifyCSV([]byte(tt.mutate(validCSV())), inventory)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyCSV() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verifyCSV() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyCSVRejectsDuplicateAndMalformedInventory(t *testing.T) {
	inventory := validInventory()
	inventory = append(inventory, imageEntry{Name: "hyperfleet-api", Image: extraImage})
	if err := verifyCSV([]byte(validCSV()), inventory); err == nil || !strings.Contains(err.Error(), "duplicate deployable image name") {
		t.Fatalf("verifyCSV() error = %v, want duplicate inventory name", err)
	}

	inventory = validInventory()
	inventory[1].Image = "registry.example.com/hyperfleet-api:latest"
	if err := verifyCSV([]byte(validCSV()), inventory); err == nil || !strings.Contains(err.Error(), "mutable or malformed inventory image") {
		t.Fatalf("verifyCSV() error = %v, want malformed inventory image", err)
	}
}
