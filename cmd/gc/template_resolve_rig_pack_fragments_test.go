package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestEffectivePackDirsMergesCityAndRig verifies the helper concatenates
// city pack dirs first (lower priority) followed by rig-specific pack dirs
// (higher priority), mirroring the contract of effectiveOverlayDirs.
func TestEffectivePackDirsMergesCityAndRig(t *testing.T) {
	got := effectivePackDirs(
		[]string{"/city-a", "/city-b"},
		map[string][]string{"r1": {"/rig-x"}, "r2": {"/rig-y"}},
		"r1",
	)
	want := []string{"/city-a", "/city-b", "/rig-x"}
	if len(got) != len(want) {
		t.Fatalf("effectivePackDirs len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("effectivePackDirs[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestEffectivePackDirsEmptyRigReturnsCityOnly verifies the helper returns
// the city dirs slice unchanged when the rig has no extra pack imports.
func TestEffectivePackDirsEmptyRigReturnsCityOnly(t *testing.T) {
	city := []string{"/city-a"}
	got := effectivePackDirs(city, map[string][]string{"r1": {"/rig-x"}}, "no-such-rig")
	if len(got) != 1 || got[0] != "/city-a" {
		t.Errorf("effectivePackDirs(no rig match) = %v, want [/city-a]", got)
	}
}

// TestEffectivePackDirsEmptyCityReturnsRigOnly verifies the helper returns
// the rig-specific slice when no city-level packs are configured.
func TestEffectivePackDirsEmptyCityReturnsRigOnly(t *testing.T) {
	got := effectivePackDirs(nil, map[string][]string{"r1": {"/rig-x", "/rig-y"}}, "r1")
	want := []string{"/rig-x", "/rig-y"}
	if len(got) != len(want) {
		t.Fatalf("effectivePackDirs(no city) = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("effectivePackDirs[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestResolveTemplateLoadsRigImportedTemplateFragments is the regression
// test for ga-bbi: a city that imports a pack at the rig level (the dipcity
// shape — see https://github.com/kolloch/gascity beads ga-bbi) must still
// see template-fragments/ from that pack when rendering an agent prompt.
//
// Before the fix, the renderer only loaded fragments from cfg.PackDirs
// (city-level imports). Rig-imported packs ended up in cfg.RigPackDirs and
// were silently skipped, so the agent's prompt body would still contain
// literal `{{ template "foo" . }}` and `{{ .BindingPrefix }}` directives —
// either as raw leaked text (when template execution failed because the
// referenced fragment was undefined) or as un-routed handoff targets.
func TestResolveTemplateLoadsRigImportedTemplateFragments(t *testing.T) {
	cityPath := t.TempDir()
	write := func(rel, data string) {
		path := filepath.Join(cityPath, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	rigRoot := filepath.Join(cityPath, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigRoot): %v", err)
	}

	write("city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "demo"
path = "repos/demo"

[rigs.imports.gastown]
source = "packs/gastown"
`)
	write("packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 2

[[agent]]
name = "polecat"
scope = "rig"
provider = "claude"
prompt_template = "agents/polecat/prompt.template.md"
`)
	// Pack-level template-fragments/. Referenced by the agent prompt below.
	write("packs/gastown/template-fragments/handoff.template.md",
		`{{ define "handoff" }}HANDOFF target={{ .RigName }}/{{ .BindingPrefix }}refinery{{ end }}`)
	write("packs/gastown/agents/polecat/prompt.template.md",
		`Polecat in {{ .RigName }} ({{ .BindingPrefix }}role)
{{ template "handoff" . }}`)

	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Sanity: the rig's import landed in RigPackDirs, not PackDirs.
	if rigDirs := cfg.RigPackDirs["demo"]; len(rigDirs) == 0 {
		t.Fatalf("cfg.RigPackDirs[\"demo\"] is empty; rig import did not populate (got %v)", cfg.RigPackDirs)
	}
	for _, d := range cfg.PackDirs {
		if strings.HasSuffix(d, filepath.Join("packs", "gastown")) {
			t.Fatalf("cfg.PackDirs unexpectedly contains the rig-imported gastown pack (%q) — this test only repros the bug when the pack is rig-scoped", d)
		}
	}

	var agentCfg config.Agent
	for _, a := range cfg.Agents {
		if !a.Implicit && a.Name == "polecat" {
			agentCfg = a
			break
		}
	}
	if agentCfg.Name == "" {
		t.Fatalf("expected polecat agent from rig import; got agents %v", cfg.Agents)
	}

	var stderr bytes.Buffer
	params := &agentBuildParams{
		city:            cfg,
		fs:              fsys.OSFS{},
		cityName:        "test",
		cityPath:        cityPath,
		workspace:       &cfg.Workspace,
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/claude", nil },
		rigs:            cfg.Rigs,
		beaconTime:      testBeaconTime,
		packDirs:        cfg.PackDirs,
		rigPackDirs:     cfg.RigPackDirs,
		globalFragments: cfg.Workspace.GlobalFragments,
		appendFragments: mergeFragmentLists(cfg.AgentDefaults.AppendFragments, cfg.AgentsDefaults.AppendFragments),
		beadNames:       make(map[string]string),
		stderr:          &stderr,
	}

	tp, err := resolveTemplate(params, &agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	for _, leak := range []string{
		`{{ template "handoff"`,
		`{{ .BindingPrefix }}`,
		`{{ .RigName }}`,
	} {
		if strings.Contains(tp.Prompt, leak) {
			t.Errorf("rendered prompt still contains literal %q — template-fragments were not loaded.\nstderr=%q\nprompt=%q",
				leak, stderr.String(), tp.Prompt)
		}
	}

	for _, want := range []string{
		"Polecat in demo (gastown.role)",
		"HANDOFF target=demo/gastown.refinery",
	} {
		if !strings.Contains(tp.Prompt, want) {
			t.Errorf("rendered prompt missing %q.\nstderr=%q\nprompt=%q", want, stderr.String(), tp.Prompt)
		}
	}
}

// TestDoPrime_LoadsRigImportedTemplateFragments is the companion regression
// test for ga-bbi covering the `gc prime` codepath. `gc prime` is invoked
// by the SessionStart hook each time an agent session boots — a different
// rendering call site from resolveTemplate, so both need explicit coverage.
func TestDoPrime_LoadsRigImportedTemplateFragments(t *testing.T) {
	cityDir := t.TempDir()
	rigRoot := filepath.Join(cityDir, "repos", "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(rigRoot): %v", err)
	}
	write := func(rel, data string) {
		path := filepath.Join(cityDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", path, err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	write("city.toml", `
[workspace]
name = "test"

[[rigs]]
name = "demo"
path = "repos/demo"

[rigs.imports.gastown]
source = "packs/gastown"
`)
	write("packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 2

[[agent]]
name = "polecat"
scope = "rig"
provider = "claude"
prompt_template = "agents/polecat/prompt.template.md"
`)
	write("packs/gastown/template-fragments/handoff.template.md",
		`{{ define "handoff" }}HANDOFF target={{ .RigName }}/{{ .BindingPrefix }}refinery{{ end }}`)
	write("packs/gastown/agents/polecat/prompt.template.md",
		`Polecat in {{ .RigName }} ({{ .BindingPrefix }}role)
{{ template "handoff" . }}`)

	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_RIG", "demo")
	t.Setenv("GC_RIG_ROOT", rigRoot)
	t.Setenv("GC_DIR", rigRoot)
	t.Setenv("GC_ALIAS", "demo/gastown.polecat")
	t.Setenv("GC_AGENT", "demo/gastown.polecat")
	t.Setenv("GC_BRANCH", "")

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"demo/gastown.polecat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime() = %d, want 0; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, leak := range []string{
		`{{ template "handoff"`,
		`{{ .BindingPrefix }}`,
		`{{ .RigName }}`,
	} {
		if strings.Contains(out, leak) {
			t.Errorf("rendered prompt still contains literal %q — rig-import template-fragments were not loaded.\nstderr=%q\nstdout=%q",
				leak, stderr.String(), out)
		}
	}
	for _, want := range []string{
		"Polecat in demo (gastown.role)",
		"HANDOFF target=demo/gastown.refinery",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered prompt missing %q.\nstderr=%q\nstdout=%q", want, stderr.String(), out)
		}
	}
}

