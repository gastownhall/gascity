// This file is Linux-only by its filename, which is the honest constraint: it
// executes the POSIX provider script, and 07-design scopes the proxied
// lifecycle to Linux (rc.2 calls the Windows/macOS lanes "defined, not
// implemented").
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An interrupted `gc rig add` journals a still-initializing `path:` record
// before city.toml is written. If the operator then deletes the half-built
// directory, the record survives — and the provider script's every arm starts
// with `cd "$dir"`, so a retiring op exited 1 on a scope that has no processes
// to retire, which the stop fan-out then reported as a failure.
//
// This drives the real script with the environment Go passes it, not a Go
// stand-in: the `cd` is in the shell, so only the shell can prove it.
func TestProviderScriptStopIsANoOpForAMissingScopeDirectory(t *testing.T) {
	city := t.TempDir()
	missing := filepath.Join(city, "rigs", "deleted")

	// A bd that would fail loudly if it were ever reached.
	env := providerOwnedScriptEnv(city, missing, writeFailingBd(t, city))
	for _, op := range []string{"stop", "shutdown"} {
		out, err := runProviderOwnedScriptOp(t, env, op)
		if err != nil {
			t.Fatalf("%s on a deleted scope directory: %v\n%s", op, err, out)
		}
	}

	out, err := runProviderOwnedScriptOp(t, env, "start")
	if err == nil {
		t.Fatalf("start succeeded for a deleted scope directory:\n%s", out)
	}
	if strings.Contains(out, "BD MUST NOT RUN") {
		t.Fatalf("start invoked bd for a deleted scope directory:\n%s", out)
	}
}

// TestProviderScriptStopIsANoOpForAScopeWithNoBeadsStore covers the other half of
// the interrupted-`gc rig add` shape: the journal records a still-initializing
// path, `bd init` failed, and removePartialBeadsStore deleted the partial .beads
// it had created — so the directory is still there and the store is not.
//
// The script used to short-circuit only on a missing directory, so this shape ran
// `bd dolt stop` with a BEADS_DIR that does not exist. bd ignores such a
// BEADS_DIR and walks up from the cwd, which for a rig that is its own repository
// is "no active beads workspace found" — a message provider_owned_retire_local_dolt
// does not tolerate — so `gc stop` returned a per-scope failure on every run for a
// scope where nothing was running.
func TestProviderScriptStopIsANoOpForAScopeWithNoBeadsStore(t *testing.T) {
	city := t.TempDir()
	scope := filepath.Join(city, "rigs", "half-built")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	env := providerOwnedScriptEnv(city, scope, writeFailingBd(t, city))

	for _, op := range []string{"stop", "shutdown"} {
		out, err := runProviderOwnedScriptOp(t, env, op)
		if err != nil {
			t.Fatalf("%s on a scope with no .beads: %v\n%s", op, err, out)
		}
		if strings.Contains(out, "BD MUST NOT RUN") {
			t.Fatalf("%s invoked bd for a scope with no .beads:\n%s", op, out)
		}
		if !strings.Contains(out, "nothing to retire") {
			t.Fatalf("%s did not say why it was a no-op:\n%s", op, out)
		}
	}

	// Control: the same shape must not become quietly startable.
	out, err := runProviderOwnedScriptOp(t, env, "start")
	if err == nil {
		t.Fatalf("start succeeded for a scope with no .beads:\n%s", out)
	}
}

func writeFailingBd(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'BD MUST NOT RUN' >&2\nexit 9\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return path
}
