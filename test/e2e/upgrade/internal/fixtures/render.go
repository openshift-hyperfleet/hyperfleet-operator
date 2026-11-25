// Copyright 2026. Licensed under the Apache License, Version 2.0.

// Package fixtures prepares the disposable database and broker for an upgrade run.
package fixtures

import (
	"bytes"
	"crypto/rand"
	_ "crypto/sha256" // Register SHA-256 for distribution/reference.
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const outputName = "fixtures.yaml"

// Render creates a private fixtures.yaml beside the release-qualified scenario.
func Render(dir, postgresImage, rabbitmqImage string) error {
	if err := checkScenario(filepath.Join(dir, "scenario.yaml")); err != nil {
		return err
	}
	template, err := os.ReadFile(filepath.Join(dir, "fixtures.template.yaml"))
	if err != nil {
		return err
	}
	for name, image := range map[string]string{
		"UPGRADE_POSTGRES_IMAGE": postgresImage, "UPGRADE_RABBITMQ_IMAGE": rabbitmqImage,
	} {
		ref, err := reference.ParseNormalizedNamed(image)
		digested, ok := ref.(reference.Digested)
		if err != nil || !ok || digested.Digest().Algorithm() != "sha256" {
			return fmt.Errorf("%s must be a SHA-256 digest-pinned image reference", name)
		}
	}
	// The prefix keeps even an all-numeric random password a YAML string.
	rendered := string(template)
	for _, input := range []struct{ name, value string }{
		{"UPGRADE_POSTGRES_IMAGE", postgresImage}, {"UPGRADE_RABBITMQ_IMAGE", rabbitmqImage},
		{"UPGRADE_DB_PASSWORD", "hf-" + rand.Text()}, {"UPGRADE_BROKER_PASSWORD", "hf-" + rand.Text()},
	} {
		token := "${" + input.name + "}"
		if !strings.Contains(rendered, token) {
			return fmt.Errorf("fixture template must contain %s", token)
		}
		rendered = strings.ReplaceAll(rendered, token, input.value)
	}
	if strings.Contains(rendered, "${") || strings.Contains(rendered, "$UPGRADE_") {
		return errors.New("fixture template contains an unresolved placeholder")
	}
	path := filepath.Join(dir, outputName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(rendered)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	return nil
}

func checkScenario(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("${")) || bytes.Contains(data, []byte("$UPGRADE_")) {
		return errors.New("release-qualified scenario still contains placeholders")
	}
	// Only the pinned target suite knows the release's full scenario schema.
	var scenario struct {
		Fixtures string `json:"fixtures"`
	}
	decoder := yaml.NewYAMLToJSONDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&scenario); err != nil {
		return errors.New("invalid scenario YAML")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single scenario YAML document")
	}
	if scenario.Fixtures != outputName {
		return fmt.Errorf("scenario must reference %s beside itself", outputName)
	}
	return nil
}
