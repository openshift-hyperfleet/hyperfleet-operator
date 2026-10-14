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
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
)

const (
	testTLSCASecretKey         = "ca.crt"
	testAuthorizationSecretKey = "token"
	testServerName             = "metrics.example"
)

// TestBuildServiceMonitor verifies buildServiceMonitor renders the expected
// name, namespace, GVK, metrics Service selector, owner reference and single
// scrape endpoint.
func TestBuildServiceMonitor(t *testing.T) {
	owner := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: metricsServiceName, UID: types.UID("test-uid")},
	}
	sm := buildServiceMonitor("hyperfleet-system", owner, ScrapeConfig{})

	if got := sm.GetName(); got != serviceMonitorName {
		t.Errorf("name = %q, want %q", got, serviceMonitorName)
	}
	if got := sm.GetNamespace(); got != "hyperfleet-system" {
		t.Errorf("namespace = %q, want %q", got, "hyperfleet-system")
	}
	if gvk := sm.GroupVersionKind(); gvk.Group != smGroup || gvk.Version != smVersion || gvk.Kind != smKind {
		t.Errorf("gvk = %v, want %s/%s %s", gvk, smGroup, smVersion, smKind)
	}

	// The ServiceMonitor must be owned by the metrics Service, so it is
	// garbage-collected when the operator's install (and that Service) is removed.
	owners := sm.GetOwnerReferences()
	if len(owners) != 1 {
		t.Fatalf("len(ownerReferences) = %d, want 1", len(owners))
	}
	if owners[0].Kind != "Service" || owners[0].Name != owner.Name || owners[0].UID != owner.UID {
		t.Errorf("ownerReferences[0] = %+v, want Kind=Service Name=%s UID=%s", owners[0], owner.Name, owner.UID)
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
	if _, found := ep["tlsConfig"]; found {
		t.Errorf("plain HTTP endpoint has tlsConfig: %v", ep)
	}
	if _, found := ep["authorization"]; found {
		t.Errorf("plain HTTP endpoint has authorization: %v", ep)
	}
}

func TestBuildServiceMonitorSecure(t *testing.T) {
	owner := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: metricsServiceName, UID: types.UID("test-uid")},
	}
	config := ScrapeConfig{
		Secure:                       true,
		ServingCertificateConfigured: true,
		TLSCASecretName:              "metrics-server-ca",
		TLSCASecretKey:               testTLSCASecretKey,
		ServerName:                   "metrics.hyperfleet-system.svc",
		AuthorizationSecretName:      "metrics-scrape-credentials",
		AuthorizationSecretKey:       testAuthorizationSecretKey,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	sm := buildServiceMonitor("hyperfleet-system", owner, config)
	endpoints, found, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	if err != nil || !found || len(endpoints) != 1 {
		t.Fatalf("spec.endpoints = %v, found=%v, err=%v", endpoints, found, err)
	}
	ep, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("endpoints[0] type = %T, want map[string]any", endpoints[0])
	}
	if got := ep["scheme"]; got != "https" {
		t.Errorf("endpoint scheme = %v, want https", got)
	}
	tlsConfig, ok := ep["tlsConfig"].(map[string]any)
	if !ok {
		t.Fatalf("tlsConfig = %T, want map[string]any", ep["tlsConfig"])
	}
	if got := tlsConfig["serverName"]; got != config.ServerName {
		t.Errorf("tlsConfig.serverName = %v, want %q", got, config.ServerName)
	}
	if _, found := tlsConfig["insecureSkipVerify"]; found {
		t.Errorf("tlsConfig disables TLS verification: %v", tlsConfig)
	}
	ca, ok := tlsConfig["ca"].(map[string]any)
	if !ok {
		t.Fatalf("tlsConfig.ca = %T, want map[string]any", tlsConfig["ca"])
	}
	caSecret, ok := ca["secret"].(map[string]any)
	if !ok || caSecret["name"] != config.TLSCASecretName || caSecret["key"] != config.TLSCASecretKey {
		t.Errorf("tlsConfig.ca.secret = %v, want name=%q key=%q", ca["secret"], config.TLSCASecretName, config.TLSCASecretKey)
	}

	authorization, ok := ep["authorization"].(map[string]any)
	if !ok {
		t.Fatalf("authorization = %T, want map[string]any", ep["authorization"])
	}
	if got := authorization["type"]; got != "Bearer" {
		t.Errorf("authorization.type = %v, want Bearer", got)
	}
	credentials, ok := authorization["credentials"].(map[string]any)
	if !ok || credentials["name"] != config.AuthorizationSecretName || credentials["key"] != config.AuthorizationSecretKey {
		t.Errorf("authorization.credentials = %v, want name=%q key=%q", authorization["credentials"], config.AuthorizationSecretName, config.AuthorizationSecretKey)
	}
	if _, found := ep["bearerTokenFile"]; found {
		t.Errorf("endpoint uses bearerTokenFile: %v", ep)
	}
}

// Render the real manifests with the repository's pinned Kustomize, rather
// than duplicating its replacement and namespace transformation rules in Go.
func TestSecureMetricsManifests(t *testing.T) {
	const (
		noCustomization = "none"
		serviceKind     = "Service"
		secureScheme    = "https"
	)
	for _, customization := range []string{noCustomization, "default", "secure-metrics"} {
		t.Run(customization, func(t *testing.T) {
			configDir := filepath.Join(t.TempDir(), "config")
			if err := os.CopyFS(configDir, os.DirFS("../../config")); err != nil {
				t.Fatal(err)
			}
			if customization != noCustomization {
				path := filepath.Join(configDir, customization, "kustomization.yaml")
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if customization == "default" {
					content = []byte(strings.ReplaceAll(string(content), "namespace: hyperfleet-system", "namespace: custom-system"))
					content = []byte(strings.ReplaceAll(string(content), "namePrefix: hyperfleet-operator-", "namePrefix: custom-"))
				} else {
					content = append(content, []byte("\nnamespace: custom-system\nnamePrefix: custom-\n")...)
				}
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command("go", "tool", "-modfile=../../tools/go.mod", "kustomize", "build",
				filepath.Join(configDir, "secure-metrics"))
			var buildErrors bytes.Buffer
			cmd.Stderr = &buildErrors
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("kustomize build: %v\n%s", err, buildErrors.String())
			}
			var service, monitor unstructured.Unstructured
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			for {
				var resource unstructured.Unstructured
				if err := decoder.Decode(&resource.Object); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch resource.GetKind() {
				case serviceKind:
					service = resource
				case "ServiceMonitor":
					monitor = resource
				}
			}
			if service.GetName() == "" || monitor.GetName() == "" {
				t.Fatal("rendered metrics Service or ServiceMonitor is missing")
			}
			wantNamespace := "hyperfleet-system"
			if customization != noCustomization {
				wantNamespace = "custom-system"
			}
			if service.GetNamespace() != wantNamespace || monitor.GetNamespace() != wantNamespace {
				t.Errorf("Service namespace = %q, ServiceMonitor namespace = %q; want both %q",
					service.GetNamespace(), monitor.GetNamespace(), wantNamespace)
			}
			endpoints, found, err := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
			if err != nil || !found || len(endpoints) != 1 {
				t.Fatalf("endpoints = %v, found=%t, err=%v", endpoints, found, err)
			}
			endpoint := endpoints[0].(map[string]any)
			wantServerName := service.GetName() + "." + service.GetNamespace() + ".svc"
			serverName, _, err := unstructured.NestedString(endpoint, "tlsConfig", "serverName")
			if err != nil || serverName != wantServerName {
				t.Errorf("serverName = %q, err=%v; want %q", serverName, err, wantServerName)
			}
			if endpoint["scheme"] != secureScheme {
				t.Errorf("scheme = %v, want https", endpoint["scheme"])
			}
			selector, _, err := unstructured.NestedStringMap(monitor.Object, "spec", "selector", "matchLabels")
			if err != nil || len(selector) == 0 {
				t.Fatalf("ServiceMonitor selector = %v, err=%v", selector, err)
			}
			for key, value := range selector {
				if service.GetLabels()[key] != value {
					t.Errorf("Service label %q = %q, want %q", key, service.GetLabels()[key], value)
				}
			}
		})
	}
}

func TestScrapeConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		config ScrapeConfig
		wantOK bool
	}{
		{name: "plain HTTP", config: ScrapeConfig{}, wantOK: true},
		{name: "secure without trust and auth", config: ScrapeConfig{Secure: true}, wantOK: false},
		{
			name: "secure with complete configuration",
			config: ScrapeConfig{
				Secure:                       true,
				ServingCertificateConfigured: true,
				TLSCASecretName:              "ca",
				TLSCASecretKey:               testTLSCASecretKey,
				ServerName:                   testServerName,
				AuthorizationSecretName:      "auth",
				AuthorizationSecretKey:       testAuthorizationSecretKey,
			},
			wantOK: true,
		},
		{
			name: "secure without mounted serving certificate",
			config: ScrapeConfig{
				Secure:                  true,
				TLSCASecretName:         "ca",
				TLSCASecretKey:          testTLSCASecretKey,
				ServerName:              testServerName,
				AuthorizationSecretName: "auth",
				AuthorizationSecretKey:  testAuthorizationSecretKey,
			},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.Validate()
			if (err == nil) != tc.wantOK {
				t.Errorf("Validate() error = %v, want success=%t", err, tc.wantOK)
			}
		})
	}
}

func TestBootstrapperSkipsUnavailableAPI(t *testing.T) {
	secureConfig := ScrapeConfig{
		Secure:                       true,
		ServingCertificateConfigured: true,
		TLSCASecretName:              "metrics-ca",
		TLSCASecretKey:               testTLSCASecretKey,
		ServerName:                   testServerName,
		AuthorizationSecretName:      "metrics-scrape-credentials",
		AuthorizationSecretKey:       testAuthorizationSecretKey,
	}

	b := &Bootstrapper{
		ScrapeConfig: secureConfig,
		serviceMonitorAvailability: func(*rest.Config) (bool, error) {
			return false, nil
		},
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v, want nil when the ServiceMonitor API is absent", err)
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
