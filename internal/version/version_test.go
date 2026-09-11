package version

import "testing"

// shortTestSHA is the 7-character digest shared by the Commit tests below.
const shortTestSHA = "abc1234"

// TestVersionPrefersInjectedValue verifies Version returns the ldflags-injected
// value when one is set.
func TestVersionPrefersInjectedValue(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v1.2.3"
	if got := Version(); got != "v1.2.3" {
		t.Errorf("Version() = %q, want v1.2.3", got)
	}
}

// TestVersionFallsBackToNonEmpty verifies Version never returns empty: with no
// injected value it falls back to the module version or the "dev" sentinel.
func TestVersionFallsBackToNonEmpty(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = ""
	if got := Version(); got == "" {
		t.Error("Version() returned empty; want module version or \"dev\"")
	}
}

// TestCommitPrefersInjectedValue verifies Commit returns the ldflags-injected
// value when one is set.
func TestCommitPrefersInjectedValue(t *testing.T) {
	orig := commit
	t.Cleanup(func() { commit = orig })

	commit = shortTestSHA
	if got := Commit(); got != shortTestSHA {
		t.Errorf("Commit() = %q, want %q", got, shortTestSHA)
	}
}

// TestCommitTruncatesInjectedFullSHA verifies Commit shortens a full 40-character
// injected SHA to 7 characters. CI (see .tekton/*.yaml GIT_SHA build-arg) injects
// the full commit SHA via -ldflags, and the metric/doc contract is a short SHA.
func TestCommitTruncatesInjectedFullSHA(t *testing.T) {
	orig := commit
	t.Cleanup(func() { commit = orig })

	commit = shortTestSHA + "def5678900000000000000000000000a"
	if got := Commit(); got != shortTestSHA {
		t.Errorf("Commit() = %q, want %q", got, shortTestSHA)
	}
}

// TestCommitFallsBackToNonEmpty verifies Commit never returns empty: with no
// injected value it falls back to the VCS revision or the "unknown" sentinel.
func TestCommitFallsBackToNonEmpty(t *testing.T) {
	orig := commit
	t.Cleanup(func() { commit = orig })

	commit = ""
	if got := Commit(); got == "" {
		t.Error("Commit() returned empty; want vcs.revision or \"unknown\"")
	}
}

// TestGoVersionIsPopulated verifies GoVersion reports the runtime version.
func TestGoVersionIsPopulated(t *testing.T) {
	if GoVersion() == "" {
		t.Error("GoVersion() returned empty")
	}
}
