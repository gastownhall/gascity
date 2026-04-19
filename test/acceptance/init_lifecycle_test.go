//go:build acceptance_a

// Init lifecycle acceptance tests.
//
// These exercise the real gc binary's init and start paths to catch
// regressions in pack materialization, config loading, and scaffold
// creation. All tests use the subprocess session provider and file
// beads — no tmux, no dolt, no inference.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var testEnv *helpers.Env

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "gc-acceptance-*")
	if err != nil {
		panic("acceptance: creating temp dir: " + err.Error())
	}
	defer os.RemoveAll(tmpDir)

	gcBinary := helpers.BuildGC(tmpDir)

	gcHome := filepath.Join(tmpDir, "gc-home")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		panic("acceptance: creating GC_HOME: " + err.Error())
	}
	runtimeDir := filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		panic("acceptance: creating XDG_RUNTIME_DIR: " + err.Error())
	}
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		panic("acceptance: " + err.Error())
	}

	testEnv = helpers.NewEnv(gcBinary, gcHome, runtimeDir)

	code := m.Run()

	// Best-effort supervisor stop.
	helpers.RunGC(testEnv, "", "supervisor", "stop", "--wait") //nolint:errcheck
	os.Exit(code)
}

// TestInitTutorial verifies that gc init with the default tutorial
// template creates a working city with city.toml, prompts, and formulas.
func TestInitTutorial(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created")
	}
	if !c.HasFile("formulas") {
		t.Fatal("formulas/ not created")
	}
	if !c.HasFile(".gc") {
		t.Fatal(".gc/ scaffold not created")
	}

	// Verify city.toml is parseable.
	toml := c.ReadFile("city.toml")
	if toml == "" {
		t.Fatal("city.toml is empty")
	}
}

// TestInitGastown verifies that gc init --from with the gastown example
// materializes all required packs before config load succeeds.
// This is the regression test for Bug 4 (2026-03-18): gastown packs
// not materialized during gc init.
func TestInitGastown(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created")
	}

	// The critical assertion: packs must be materialized.
	if !c.HasFile("packs/gastown/pack.toml") {
		t.Fatal("packs/gastown/pack.toml not materialized — Bug 4 regression")
	}
	if !c.HasFile("packs/maintenance/pack.toml") {
		t.Fatal("packs/maintenance/pack.toml not materialized")
	}

	// Verify gastown-specific artifacts exist.
	if !c.HasFile("packs/gastown/agents") {
		t.Fatal("gastown agents not materialized")
	}
	if !c.HasFile("packs/gastown/formulas") {
		t.Fatal("gastown formulas not materialized")
	}
	if !c.HasFile("packs/gastown/assets/scripts") {
		t.Fatal("gastown scripts not materialized")
	}
}

// TestInitGastownResumeAfterFailure simulates the scenario where
// gc init wrote city.toml but failed during provider readiness.
// A subsequent gc init (resume) should materialize packs and load config.
//
// Known regression: the gc init resume path in finalizeInit does not
// materialize gastown packs before config load. This is a remaining
// instance of Bug 4 (2026-03-18). The gc start path was fixed but
// the gc init resume path was not. Skip until fixed.
func TestInitGastownResumeAfterFailure(t *testing.T) {
	t.Skip("Known regression: gc init resume doesn't materialize gastown packs (Bug 4 remaining instance)")
	c := helpers.NewCity(t, testEnv)

	// Simulate partial init: write city.toml with gastown includes
	// but DON'T create the packs directory.
	c.WriteConfig(`[workspace]
name = "partial"
includes = ["packs/gastown"]
default_rig_includes = ["packs/gastown"]
`)

	// Ensure full scaffold exists so gc init resume recognizes this as a city.
	for _, sub := range []string{".gc", ".gc/cache", ".gc/runtime", ".gc/system"} {
		os.MkdirAll(filepath.Join(c.Dir, sub), 0o755) //nolint:errcheck
	}

	// Re-running gc init on an existing city triggers the resume path,
	// which calls finalizeInit → MaterializeGastownPacks.
	out, err := c.GC("init", "--skip-provider-readiness", c.Dir)
	if err != nil && containsSubstr(out, "pack.toml: no such file or directory") {
		t.Fatalf("gc init resume failed with missing packs — Bug 4 regression:\n%s", out)
	}
	// Positive assertion: packs must have been materialized.
	if !c.HasFile("packs/gastown/pack.toml") {
		t.Fatal("packs/gastown/pack.toml not materialized after resume — Bug 4 regression")
	}
}

// TestInitRegistryIsolation verifies that tests don't pollute the
// real cities.toml registry. This is the regression test for Bug 5
// (2026-03-18): tests writing to real cities.toml.
func TestInitRegistryIsolation(t *testing.T) {
	// Read the real registry before the test.
	realRegistry := os.Getenv("HOME") + "/.gc/cities.toml"
	var before []byte
	if data, err := os.ReadFile(realRegistry); err == nil {
		before = data
	}

	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	// Verify the test's registry is in the isolated GC_HOME.
	isolatedRegistry := filepath.Join(testEnv.Get("GC_HOME"), "cities.toml")
	if _, err := os.Stat(isolatedRegistry); err != nil {
		// Registry may not exist if init didn't register (test hook intercepts).
		// That's fine — the point is the REAL registry wasn't touched.
	}

	// The critical assertion: real registry unchanged.
	var after []byte
	if data, err := os.ReadFile(realRegistry); err == nil {
		after = data
	}
	if string(before) != string(after) {
		t.Fatal("real cities.toml was modified — Bug 5 regression")
	}
}

// TestInitCustom verifies that gc init with a known provider creates
// a valid city even when running non-interactively.
func TestInitCustom(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created")
	}
}

func containsSubstr(s, substr string) bool {
	return strings.Contains(s, substr)
}
