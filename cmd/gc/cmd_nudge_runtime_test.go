package main

import (
	"bytes"
	"io"
	"runtime/debug"
	"strings"
	"testing"
)

// readMemLimit returns the current soft memory limit without changing it.
// debug.SetMemoryLimit treats a negative argument as a read-only query.
func readMemLimit() int64 { return debug.SetMemoryLimit(-1) }

// These tests mutate the process-global Go memory limit, so they must run
// serially (no t.Parallel) and restore the original limit on exit.

// TestConfigureNudgePollRuntimeInstallsDefaultLimit verifies the nudge-poll
// sidecar installs a soft memory limit when none is set in the environment.
// This is the gc-3ftcq regression guard: without the bound, each sidecar's RSS
// plateaus near 1GB because the whole-file city store is re-parsed every tick
// and the Go runtime retains the arena. A measured before/after for the real
// 124MB city beads.json: ~1161MB Sys -> ~607MB Sys (debug docs at the call site).
func TestConfigureNudgePollRuntimeInstallsDefaultLimit(t *testing.T) {
	orig := readMemLimit()
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	// A high explicit override avoids constraining other tests sharing this
	// process while still proving the limit is installed from the env value.
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv(nudgePollMemLimitEnv, "4096")

	// Start from a sentinel so we can prove the value changed to ours.
	debug.SetMemoryLimit(1 << 62)
	stop := configureNudgePollRuntime(io.Discard)
	defer stop()

	if got, want := readMemLimit(), int64(4096)*1024*1024; got != want {
		t.Fatalf("memory limit = %d, want %d", got, want)
	}
	if stop == nil {
		t.Fatal("cleanup func is nil")
	}
}

// TestConfigureNudgePollRuntimeUsesDefaultWhenUnset verifies the built-in
// default applies when the override env is absent.
func TestConfigureNudgePollRuntimeUsesDefaultWhenUnset(t *testing.T) {
	orig := readMemLimit()
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	t.Setenv("GOMEMLIMIT", "")
	t.Setenv(nudgePollMemLimitEnv, "")

	debug.SetMemoryLimit(1 << 62)
	stop := configureNudgePollRuntime(io.Discard)
	defer stop()

	if got, want := readMemLimit(), int64(defaultNudgePollMemLimitMB)*1024*1024; got != want {
		t.Fatalf("memory limit = %d, want default %d", got, want)
	}
}

// TestConfigureNudgePollRuntimeDisableWithZero verifies "0" disables the soft
// limit so operators can opt out without forcing a GOMEMLIMIT.
func TestConfigureNudgePollRuntimeDisableWithZero(t *testing.T) {
	orig := readMemLimit()
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	t.Setenv("GOMEMLIMIT", "")
	t.Setenv(nudgePollMemLimitEnv, "0")

	sentinel := int64(1 << 62)
	debug.SetMemoryLimit(sentinel)
	stop := configureNudgePollRuntime(io.Discard)
	defer stop()

	if got := readMemLimit(); got != sentinel {
		t.Fatalf("memory limit = %d, want unchanged %d (disabled)", got, sentinel)
	}
}

// TestConfigureNudgePollRuntimeWarnsOnInvalidLimit verifies a malformed override
// (e.g. a unit suffix typo like "512mb") is not silently swallowed: the sidecar
// warns and falls back to the default rather than leaving the operator's intent
// invisibly ignored.
func TestConfigureNudgePollRuntimeWarnsOnInvalidLimit(t *testing.T) {
	orig := readMemLimit()
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	t.Setenv("GOMEMLIMIT", "")
	t.Setenv(nudgePollMemLimitEnv, "512mb")

	debug.SetMemoryLimit(1 << 62)
	var warn bytes.Buffer
	stop := configureNudgePollRuntime(&warn)
	defer stop()

	if got, want := readMemLimit(), int64(defaultNudgePollMemLimitMB)*1024*1024; got != want {
		t.Fatalf("memory limit = %d, want default %d after invalid override", got, want)
	}
	if !strings.Contains(warn.String(), "ignoring invalid") {
		t.Fatalf("expected a warning about the invalid override, got %q", warn.String())
	}
}

// TestConfigureNudgePollRuntimeRespectsGOMEMLIMIT verifies an operator-provided
// GOMEMLIMIT is honored: the runtime already applied it at startup, so the
// sidecar must not override it with the default.
func TestConfigureNudgePollRuntimeRespectsGOMEMLIMIT(t *testing.T) {
	orig := readMemLimit()
	t.Cleanup(func() { debug.SetMemoryLimit(orig) })

	t.Setenv("GOMEMLIMIT", "900MiB")
	t.Setenv(nudgePollMemLimitEnv, "256")

	// The runtime would have set the limit from GOMEMLIMIT at startup; emulate
	// that with a sentinel and prove configureNudgePollRuntime leaves it intact.
	sentinel := int64(1 << 62)
	debug.SetMemoryLimit(sentinel)
	stop := configureNudgePollRuntime(io.Discard)
	defer stop()

	if got := readMemLimit(); got != sentinel {
		t.Fatalf("memory limit = %d, want unchanged %d (GOMEMLIMIT precedence)", got, sentinel)
	}
}
