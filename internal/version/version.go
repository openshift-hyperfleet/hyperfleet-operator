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

// Package version exposes the operator's build identity (version, commit, Go
// runtime) so it can be surfaced in logs and in the hyperfleet_operator_build_info
// metric, per the HyperFleet metrics standard.
//
// The values are intended to be injected at build time via -ldflags -X, e.g.
//
//	-X github.com/openshift-hyperfleet/hyperfleet-operator/internal/version.version=v1.2.3
//	-X github.com/openshift-hyperfleet/hyperfleet-operator/internal/version.commit=abc1234
//
// When they are not injected (plain `go build`, `make run`, tests) the accessors
// fall back to the module version and VCS revision recorded in the binary's
// build info, so the metric is still populated with something meaningful.
package version

import (
	"runtime"
	"runtime/debug"
)

// These are overridden at build time via -ldflags -X. Keep them unexported and
// read through the accessors so the build-info fallback below always applies.
var (
	version = ""
	commit  = ""
)

// Version returns the operator version, e.g. "v1.2.3" or "dev-abc1234". It
// prefers the ldflags-injected value, then the module version from build info,
// and finally "dev".
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// Commit returns the short VCS revision the binary was built from, or "unknown"
// when it cannot be determined. Truncated to 7 characters regardless of source,
// since CI conventionally injects the full 40-character SHA via -ldflags (see
// GIT_SHA in the Tekton pipelines) while `make build`'s fallback below already
// shortens it — both must agree on one format.
func Commit() string {
	if commit != "" {
		return shortSHA(commit)
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return shortSHA(s.Value)
			}
		}
	}
	return "unknown"
}

// shortSHA truncates a VCS revision to 7 characters, the convention this
// package's docs and `git rev-parse --short` agree on.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// GoVersion returns the Go runtime version the binary was compiled with.
func GoVersion() string {
	return runtime.Version()
}
