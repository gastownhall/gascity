package main

import (
	"bytes"
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
