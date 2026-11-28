package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestInternalMaterializeSkillsSharedSinkKeepsEarlierAgentLinks is the
// issue #7549 triage reproduction: two agents with no work_dir, each
// materialized into the same explicit workdir — a directory no agent is
// configured for, so no union folds both writers. The second pass must
// keep the first agent's links: they live under the first agent's skill
// root, a gc-managed root this pass does not reconcile (ForeignRoots),
// not stale links this pass may prune.
func TestInternalMaterializeSkillsSharedSinkKeepsEarlierAgentLinks(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeMaterializeTestCityFile(t, cityDir, "city.toml", "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(cityDir, "agents", name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeMaterializeTestCityFile(t, cityDir, filepath.Join("agents", name, "agent.toml"), "provider = \"claude\"\nstart_command = \"echo\"\n")
		writeSkillSource(t, filepath.Join(cityDir, "agents", name, "skills", name+"-skill"))
	}

	workdir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{
			"internal", "materialize-skills",
			"--agent", name,
			"--workdir", workdir,
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s's pass exited %d: stderr=%q stdout=%q", name, code, stderr.String(), stdout.String())
		}
	}
	sink := filepath.Join(workdir, ".claude", "skills")
	for _, agent := range []string{"alpha", "beta"} {
		name := agent + "-skill"
		target, err := os.Readlink(filepath.Join(sink, name))
		if err != nil {
			t.Errorf("%s missing from the shared sink: %v", name, err)
			continue
		}
		want := filepath.Join(cityDir, "agents", agent, "skills", name)
		if target != want {
			t.Errorf("%s -> %s, want %s", name, target, want)
		}
	}
}
