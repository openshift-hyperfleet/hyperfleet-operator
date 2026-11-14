//go:build upgrade_e2e

// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestOLMUpgrade(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OLM Classic N-1 to N upgrade")
}

// Run uses the interruptible context to stop work, then finishes diagnostics
// and cleanup with separate bounded contexts. Allow both cleanup attempts to
// finish before Ginkgo abandons an interrupted node.
var _ = It("converges a published release and preserves its installation", func(ctx SpecContext) {
	if os.Getenv(cleanupOnlyEnv) == "true" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		Expect(Cleanup(cleanupCtx, os.Getenv(kubeconfigEnv), os.Getenv(artifactDirEnv))).To(Succeed())
		return
	}
	config, err := LoadConfig()
	Expect(err).NotTo(HaveOccurred())
	Expect(Run(ctx, config)).To(Succeed())
}, GracePeriod(25*time.Minute))
