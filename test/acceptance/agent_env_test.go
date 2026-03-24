//go:build acceptance_a

// Agent config loading acceptance tests.
//
// For each example config, verifies that gc init produces a city where
// config explain loads successfully without missing pack errors.
// These tests exercise the real gc binary's config resolution path.
//
// NOTE: These test config LOADING, not env var VALUES. Env var value
// assertions are in env_invariant_test.go (property-based) and will be
// extended in Tier B tests that start agents and capture their env.
package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// TestConfigLoad_GastownCityAgents verifies that gastown config loads
// without errors and produces city-scoped agents.
func TestConfigLoad_GastownCityAgents(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	out, err := c.GC("config", "explain", "--city", c.Dir)
	if err != nil {
		t.Fatalf("gc config explain: %v\n%s", err, out)
	}

	if strings.Contains(out, "pack.toml: no such file") {
		t.Fatalf("config explain failed with missing packs:\n%s", out)
	}

	// Gastown must produce at least these city-scoped agents.
	for _, agent := range []string{"mayor", "deacon", "boot"} {
		if !strings.Contains(out, agent) {
			t.Errorf("config explain missing city-scoped agent %q", agent)
		}
	}
}

// TestConfigLoad_TutorialAgent verifies the tutorial config produces
// at least one agent.
func TestConfigLoad_TutorialAgent(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	out, err := c.GC("config", "explain", "--city", c.Dir)
	if err != nil {
		t.Fatalf("gc config explain: %v\n%s", err, out)
	}

	if !strings.Contains(out, "Agent:") {
		t.Fatal("config explain shows no agents for tutorial config")
	}
}

// TestConfigLoad_GastownWithRig verifies that gastown config with a
// rig loads without pack errors.
func TestConfigLoad_GastownWithRig(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	rigDir := filepath.Join(c.Dir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	toml := c.ReadFile("city.toml")
	toml += "\n[[rigs]]\nname = \"myrig\"\npath = \"" + rigDir + "\"\nincludes = [\"packs/gastown\"]\n"
	c.WriteConfig(toml)

	out, err := c.GC("config", "explain", "--city", c.Dir)
	if err != nil && strings.Contains(out, "pack.toml: no such file") {
		t.Fatalf("config explain failed with missing packs for rig config:\n%s", out)
	}
}

// TestConfigLoad_SwarmConfig verifies the swarm example config loads.
func TestConfigLoad_SwarmConfig(t *testing.T) {
	swarmDir := filepath.Join(helpers.ExamplesDir(), "swarm")
	if _, err := os.Stat(swarmDir); err != nil {
		t.Skip("swarm example not found")
	}

	c := helpers.NewCity(t, testEnv)
	c.InitFrom(swarmDir)

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created for swarm config")
	}

	rigDir := filepath.Join(c.Dir, "swarm-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := c.ReadFile("city.toml")
	toml += "\n[[rigs]]\nname = \"swarm\"\npath = \"" + rigDir + "\"\nincludes = [\"packs/swarm\"]\n"
	c.WriteConfig(toml)

	out, err := c.GC("config", "explain", "--city", c.Dir)
	if err != nil && strings.Contains(out, "pack.toml: no such file") {
		t.Fatalf("swarm config has missing pack references:\n%s", out)
	}
}

// TestConfigLoad_LifecycleWithRig verifies the lifecycle example config loads
// when a rig is registered, which is required for its rig-scoped agents.
func TestConfigLoad_LifecycleWithRig(t *testing.T) {
	lifecycleDir := filepath.Join(helpers.ExamplesDir(), "lifecycle")
	if _, err := os.Stat(lifecycleDir); err != nil {
		t.Skip("lifecycle example not found")
	}

	c := helpers.NewCity(t, testEnv)
	c.InitFrom(lifecycleDir)

	if !c.HasFile("city.toml") {
		t.Fatal("city.toml not created for lifecycle config")
	}

	rigDir := filepath.Join(c.Dir, "lifecycle-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := c.ReadFile("city.toml")
	toml += "\n[[rigs]]\nname = \"lifecycle\"\npath = \"" + rigDir + "\"\nincludes = [\"packs/lifecycle\"]\n"
	c.WriteConfig(toml)

	out, err := c.GC("config", "explain", "--city", c.Dir)
	if err != nil && strings.Contains(out, "pack.toml: no such file") {
		t.Fatalf("lifecycle config has missing pack references:\n%s", out)
	}
}
