// Copyright 2026. Licensed under the Apache License, Version 2.0.

// Command render-fixtures prepares release-qualified fixtures for the upgrade suite.
package main

import (
	"fmt"
	"os"

	"github.com/openshift-hyperfleet/hyperfleet-operator/test/e2e/upgrade/internal/fixtures"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: render-fixtures <scratch-directory>")
		os.Exit(2)
	}
	err := fixtures.Render(os.Args[1], os.Getenv("UPGRADE_POSTGRES_IMAGE"), os.Getenv("UPGRADE_RABBITMQ_IMAGE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
