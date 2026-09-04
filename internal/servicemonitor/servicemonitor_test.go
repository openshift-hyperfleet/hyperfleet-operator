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

package servicemonitor

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestBuildServiceMonitor verifies buildServiceMonitor renders the expected
// name, namespace, GVK, metrics Service selector and single scrape endpoint.
func TestBuildServiceMonitor(t *testing.T) {
	sm := buildServiceMonitor("hyperfleet-system")

	if got := sm.GetName(); got != serviceMonitorName {
		t.Errorf("name = %q, want %q", got, serviceMonitorName)
	}
	if got := sm.GetNamespace(); got != "hyperfleet-system" {
		t.Errorf("namespace = %q, want %q", got, "hyperfleet-system")
	}
	if gvk := sm.GroupVersionKind(); gvk.Group != smGroup || gvk.Version != smVersion || gvk.Kind != smKind {
		t.Errorf("gvk = %v, want %s/%s %s", gvk, smGroup, smVersion, smKind)
	}

	// The ServiceMonitor selector must match the labels the operator's metrics
	// Service carries, or Prometheus discovers no target to scrape.
	sel, found, err := unstructured.NestedStringMap(sm.Object, "spec", "selector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("spec.selector.matchLabels missing: found=%v err=%v", found, err)
	}
	if sel["control-plane"] != controlPlane || sel["app.kubernetes.io/name"] != appName {
		t.Errorf("selector.matchLabels = %v", sel)
	}

	// Exactly one endpoint, scraping the plain-HTTP :9090 metrics port by name.
	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if err != nil || !found {
		t.Fatalf("spec.endpoints missing: found=%v err=%v", found, err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("len(spec.endpoints) = %d, want 1", len(endpoints))
	}
	ep, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("endpoints[0] type = %T, want map[string]any", endpoints[0])
	}
	if ep["port"] != "metrics" || ep["path"] != "/metrics" || ep["scheme"] != "http" {
		t.Errorf("endpoint = %v, want port=metrics path=/metrics scheme=http", ep)
	}
}

// TestHasServiceMonitorKind verifies hasServiceMonitorKind detects the
// ServiceMonitor kind in a discovery list and handles a nil/absent group.
func TestHasServiceMonitorKind(t *testing.T) {
	tests := []struct {
		name string
		list *metav1.APIResourceList
		want bool
	}{
		{
			name: "nil list (group absent)",
			list: nil,
			want: false,
		},
		{
			name: "group present without ServiceMonitor",
			list: &metav1.APIResourceList{APIResources: []metav1.APIResource{{Kind: "PrometheusRule"}}},
			want: false,
		},
		{
			name: "ServiceMonitor advertised",
			list: &metav1.APIResourceList{APIResources: []metav1.APIResource{
				{Kind: "PrometheusRule"},
				{Kind: smKind},
			}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasServiceMonitorKind(tc.list); got != tc.want {
				t.Errorf("hasServiceMonitorKind() = %v, want %v", got, tc.want)
			}
		})
	}
}
