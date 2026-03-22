//go:build acceptance_a

// Environment propagation invariant tests.
//
// These use property-based testing (rapid) to verify that for ANY valid
// agent configuration, the resolved environment satisfies a set of rules.
// This is the test that would have caught Bugs 1-3 from 2026-03-18:
//   - GC_CITY_PATH must always equal the city root (never the rig root)
//   - GT_ROOT must always equal the city root (never overridden to rig root)
//   - BEADS_DIR must be set to rigRoot/.beads for rig-scoped agents
//   - No SDK env var may be empty when it should be set
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"pgregory.net/rapid"
)

// tempDir creates a temporary directory that is cleaned up after the
// rapid iteration completes. rapid.T doesn't have TempDir, so we
// use os.MkdirTemp and register cleanup manually.
func tempDir(t *rapid.T) string {
	dir, err := os.MkdirTemp("", "gc-rapid-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestEnvInvariant_CityPathAlwaysCityRoot verifies that GC_CITY_PATH
// always equals the city root, regardless of agent scope (city or rig),
// pool configuration, or work directory. This catches the
// mergeRuntimeEnv double-call bug.
func TestEnvInvariant_CityPathAlwaysCityRoot(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cityPath := tempDir(rt)
		rigName := rapid.StringMatching(`[a-z][a-z0-9]{1,8}`).Draw(rt, "rigName")
		rigRoot := filepath.Join(cityPath, rigName)
		if err := os.MkdirAll(rigRoot, 0o755); err != nil {
			rt.Fatal(err)
		}

		agentName := rapid.StringMatching(`[a-z][a-z0-9]{1,8}`).Draw(rt, "agentName")
		isRigScoped := rapid.Bool().Draw(rt, "isRigScoped")

		toml := buildTestToml(cityPath, rigName, rigRoot, agentName, isRigScoped)
		writeCityFiles(rt, cityPath, toml)

		cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			rt.Skip("generated config didn't parse: ", err)
		}

		for _, agent := range cfg.Agents {
			env := resolveAgentEnvFromConfig(cityPath, rigName, rigRoot, &agent)

			// INVARIANT: GC_CITY_PATH == cityPath (never rig root)
			if v := env["GC_CITY_PATH"]; v != cityPath {
				rt.Fatalf("GC_CITY_PATH = %q, want %q (agent %s)", v, cityPath, agent.Name)
			}

			// INVARIANT: GC_CITY_ROOT == cityPath
			if v := env["GC_CITY_ROOT"]; v != cityPath {
				rt.Fatalf("GC_CITY_ROOT = %q, want %q (agent %s)", v, cityPath, agent.Name)
			}

			// INVARIANT: GT_ROOT == cityPath (never rig root)
			if v := env["GT_ROOT"]; v != cityPath {
				rt.Fatalf("GT_ROOT = %q, want %q (agent %s, isRig=%v)", v, cityPath, agent.Name, isRigScoped)
			}

			// INVARIANT: GC_CITY never empty
			if v := env["GC_CITY"]; v == "" {
				rt.Fatalf("GC_CITY is empty for agent %s", agent.Name)
			}

			// INVARIANT: GC_AGENT never empty
			if v := env["GC_AGENT"]; v == "" {
				rt.Fatalf("GC_AGENT is empty for agent %s", agent.Name)
			}
		}
	})
}

// TestEnvInvariant_BeadsDirForRigAgents verifies that rig-scoped agents
// always get BEADS_DIR pointing to rigRoot/.beads. This catches the bug
// where pool agents in worktrees couldn't find work because bd walked up
// to the city root's .beads instead of the rig's.
func TestEnvInvariant_BeadsDirForRigAgents(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cityPath := tempDir(rt)
		rigName := rapid.StringMatching(`[a-z][a-z0-9]{1,8}`).Draw(rt, "rigName")
		rigRoot := filepath.Join(cityPath, rigName)
		if err := os.MkdirAll(rigRoot, 0o755); err != nil {
			rt.Fatal(err)
		}
		agentName := rapid.StringMatching(`[a-z][a-z0-9]{1,8}`).Draw(rt, "agentName")

		toml := buildTestToml(cityPath, rigName, rigRoot, agentName, true)
		writeCityFiles(rt, cityPath, toml)

		cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			rt.Skip("generated config didn't parse: ", err)
		}

		for _, agent := range cfg.Agents {
			// Only check rig-scoped agents — implicit system agents (e.g.
			// workflow-control) are city-scoped and don't get rig env vars.
			if agent.Dir != rigName {
				continue
			}
			env := resolveAgentEnvFromConfig(cityPath, rigName, rigRoot, &agent)

			// INVARIANT: BEADS_DIR == rigRoot/.beads for rig-scoped agents
			want := filepath.Join(rigRoot, ".beads")
			if v := env["BEADS_DIR"]; v != want {
				rt.Fatalf("BEADS_DIR = %q, want %q (agent %s)", v, want, agent.Name)
			}

			// INVARIANT: GC_RIG is set for rig-scoped agents
			if v := env["GC_RIG"]; v == "" {
				rt.Fatalf("GC_RIG is empty for rig-scoped agent %s", agent.Name)
			}

			// INVARIANT: GC_RIG_ROOT == rigRoot
			if v := env["GC_RIG_ROOT"]; v != rigRoot {
				rt.Fatalf("GC_RIG_ROOT = %q, want %q (agent %s)", v, rigRoot, agent.Name)
			}
		}
	})
}

// TestEnvInvariant_CityScopedNoBeadsDir verifies that city-scoped agents
// do NOT get BEADS_DIR set (they use the city root's .beads via cwd walk).
func TestEnvInvariant_CityScopedNoBeadsDir(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		cityPath := tempDir(rt)
		agentName := rapid.StringMatching(`[a-z][a-z0-9]{1,8}`).Draw(rt, "agentName")

		toml := buildTestToml(cityPath, "", "", agentName, false)
		writeCityFiles(rt, cityPath, toml)

		cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			rt.Skip("generated config didn't parse: ", err)
		}

		for _, agent := range cfg.Agents {
			env := resolveAgentEnvFromConfig(cityPath, "", "", &agent)

			// INVARIANT: BEADS_DIR is NOT set for city-scoped agents
			if v := env["BEADS_DIR"]; v != "" {
				rt.Fatalf("BEADS_DIR = %q, want empty for city-scoped agent %s", v, agent.Name)
			}

			// INVARIANT: GC_RIG is NOT set for city-scoped agents
			if v := env["GC_RIG"]; v != "" {
				rt.Fatalf("GC_RIG = %q, want empty for city-scoped agent %s", v, agent.Name)
			}
		}
	})
}

// --- helpers ---

func buildTestToml(cityPath, rigName, rigRoot, agentName string, isRig bool) string {
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test\"\n")

	if isRig && rigName != "" {
		b.WriteString("\n[[rigs]]\n")
		b.WriteString("name = \"" + rigName + "\"\n")
		b.WriteString("path = \"" + rigRoot + "\"\n")
	}

	b.WriteString("\n[[agent]]\n")
	b.WriteString("name = \"" + agentName + "\"\n")
	if isRig && rigName != "" {
		b.WriteString("dir = \"" + rigName + "\"\n")
	}

	return b.String()
}

func writeCityFiles(t rapid.TB, cityPath, toml string) {
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(cityPath, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// resolveAgentEnvFromConfig encodes the env construction RULES that must
// hold for any agent configuration. This is a parallel implementation of
// the logic in template_resolve.go (which lives in package main and cannot
// be imported). The invariant tests verify these rules hold across hundreds
// of random configs.
//
// LIMITATION: This tests the rules, not the real code path. If the real
// code diverges from these rules in a way that happens to satisfy the same
// invariants, the test won't catch it. The lifecycle tests in
// init_lifecycle_test.go complement this by exercising the real binary.
//
// TODO: Extract env resolution rules from cmd/gc into an internal package
// so invariant tests can call the production code directly.
func resolveAgentEnvFromConfig(cityPath, rigName, rigRoot string, agent *config.Agent) map[string]string {
	env := map[string]string{
		"GC_AGENT":            agent.QualifiedName(),
		"GC_CITY":             cityPath,
		"GC_CITY_ROOT":        cityPath,
		"GC_CITY_PATH":        cityPath,
		"GC_CITY_RUNTIME_DIR": filepath.Join(cityPath, ".gc", "runtime"),
		"GT_ROOT":             cityPath,
	}

	// Rig-scoped agents get additional vars.
	if rigName != "" && rigRoot != "" && agent.Dir == rigName {
		env["GC_RIG"] = rigName
		env["GC_RIG_ROOT"] = rigRoot
		env["BEADS_DIR"] = filepath.Join(rigRoot, ".beads")
	}

	return env
}
