// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func exampleConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	scenario, err := os.ReadFile("testdata/scenario.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := os.ReadFile("testdata/fixtures.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	scenario = []byte(strings.ReplaceAll(string(scenario), "${UPGRADE_API_HASH_CHANGE}", "false"))
	fixtures = []byte(strings.NewReplacer("${UPGRADE_POSTGRES_IMAGE}", testImage("postgres", "c"),
		"${UPGRADE_RABBITMQ_IMAGE}", testImage("rabbitmq", "d"), "${UPGRADE_DB_PASSWORD}", "unit-test-password",
		"${UPGRADE_BROKER_PASSWORD}", "unit-test-broker-password").Replace(string(fixtures)))
	for name, data := range map[string][]byte{"scenario.yaml": scenario, "fixtures.yaml": fixtures} {
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		kubeconfigEnv: filepath.Join(dir, "explicit.kubeconfig"), "ARTIFACT_DIR": filepath.Join(dir, "artifacts"),
		catalogEnv: testImage("catalog", "e"), fromBundleEnv: testImage("bundle", "a"),
		"UPGRADE_TO_BUNDLE": testImage("bundle", "b"), channelEnv: testChannel,
		"UPGRADE_SCENARIO": filepath.Join(dir, "scenario.yaml"), "UPGRADE_TIMEOUT": "",
		"UPGRADE_NAMESPACE": "", "OPM": "", "UPGRADE_RETAIN_ON_SUCCESS": "",
	} {
		t.Setenv(name, value)
	}
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExampleScenarioAndFixtures(t *testing.T) {
	c := exampleConfig(t)
	if _, err := loadFixtures(c.Scenario.Fixtures); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Scenario.Fixtures) {
		t.Fatal("fixture path not resolved independently of cleanup cwd")
	}
	if len(c.Scenario.To.Components) != 6 {
		t.Fatal("example inventory was lost")
	}
}

func TestConfigFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, env, value string }{
		{"ambient kubeconfig", kubeconfigEnv, ""},
		{"mutable catalog", catalogEnv, "quay.io/test/catalog:latest"},
		{"missing pair", fromBundleEnv, ""},
		{"missing channel", channelEnv, ""},
		{"system namespace", "UPGRADE_NAMESPACE", systemNamespace},
		{"invalid deadline", "UPGRADE_TIMEOUT", "0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exampleConfig(t)
			t.Setenv(tc.env, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

func TestScenarioRequiresExplicitHashContract(t *testing.T) {
	c := exampleConfig(t)
	c.Scenario.To.Components[0].HashChange = nil
	if c.Scenario.validate() == nil {
		t.Fatal("missing hash expectation was inferred")
	}
	c = exampleConfig(t)
	c.Scenario.To.Components = append(c.Scenario.To.Components, c.Scenario.To.Components[0])
	if c.Scenario.validate() == nil {
		t.Fatal("duplicate workload accepted")
	}
}

func TestProbeClientRejectsRedirects(t *testing.T) {
	var visited atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { visited.Store(true) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	client, err := probeHTTPClient(ProbeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(origin.URL)
	if response != nil {
		if closeErr := response.Body.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if err == nil || visited.Load() {
		t.Fatal("redirect could forward probe credentials")
	}
}
