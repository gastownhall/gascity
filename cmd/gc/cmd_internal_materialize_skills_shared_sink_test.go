package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestInternalMaterializeSkillsSharedWorkdirKeepsOtherAgentLocalLinks covers
// two sessions of different agents that share one work_dir: each PreStart
// materializes its own agent into the same sink, and the later one must not
// prune the agent-local link the earlier one still wants.
func TestInternalMaterializeSkillsSharedWorkdirKeepsOtherAgentLocalLinks(t *testing.T) {
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
		agentDir := filepath.Join(cityDir, "agents", name)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", agentDir, err)
		}
		writeMaterializeTestCityFile(t, agentDir, "agent.toml", "provider = \"claude\"\nstart_command = \"echo\"\n")
		writeSkillSource(t, filepath.Join(agentDir, "skills", name+"-local"))
	}

	workdir := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{"internal", "materialize-skills", "--agent", name, "--workdir", workdir}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("materialize %s: exit %d: stderr=%q stdout=%q", name, code, stderr.String(), stdout.String())
		}
	}

	for _, skill := range []string{"alpha-local", "beta-local"} {
		link := filepath.Join(workdir, ".claude", "skills", skill)
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("shared sink lost %q after both agents materialized into it: %v", skill, err)
		}
	}
}
