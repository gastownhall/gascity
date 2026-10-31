//go:build windows

package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestDoSlingForceRefusesABeadIDCmdExeWouldRead pins that a forced CLI sling,
// which skips the store lookup, never substitutes a bead ID cmd.exe would read
// as code into a custom sling_query on Windows.
func TestDoSlingForceRefusesABeadIDCmdExeWouldRead(t *testing.T) {
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{a}}
	runner := newFakeRunner()
	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), runner.run)
	opts := testOpts(a, "BL-1&calc")
	opts.Force = true

	if code := doSling(opts, deps, nil, stdout, stderr); code == 0 {
		t.Errorf("doSling = 0, want a refusal; stderr: %s", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Errorf("doSling ran %q", runner.calls)
	}
}
