//go:build integration && unix

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file sorts last in integration_test's srcs on purpose. rules_go shards
// a go_test round-robin in source order (test i runs in shard i % shard_count),
// so a test added ahead of the long ones in heavy_tests.txt shifts them and can
// put two in one shard (scripts TestIntegrationShardsKeepHeavyTestsApart).
// Tests added here leave every existing test's shard where it was.

// TestCommandCityDirForArgs pins commandCityDirForArgs: it resolves the city
// directory from an absolute path argument only for the lifecycle verbs, and
// only when no explicit dir was given. gc() relies on that being a no-op in
// every other case so that the env directory and the working directory agree.
func TestCommandCityDirForArgs(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")

	for _, verb := range []string{"start", "stop", "restart", "suspend", "resume"} {
		if got := commandCityDirForArgs("", []string{verb, cityDir}); got != cityDir {
			t.Errorf("commandCityDirForArgs(\"\", [%s <abs>]) = %q, want %q", verb, got, cityDir)
		}
	}

	noop := []struct {
		name string
		dir  string
		args []string
	}{
		{"explicit dir wins over path arg", "/tmp/cwd", []string{"start", cityDir}},
		{"relative path arg", "", []string{"start", "relative/city"}},
		{"non-lifecycle verb", "", []string{"status", cityDir}},
		{"verb without path arg", "", []string{"start"}},
		{"no args", "", nil},
	}
	for _, tc := range noop {
		if got := commandCityDirForArgs(tc.dir, tc.args); got != tc.dir {
			t.Errorf("%s: commandCityDirForArgs(%q, %v) = %q, want dir %q unchanged", tc.name, tc.dir, tc.args, got, tc.dir)
		}
	}
}

// TestGCRunsInResolvedCityDir pins that gc() runs the subprocess with the same
// directory it selects the city env for. A lifecycle verb given an absolute
// city path and an empty dir resolves the env to that city; the subprocess
// must also start there, not in the test's own working directory, or an
// ambient walk-up from that directory can reach the wrong city.
func TestGCRunsInResolvedCityDir(t *testing.T) {
	cityDir := t.TempDir()
	shim := filepath.Join(t.TempDir(), "gc-pwd-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\npwd -P\n"), 0o755); err != nil {
		t.Fatalf("writing cwd shim: %v", err)
	}
	prev := gcBinary
	gcBinary = shim
	t.Cleanup(func() { gcBinary = prev })

	want, err := filepath.EvalSymlinks(cityDir)
	if err != nil {
		t.Fatalf("resolving %q: %v", cityDir, err)
	}

	// Lifecycle verb + absolute path + empty dir: cwd must be the city.
	out, err := gc("", "start", cityDir)
	if err != nil {
		t.Fatalf("gc(\"\", start, %q): %v\n%s", cityDir, err, out)
	}
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("gc(\"\", start, <city>) ran in %q, want resolved city dir %q", got, want)
	}

	// An explicit dir still wins as the cwd.
	other := t.TempDir()
	wantOther, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatalf("resolving %q: %v", other, err)
	}
	out, err = gc(other, "start", cityDir)
	if err != nil {
		t.Fatalf("gc(%q, start, %q): %v\n%s", other, cityDir, err, out)
	}
	if got := strings.TrimSpace(out); got != wantOther {
		t.Errorf("gc(<dir>, start, <city>) ran in %q, want explicit dir %q", got, wantOther)
	}
}
