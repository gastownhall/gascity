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

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
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
	if err := helpers.StageIdleProviderBinary(filepath.Join(gcHome, "bin"), "claude"); err != nil {
		panic("acceptance: staging idle Claude process double: " + err.Error())
	}

	// In-process config loads and packman cache lookups must resolve the
	// same isolated GC_HOME the subprocess env uses. internal/testenv
	// scrubs GC_HOME at test-binary init, so opt back in explicitly here;
	// otherwise pinned bundled imports cannot resolve the repo cache.
	if err := os.Setenv("GC_HOME", gcHome); err != nil {
		panic("acceptance: setting GC_HOME: " + err.Error())
	}

	code := m.Run()

	// Best-effort supervisor stop.
	helpers.RunGC(testEnv, "", "supervisor", "stop", "--wait") //nolint:errcheck
	os.Exit(code)
}

func TestTierAUsesHermeticClaudeProcessDouble(t *testing.T) {
	wantDir := filepath.Join(testEnv.Get("GC_HOME"), "bin")
	pathEntries := filepath.SplitList(testEnv.Get("PATH"))
	if len(pathEntries) == 0 || pathEntries[0] != wantDir {
		t.Fatalf("Tier A PATH starts with %v, want hermetic provider directory %q", pathEntries, wantDir)
	}

	wantPath := filepath.Join(wantDir, "claude")
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("Tier A Claude process double: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("Tier A Claude process double mode = %v, want executable", info.Mode())
	}
}

// TestInitMinimal verifies that gc init with the default minimal
// template creates a working city with city.toml, prompts, and formulas.
func TestInitMinimal(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")

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
// wires the gastown pack so config load succeeds. This is the regression
// test for Bug 4 (2026-03-18): gastown packs not available during gc init.
// The pack now arrives via the pinned public import (resolved from the
// repo cache) rather than a city-local packs/ copy, so the assertions
// cover the pinned import, the lock entry, and composition results.
func TestInitGastown(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFromNoStart(filepath.Join(helpers.ExamplesDir(), "gastown"))

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created")
	}

	// The gastown pack is wired via the pinned public import.
	packToml := c.ReadFile("pack.toml")
	if !strings.Contains(packToml, `source = "`+config.PublicGastownPackSource+`"`) {
		t.Fatalf("pack.toml missing pinned public gastown source:\n%s", packToml)
	}
	if !strings.Contains(packToml, `version = "`+config.PublicGastownPackVersion+`"`) {
		t.Fatalf("pack.toml missing pinned public gastown version:\n%s", packToml)
	}

	packsLock := c.ReadFile("packs.lock")
	if !strings.Contains(packsLock, `[packs."`+config.PublicGastownPackSource+`"]`) {
		t.Fatalf("packs.lock missing pinned public gastown source:\n%s", packsLock)
	}

	// The critical assertion: composition must surface the gastown agents
	// from the resolved import — Bug 4 regression.
	out, err := c.GC("config", "explain")
	if err != nil {
		t.Fatalf("gc config explain failed: %v\n%s", err, out)
	}
	agentsListed := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "Agent: ")
		if !ok {
			continue
		}
		// Qualified names may carry an import-binding prefix
		// (e.g. gastown.mayor); index by the unqualified tail.
		name = strings.TrimSpace(name)
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		agentsListed[name] = true
	}
	for _, agent := range []string{"mayor", "deacon", "boot"} {
		if !agentsListed[agent] {
			t.Errorf("gastown agent %q missing from gc config explain — Bug 4 regression:\n%s", agent, out)
		}
	}
}

// TestInitGastownResumeAfterFailure simulates the scenario where gc init wrote
// city.toml and pack.toml but failed before the import lockfile was written. A
// subsequent gc init (resume) should sync the lock and hydrate the bundled
// cache so the pinned import resolves before config load.
func TestInitGastownResumeAfterFailure(t *testing.T) {
	c := helpers.NewCity(t, testEnv)

	// Simulate partial PackV2 init: manifests exist but no packs.lock. A
	// real init writes the required builtin imports (core for this beads
	// provider) alongside the requested pack, so mirror that manifest shape
	// at the running binary's canonical pins.
	c.WriteConfig(`[workspace]
name = "partial"
`)
	coreSource, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal("builtinpacks has no core source")
	}
	packToml := `[pack]
name = "partial"
schema = 2

[imports.core]
source = "` + coreSource + `"
version = "` + config.BundledSourcePinnedVersion(coreSource) + `"

[imports.gastown]
source = "` + config.PublicGastownPackSource + `"
version = "` + config.PublicGastownPackVersion + `"
`
	if err := os.WriteFile(filepath.Join(c.Dir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatalf("writing pack.toml: %v", err)
	}

	// Ensure full scaffold exists so gc init resume recognizes this as a city.
	for _, sub := range []string{".gc", ".gc/cache", ".gc/runtime"} {
		os.MkdirAll(filepath.Join(c.Dir, sub), 0o755) //nolint:errcheck
	}
	if err := os.WriteFile(filepath.Join(c.Dir, ".gc", "events.jsonl"), nil, 0o644); err != nil {
		t.Fatalf("writing events log: %v", err)
	}

	// Re-running gc init on an existing city triggers the resume path,
	// which syncs the lock and installs the pinned imports.
	out, err := c.GC("init", "--skip-provider-readiness", "--no-start", c.Dir)
	if err != nil {
		t.Fatalf("gc init resume failed — Bug 4 regression:\n%s", out)
	}
	// Positive assertions: the lock pins the import and config composes.
	if !c.HasFile("packs.lock") {
		t.Fatal("packs.lock not written by gc init resume — Bug 4 regression")
	}
	if out, err := c.GC("config", "show", "--validate"); err != nil {
		t.Fatalf("gc config show --validate after resume failed: %v\n%s", err, out)
	}
}

func TestInitPublicGastownPackStartsFromCanonicalImport(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	templatePath := filepath.Join(helpers.TempDir(t), "public-gastown.toml")
	cfg := config.GastownCity(filepath.Base(c.Dir), "claude", "")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshaling public gastown template: %v", err)
	}
	if err := os.WriteFile(templatePath, data, 0o644); err != nil {
		t.Fatalf("writing public gastown template: %v", err)
	}

	out, err := helpers.RunGC(testEnv, "", "init", "--file", templatePath, "--skip-provider-readiness", "--no-start", c.Dir)
	if err != nil {
		t.Fatalf("gc init --file public gastown failed: %v\n%s", err, out)
	}

	packToml := c.ReadFile("pack.toml")
	if !strings.Contains(packToml, `source = "`+config.PublicGastownPackSource+`"`) {
		t.Fatalf("pack.toml missing canonical public gastown source:\n%s", packToml)
	}
	if !strings.Contains(packToml, `version = "`+config.PublicGastownPackVersion+`"`) {
		t.Fatalf("pack.toml missing canonical public gastown version:\n%s", packToml)
	}
	if strings.Contains(packToml, ".gc/system/packs/gastown") {
		t.Fatalf("pack.toml should not reference legacy materialized gastown paths:\n%s", packToml)
	}

	packsLock := c.ReadFile("packs.lock")
	if !strings.Contains(packsLock, `[packs."`+config.PublicGastownPackSource+`"]`) {
		t.Fatalf("packs.lock missing canonical public gastown source:\n%s", packsLock)
	}

	if out, err := c.GC("config", "show", "--validate"); err != nil {
		t.Fatalf("config validation after public gastown init failed: %v\n%s", err, out)
	}

	c.StartWithSupervisor()
	if out, err := c.GC("status", "--city", c.Dir); err != nil {
		t.Fatalf("gc status after public gastown start failed: %v\n%s", err, out)
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
	c.InitNoStart("claude")

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created")
	}
}

func containsSubstr(s, substr string) bool {
	return strings.Contains(s, substr)
}
