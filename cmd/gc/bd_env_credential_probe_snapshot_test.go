package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestHostedCredentialProbeDeclinesTheRevisionSnapshot pins that the hosted
// Beads credential probe in bd_env.go declines the load-time revision snapshot.
//
// citySelectsHostedBeadsCredentialProvider loads city.toml, reads one boolean
// off the storage binding, and discards the Provenance. It never computes a
// config.Revision(), so it can never observe the snapshot — but the default
// load builds one anyway, and building it content-hashes (reads + SHA-256s)
// every file of every pack directory, recursively.
//
// That probe runs once per bd command-runner construction, which is once per bd
// subprocess. On a long-running controller that is thousands of times per tick.
// Measured on gc-management 2026-09-16 (ga-s3cnmy): 72.7% of ALL controller CPU
// was inside config.LoadWithIncludesOptions, 80% of that under this one
// function, and declining the snapshot cut a single load from 94.6ms to 38.8ms.
//
// This is a source-level guard for the same reason
// TestCityConfigLoadersDeclineTheRevisionSnapshot is: reverting to the default
// returns exactly the same config and passes every functional test. Nothing
// fails, it only gets slower — precisely the regression a suite does not
// otherwise catch.
//
// Like that guard, this requires a *named* options value rather than an inline
// literal, so the reasoning above stays attached to the option instead of to a
// call site the next edit can silently drop.
func TestHostedCredentialProbeDeclinesTheRevisionSnapshot(t *testing.T) {
	const guarded = "bd_env.go"
	// capturingCall ends in '(' so it does not also match
	// config.LoadWithIncludesOptions(, whose next character is 'O'.
	const capturingCall = "config.LoadWithIncludes("
	const optionsType = "config.LoadOptions"
	const option = "SkipRevisionSnapshot: true"

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), guarded))
	if err != nil {
		t.Fatalf("reading %s: %v", guarded, err)
	}
	text := string(src)

	if strings.Contains(text, capturingCall) {
		t.Errorf("%s calls %s, which captures the load-time revision snapshot; "+
			"use config.LoadWithIncludesOptions with a named config.LoadOptions "+
			"value that sets %s", guarded, capturingCall, option)
	}

	// Every config.LoadOptions value this file declares must decline the
	// snapshot. A zero value declared without a literal captures it just as
	// surely, so match on the type name rather than on an opening brace.
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, optionsType) {
			continue
		}
		if strings.Contains(line, "LoadWithIncludesOptions") {
			continue // a call passing a value, checked by the declaration itself
		}
		if !strings.Contains(line, option) {
			t.Errorf("%s declares a %s that does not set %s: %s",
				guarded, optionsType, option, strings.TrimSpace(line))
		}
	}
}
