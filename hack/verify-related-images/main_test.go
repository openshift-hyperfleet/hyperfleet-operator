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
		}, "expected"},
		{"tagged bundle image", func(s string) string {
			return strings.ReplaceAll(s, apiImage, "registry.example.com/api:latest")
		}, "mutable or malformed"},
		{"URL-style bundle image", func(s string) string {
			return strings.ReplaceAll(s, apiImage, "https://registry.example.com/api@sha256:"+strings.Repeat("4", 64))
		}, "mutable or malformed"},
		{"duplicate", func(s string) string {
			return s + "  - name: hyperfleet-api\n    image: " + apiImage + "\n"
		}, "duplicate name"},
		{"missing API everywhere", func(s string) string {
			env := "                env:\n                - name: RELATED_IMAGE_HYPERFLEET_API\n" +
				"                  value: " + apiImage + "\n"
			s = strings.Replace(s, env, "", 1)
			return strings.Replace(s, "  - name: hyperfleet-api\n    image: "+apiImage+"\n", "", 1)
		}, "missing required runtime override"},
		{"missing manager", func(s string) string {
			return strings.Replace(s, "name: manager", "name: other", 1)
		}, "expected exactly one manager"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyCSV([]byte(tt.mutate(validCSV())))
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
