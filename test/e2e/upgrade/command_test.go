// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/onsi/gomega/matchers"
)

const commandSecretMarker = "synthetic-registry-password"

func TestCommandStderrHelper(t *testing.T) {
	if os.Getenv("UPGRADE_STDERR_HELPER") != "1" {
		return
	}
	if _, err := fmt.Fprint(os.Stderr, commandSecretMarker); err != nil {
		t.Fatal("write synthetic stderr", err)
	}
	os.Exit(1)
}

func TestCommandFailureCannotExposeStderrThroughGomega(t *testing.T) {
	t.Setenv("UPGRADE_STDERR_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runCommand(t.Context(), executable, "-test.run=^TestCommandStderrHelper$")
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatal("expected helper process failure", err)
	}
	if len(exitError.Stderr) != 0 {
		t.Fatal("unredacted command stderr remains in the returned error")
	}
	wrapped := fmt.Errorf("render bundle: %w", err)
	formatted := (&matchers.SucceedMatcher{}).FailureMessage(wrapped)
	if strings.Contains(formatted, commandSecretMarker) || strings.Contains(wrapped.Error(), commandSecretMarker) {
		t.Fatal("Gomega failure exposed command stderr")
	}
}
