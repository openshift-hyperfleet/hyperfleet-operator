//go:build upgrade_e2e

// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestOLMUpgrade(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OLM Classic N-1 to N upgrade")
}

var _ = It("converges a published release and preserves its installation", func() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if os.Getenv("UPGRADE_CLEANUP_ONLY") == "true" {
		Expect(Cleanup(ctx, os.Getenv(kubeconfigEnv), os.Getenv("ARTIFACT_DIR"))).To(Succeed())
		return
	}
	config, err := LoadConfig()
	Expect(err).NotTo(HaveOccurred())
	Expect(Run(ctx, config)).To(Succeed())
})
