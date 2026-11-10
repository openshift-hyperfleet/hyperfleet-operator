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
		kubeconfigEnv: filepath.Join(dir, "explicit.kubeconfig"), artifactDirEnv: filepath.Join(dir, "artifacts"),
		catalogEnv: testImage("catalog", "e"), fromBundleEnv: testImage("bundle", "a"),
		toBundleEnv: testImage("bundle", "b"), channelEnv: testChannel,
		scenarioEnv: filepath.Join(dir, "scenario.yaml"), timeoutEnv: "",
		namespaceEnv: "", "OPM": "", retainEnv: "",
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
		{"system namespace", namespaceEnv, systemNamespace},
		{"invalid deadline", timeoutEnv, "0s"},
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

func TestScenarioRejectsInvalidRuntimeContracts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Scenario)
	}{
		{"missing availability bound", func(s *Scenario) { s.MaxUnavailable = "" }},
		{"negative availability bound", func(s *Scenario) { s.MaxUnavailable = "-1s" }},
		{"invalid availability bound", func(s *Scenario) { s.MaxUnavailable = "not-a-duration" }},
		{"short stability window", func(s *Scenario) { s.SettleDuration = "4s" }},
		{"invalid stability window", func(s *Scenario) { s.SettleDuration = "not-a-duration" }},
		{"zero port", func(s *Scenario) { s.DataProbe.Port = 0 }},
		{"negative port", func(s *Scenario) { s.DataProbe.Port = -1 }},
		{"high port", func(s *Scenario) { s.DataProbe.Port = 65536 }},
		{"non API collection", func(s *Scenario) { s.DataProbe.Collection = "/channels" }},
		{"query in collection", func(s *Scenario) { s.DataProbe.Collection = "/api/channels?x=y" }},
		{"fragment in collection", func(s *Scenario) { s.DataProbe.Collection = "/api/channels#x" }},
		{"traversal in collection", func(s *Scenario) { s.DataProbe.Collection = "/api/../channels" }},
		{"CA without server", func(s *Scenario) { s.DataProbe.CAFile = "ca.pem" }},
		{"server without CA", func(s *Scenario) { s.DataProbe.ServerName = "api.example.test" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := exampleConfig(t)
			tc.mutate(&c.Scenario)
			if c.Scenario.validate() == nil {
				t.Fatal("invalid scenario accepted")
			}
		})
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
