package formula

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// A ralph check path written in the documented ../assets/ form must resolve,
// at parse time, to the highest-priority formula layer that ships the script —
// mirroring how description_file assets shadow. This lets a city/local layer
// override a pack's check script without forking the formula.
func TestCheckPathUsesHighestPriorityAssetLayer(t *testing.T) {
	tmp := t.TempDir()
	coreFormulas := filepath.Join(tmp, "core", "formulas")
	coreChecks := filepath.Join(tmp, "core", "assets", "scripts", "checks")
	cityFormulas := filepath.Join(tmp, "city", "formulas")
	cityChecks := filepath.Join(tmp, "city", "assets", "scripts", "checks")
	for _, dir := range []string{coreFormulas, coreChecks, cityFormulas, cityChecks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	formulaText := `
formula = "review"

[[steps]]
id = "gate"
title = "Gate"
[steps.check]
max_attempts = 3
[steps.check.check]
mode = "exec"
path = "../assets/scripts/checks/build-artifact-valid.sh"
`
	if err := os.WriteFile(filepath.Join(coreFormulas, "review.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	// Both layers ship the script; the city (highest-priority) copy must win.
	for _, dir := range []string{coreChecks, cityChecks} {
		if err := os.WriteFile(filepath.Join(dir, "build-artifact-valid.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write check %s: %v", dir, err)
		}
	}

	p := NewParser(coreFormulas, cityFormulas)
	f, err := p.LoadByName("review")
	if err != nil {
		t.Fatalf("LoadByName(review): %v", err)
	}
	want := filepath.Join(cityChecks, "build-artifact-valid.sh")
	got := f.Steps[0].Ralph.Check.Path
	if got != want {
		t.Fatalf("check path = %q, want city asset shadow %q", got, want)
	}
}

// When only a lower-priority layer ships the script, the check path resolves to
// that pack's copy (fallback), exactly like description_file resolution.
func TestCheckPathFallsBackToFormulaPackAsset(t *testing.T) {
	tmp := t.TempDir()
	coreFormulas := filepath.Join(tmp, "core", "formulas")
	coreChecks := filepath.Join(tmp, "core", "assets", "scripts", "checks")
	cityFormulas := filepath.Join(tmp, "city", "formulas")
	for _, dir := range []string{coreFormulas, coreChecks, cityFormulas} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	formulaText := `
formula = "triage"

[[steps]]
id = "gate"
title = "Gate"
[steps.check.check]
mode = "exec"
path = "../assets/scripts/checks/review-approved.sh"
`
	if err := os.WriteFile(filepath.Join(coreFormulas, "triage.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	if err := os.WriteFile(filepath.Join(coreChecks, "review-approved.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write core check: %v", err)
	}

	p := NewParser(coreFormulas, cityFormulas)
	f, err := p.LoadByName("triage")
	if err != nil {
		t.Fatalf("LoadByName(triage): %v", err)
	}
	want := filepath.Join(coreChecks, "review-approved.sh")
	got := f.Steps[0].Ralph.Check.Path
	if got != want {
		t.Fatalf("check path = %q, want core asset fallback %q", got, want)
	}
}

// Non-asset check paths (legacy .gc/ runtime paths, absolute paths) must be
// left exactly as authored — the resolver only rewrites the ../assets/ form.
func TestCheckPathKeepsNonAssetPathsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	formulas := filepath.Join(tmp, "pack", "formulas")
	if err := os.MkdirAll(formulas, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	formulaText := `
formula = "legacy"

[[steps]]
id = "gate-gc"
title = "Legacy runtime path"
[steps.check.check]
mode = "exec"
path = ".gc/scripts/checks/build-artifact-valid.sh"

[[steps]]
id = "gate-abs"
title = "Absolute path"
[steps.check.check]
mode = "exec"
path = "/opt/tooling/checks/foo.sh"
`
	if err := os.WriteFile(filepath.Join(formulas, "legacy.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}

	f, err := NewParser(formulas).LoadByName("legacy")
	if err != nil {
		t.Fatalf("LoadByName(legacy): %v", err)
	}
	if got := f.Steps[0].Ralph.Check.Path; got != ".gc/scripts/checks/build-artifact-valid.sh" {
		t.Fatalf("legacy .gc path rewritten to %q, want unchanged", got)
	}
	if got := f.Steps[1].Ralph.Check.Path; got != "/opt/tooling/checks/foo.sh" {
		t.Fatalf("absolute path rewritten to %q, want unchanged", got)
	}
}

// A check path whose script name still carries a template placeholder is
// substituted later (expand time), so parse-time resolveCheckPaths must leave it
// untouched — even in strict mode — rather than failing on the literal
// placeholder. The placeholder is resolved once expansion substitutes it; see
// TestCompileResolvesExpandedTemplatedAssetCheckPath and the dispatch end-to-end
// TestRunRalphCheckAcceptsExpandedTemplatedAssetPath.
func TestResolveCheckPathsDefersTemplatedAssetPathInStrictMode(t *testing.T) {
	tmp := t.TempDir()
	layer := filepath.Join(tmp, "pack", "formulas")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const templated = "../assets/scripts/checks/{target}.sh"
	steps := []*Step{{
		ID:    "gate",
		Ralph: &RalphSpec{Check: &RalphCheckSpec{Mode: "exec", Path: templated}},
	}}

	p := NewParser(layer)
	if err := p.resolveCheckPaths(steps, layer, true /* strict */); err != nil {
		t.Fatalf("strict resolveCheckPaths errored on templated check path: %v", err)
	}
	if got := steps[0].Ralph.Check.Path; got != templated {
		t.Fatalf("templated check path = %q, want left untouched %q", got, templated)
	}
}

// Strict mode must still fail fast on a genuinely missing (non-templated)
// ../assets check script — surfacing the typo/missing file at cook time.
func TestResolveCheckPathsStrictErrorsOnMissingLiteralAsset(t *testing.T) {
	tmp := t.TempDir()
	layer := filepath.Join(tmp, "pack", "formulas")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	steps := []*Step{{
		ID:    "gate",
		Ralph: &RalphSpec{Check: &RalphCheckSpec{Mode: "exec", Path: "../assets/scripts/checks/missing.sh"}},
	}}

	p := NewParser(layer)
	if err := p.resolveCheckPaths(steps, layer, true /* strict */); err == nil {
		t.Fatalf("expected strict error for missing ../assets check script")
	}
}

// A templated "../assets/..." check path in an expansion formula is deferred at
// parse time, then resolved once Compile materializes the expansion and
// substitutes {target}. The compiled control bead must carry the absolute layer
// asset path in gc.check_path — not the relative "../assets/..." form, which the
// runtime would resolve against the store/work dir and fail to find. This covers
// the compile-time materialization path (Stage 9 MaterializeExpansion).
func TestCompileResolvesExpandedTemplatedAssetCheckPath(t *testing.T) {
	enableV2ForTest(t)
	tmp := t.TempDir()
	packFormulas := filepath.Join(tmp, "pack", "formulas")
	packChecks := filepath.Join(tmp, "pack", "assets", "scripts", "checks")
	for _, dir := range []string{packFormulas, packChecks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Standalone expansion: Compile materializes it with target id "main", so
	// "{target}" substitutes to "main" before the check path is resolved.
	formulaText := `
formula = "gated-expansion"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.gate"
title = "Gate"
[template.ralph]
max_attempts = 3
[template.ralph.check]
mode = "exec"
path = "../assets/scripts/checks/{target}.sh"
`
	if err := os.WriteFile(filepath.Join(packFormulas, "gated-expansion.toml"), []byte(formulaText), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	script := filepath.Join(packChecks, "main.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write check: %v", err)
	}

	recipe, err := Compile(context.Background(), "gated-expansion", []string{packFormulas}, nil)
	if err != nil {
		t.Fatalf("Compile(gated-expansion): %v", err)
	}

	var got string
	for _, step := range recipe.Steps {
		if p := step.Metadata[beadmeta.CheckPathMetadataKey]; p != "" {
			got = p
			break
		}
	}
	if got == "" {
		t.Fatal("no materialized recipe step carried a gc.check_path")
	}
	if got != script {
		t.Fatalf("materialized gc.check_path = %q, want absolute layer asset %q", got, script)
	}
}
