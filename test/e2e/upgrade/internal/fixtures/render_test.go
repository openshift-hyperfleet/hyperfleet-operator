// Copyright 2026. Licensed under the Apache License, Version 2.0.

package fixtures

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	scenario = "fixtures: 'fixtures.yaml'\n"
	template = `postgres: ${UPGRADE_POSTGRES_IMAGE}
rabbit: ${UPGRADE_RABBITMQ_IMAGE}
databasePassword: ${UPGRADE_DB_PASSWORD}
brokerPassword: ${UPGRADE_BROKER_PASSWORD}
brokerURL: amqp://hyperfleet:${UPGRADE_BROKER_PASSWORD}@upgrade-rabbitmq:5672/
`
	repository = "quay.io/hyperfleet/test@sha256:"
)

func prepare(t *testing.T, scenarioYAML, fixtureTemplate string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{"scenario.yaml": scenarioYAML, "fixtures.template.yaml": fixtureTemplate} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRender(t *testing.T) {
	postgres, rabbit := repository+strings.Repeat("a", 64), repository+strings.Repeat("b", 64)
	passwords := map[string]bool{}
	for range 2 {
		dir := prepare(t, scenario, template)
		if err := Render(dir, postgres, rabbit); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, outputName)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("expected a private fixture file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatal("rendered YAML must contain string values")
		}
		if got["postgres"] != postgres || got["rabbit"] != rabbit {
			t.Fatal("fixture image differs from its input")
		}
		for _, key := range []string{"databasePassword", "brokerPassword"} {
			password := got[key]
			if password == "" || passwords[password] {
				t.Fatal("expected a fresh password for each credential and run")
			}
			passwords[password] = true
		}
		brokerURL, err := url.Parse(got["brokerURL"])
		if err != nil || brokerURL.User == nil {
			t.Fatal("expected a broker URL with credentials")
		}
		if password, ok := brokerURL.User.Password(); !ok || password != got["brokerPassword"] {
			t.Fatal("broker URL and broker Secret must share a password")
		}
		if err := Render(dir, postgres, rabbit); !errors.Is(err, os.ErrExist) {
			t.Fatal("second render must refuse to overwrite the output")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(data) {
			t.Fatal("second render changed the existing output")
		}
	}
}

func TestRenderRejectsInvalidInputs(t *testing.T) {
	image := repository + strings.Repeat("a", 64)
	for _, tc := range []struct{ name, scenario, template, postgres, rabbit string }{
		{"unqualified scenario", scenario + "bundle: ${UPGRADE_TO_BUNDLE_DIGEST}", template, image, image},
		{"invalid scenario YAML", "fixtures: [", template, image, image},
		{"multiple scenarios", scenario + "---\n" + scenario, template, image, image},
		{"wrong fixture path", "fixtures: ../fixtures.yaml", template, image, image},
		{"missing variable", scenario, strings.ReplaceAll(template, "${UPGRADE_DB_PASSWORD}", ""), image, image},
		{"unexpected variable", scenario, template + "extra: ${HOME}", image, image},
		{"malformed variable", scenario, template + "extra: ${BROKEN", image, image},
		{"unbraced variable", scenario, template + "extra: $UPGRADE_OTHER", image, image},
		{"mutable postgres image", scenario, template, "quay.io/test/postgres:latest", image},
		{"missing rabbitmq image", scenario, template, image, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := prepare(t, tc.scenario, tc.template)
			if err := Render(dir, tc.postgres, tc.rabbit); err == nil {
				t.Fatal("expected invalid input to fail")
			}
			if _, err := os.Lstat(filepath.Join(dir, outputName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid input must leave no fixture output")
			}
		})
	}
}

func TestRenderRefusesSymlink(t *testing.T) {
	dir := prepare(t, scenario, template)
	target := filepath.Join(dir, "elsewhere.yaml")
	if err := os.Symlink(target, filepath.Join(dir, outputName)); err != nil {
		t.Fatal(err)
	}
	image := repository + strings.Repeat("a", 64)
	if err := Render(dir, image, image); !errors.Is(err, os.ErrExist) {
		t.Fatal("expected existing symlink to be rejected")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlink target was created")
	}
}
