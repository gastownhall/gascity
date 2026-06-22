package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestSkillInstallDirsPerProviderAcrossScopes is the issue #3643 regression
// guard. The requirement: in a fresh install the pack skill must be
// installed into the directory each provider's CLI actually reads, at the
// city scope AND at every rig scope — whether the rig lives under the city
// tree (a subdir rig) or out of tree.
//
// It drives the real production path (InjectImplicitAgents → stage-1
// materialization) rather than hand-authored [[agent]] entries, because the
// implicit per-provider agents are what a default `gc init` city relies on,
// and that path is what the bug report exercised.
//
// canonicalSink is the project-scoped skills directory each provider's own
// CLI scans, verified against vendor docs (2026-06):
//
//	claude   → .claude/skills   (code.claude.com/docs/en/skills)
//	codex    → .agents/skills   (developers.openai.com/codex/skills — Codex
//	                             does NOT read a project-scoped .codex/skills)
//	gemini   → .gemini/skills   (github.com/google-gemini/gemini-cli)
//	opencode → .opencode/skills (opencode.ai/docs/skills)
//	mimocode → .mimocode/skills (mimo.xiaomi.com/mimocode/skills)
func TestSkillInstallDirsPerProviderAcrossScopes(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", t.TempDir())

	// The pack ships a shared "mayor" skill (as the gascity pack does).
	writeSkillSource(t, filepath.Join(cityPath, "skills", "mayor"))

	// A rig under the city tree.
	subdirRig := filepath.Join(cityPath, "rigs", "inside")
	if err := os.MkdirAll(subdirRig, 0o755); err != nil {
		t.Fatal(err)
	}
	// A rig out of the city tree: a sibling temp dir not under cityPath.
	outOfTreeRig := filepath.Join(t.TempDir(), "temp-rig")
	if err := os.MkdirAll(outOfTreeRig, 0o755); err != nil {
		t.Fatal(err)
	}

	canonicalSink := map[string]string{
		"claude":   ".claude/skills",
		"codex":    ".agents/skills",
		"gemini":   ".gemini/skills",
		"opencode": ".opencode/skills",
		"mimocode": ".mimocode/skills",
	}

	providers := map[string]config.ProviderSpec{}
	for name := range canonicalSink {
		providers[name] = config.ProviderSpec{}
	}

	cfg := &config.City{
		PackSkillsDir: filepath.Join(cityPath, "skills"),
		Session:       config.SessionConfig{Provider: "tmux"},
		Providers:     providers,
		Rigs: []config.Rig{
			{Name: "inside", Path: subdirRig},
			{Name: "temp-rig", Path: outOfTreeRig},
		},
	}

	// Fresh-install production path: implicit per-provider agents at city
	// scope and at each rig scope.
	config.InjectImplicitAgents(cfg)
	config.ApplyAgentDefaults(cfg)

	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(cityPath, cfg, &stderr); err != nil {
		t.Fatalf("runStage1SkillMaterialization: %v", err)
	}

	scopes := []struct {
		label string
		root  string
	}{
		{"city", cityPath},
		{"subdir-rig", subdirRig},
		{"out-of-tree-rig", outOfTreeRig},
	}

	wantSource := filepath.Join(cityPath, "skills", "mayor")
	for _, sc := range scopes {
		for provider, sink := range canonicalSink {
			link := filepath.Join(sc.root, filepath.FromSlash(sink), "mayor")
			info, err := os.Lstat(link)
			if err != nil {
				t.Errorf("%s / %s: skill not installed where the CLI reads it: %v (want symlink at %s)",
					sc.label, provider, err, link)
				continue
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s / %s: %s is not a symlink", sc.label, provider, link)
				continue
			}
			// The provider CLI follows the symlink target, so a dangling
			// or mis-targeted link delivers zero skills even though the
			// link exists. Assert it resolves to the shared mayor source.
			tgt, err := os.Readlink(link)
			if err != nil {
				t.Errorf("%s / %s: readlink %s: %v", sc.label, provider, link, err)
				continue
			}
			if tgt != wantSource {
				t.Errorf("%s / %s: symlink target = %q, want %q", sc.label, provider, tgt, wantSource)
			}
		}
	}

	if stderr.Len() > 0 {
		t.Logf("stderr:\n%s", stderr.String())
	}
}

// TestRigAddMaterializesSkillsIntoOutOfTreeRig is the issue #3643 lifecycle
// guard. `gc rig add` must make pack skills available in the new rig's own
// directory immediately — not only after the next supervisor tick — including
// for an out-of-tree rig, so a coding CLI started there resolves the skill
// right away (codex reads `.agents/skills` from cwd up to the repo root, so
// the co-located rig sink is what carries it). This exercises the real
// load+compose+materialize path that `gc rig add` now invokes via
// materializeSkillsForCity, with a rig whose path is outside the city tree.
func TestRigAddMaterializesSkillsIntoOutOfTreeRig(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_HOME", t.TempDir())

	// Out-of-tree rig: a sibling temp dir, not under cityPath.
	outOfTreeRig := filepath.Join(t.TempDir(), "temp-rig")
	if err := os.MkdirAll(outOfTreeRig, 0o755); err != nil {
		t.Fatal(err)
	}

	// Minimal city-as-pack: a shared "mayor" skill, claude+codex providers
	// (so InjectImplicitAgents creates per-rig agents), and the out-of-tree
	// rig registered — the on-disk state `gc rig add` leaves behind (the rig
	// path is a .gc/site.toml binding, not a city.toml rig.path).
	cityTOML := "[workspace]\nname = \"test-city\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"tmux\"\n\n" +
		"[providers.claude]\ncommand = \"claude\"\n\n" +
		"[providers.codex]\ncommand = \"codex\"\n\n" +
		"[[rigs]]\nname = \"temp-rig\"\n"
	writeMaterializeTestCityFile(t, cityPath, "city.toml", cityTOML)
	writeMaterializeTestCityFile(t, cityPath, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeBuiltinImportsFixture(t, cityPath, "core")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMaterializeTestCityFile(t, filepath.Join(cityPath, ".gc"), "site.toml",
		fmt.Sprintf("[[rig]]\nname = \"temp-rig\"\npath = %q\n", outOfTreeRig))
	writeSkillSource(t, filepath.Join(cityPath, "skills", "mayor"))

	// The behavior under test: gc rig add materializes synchronously.
	var stderr bytes.Buffer
	materializeSkillsForCity(cityPath, &stderr)

	for _, sink := range []string{".agents/skills", ".claude/skills"} {
		link := filepath.Join(outOfTreeRig, filepath.FromSlash(sink), "mayor")
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("out-of-tree rig missing skill right after add: %s (%v)", link, err)
		}
	}
	if t.Failed() {
		t.Logf("stderr:\n%s", stderr.String())
	}
}
