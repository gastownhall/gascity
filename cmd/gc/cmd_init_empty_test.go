package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestNormalizeInitTemplateAcceptsEmpty pins that the front-door "empty"
// template is a recognized non-interactive template. The crucible controller
// entrypoint maps GC_PACK=empty to `gc init --template empty`; before this the
// normalizer rejected it with "unknown template".
func TestNormalizeInitTemplateAcceptsEmpty(t *testing.T) {
	got, err := normalizeInitTemplate("empty", true)
	if err != nil {
		t.Fatalf("normalizeInitTemplate(empty, true): %v", err)
	}
	if got != "empty" {
		t.Fatalf("normalizeInitTemplate(empty, true) = %q, want empty", got)
	}
}

// TestInitEmptyTemplateNoProviderRequired pins that --template empty does not
// require --default-provider (unlike minimal/gastown/gascity): an empty city is
// bare-and-bootable with no bundled roles, and gets its pack installed later via
// the front-door pack API.
func TestInitEmptyTemplateNoProviderRequired(t *testing.T) {
	cmd := newInitCmd(bytesDiscard(), bytesDiscard())
	if err := cmd.Flags().Set("template", "empty"); err != nil {
		t.Fatalf("set --template empty: %v", err)
	}
	wiz, mode, err := initWizardConfigFromFlags(cmd, "", "", nil, "empty", "", hostedDoltInitOptions{})
	if err != nil {
		t.Fatalf("initWizardConfigFromFlags(--template empty): %v", err)
	}
	if wiz.configName != "empty" {
		t.Fatalf("configName = %q, want empty", wiz.configName)
	}
	if mode != "template" {
		t.Fatalf("mode = %q, want template", mode)
	}
	if wizardDefaultProvider(wiz) != "" {
		t.Fatalf("empty template should carry no provider, got %q", wizardDefaultProvider(wiz))
	}
}

// TestInitEmptyTemplateRejectsProviderFlags pins that --template empty, like
// --template custom, cannot be combined with provider flags: an empty city has
// no agents to bind a provider to.
func TestInitEmptyTemplateRejectsProviderFlags(t *testing.T) {
	cmd := newInitCmd(bytesDiscard(), bytesDiscard())
	if err := cmd.Flags().Set("template", "empty"); err != nil {
		t.Fatalf("set --template empty: %v", err)
	}
	if err := cmd.Flags().Set("default-provider", "claude"); err != nil {
		t.Fatalf("set --default-provider: %v", err)
	}
	_, _, err := initWizardConfigFromFlags(cmd, "", "claude", []string{"claude"}, "empty", "", hostedDoltInitOptions{})
	if err == nil {
		t.Fatal("initWizardConfigFromFlags(--template empty --default-provider) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q should name the empty template", err.Error())
	}
}

// TestDoInitEmptyTemplateScaffoldsBareBootableCity pins the shape of a city
// scaffolded with the empty template:
//   - city.toml declares NO agents and NO [imports] (no bundled roles/formulas)
//   - pack.toml declares NO role/behavior packs (no gastown, no gascity)
//   - pack.toml still pins the "core" infra import, which ships the
//     control-dispatcher pool that runs the formula-v2 dispatcher — the engine
//     a controller needs to `gc start` and drain control beads without a pack.
//   - the written city.toml re-parses (a proxy for "config loads / boots").
func TestDoInitEmptyTemplateScaffoldsBareBootableCity(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wizardConfig{configName: "empty"}, "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	cityData := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cityCfg, err := config.Parse(cityData)
	if err != nil {
		t.Fatalf("parsing city.toml: %v", err)
	}
	if len(cityCfg.Agents) != 0 {
		t.Fatalf("empty city.toml should declare no agents, got %d:\n%s", len(cityCfg.Agents), cityData)
	}
	if len(cityCfg.NamedSessions) != 0 {
		t.Fatalf("empty city.toml should declare no named sessions, got %d:\n%s", len(cityCfg.NamedSessions), cityData)
	}
	if len(cityCfg.Imports) != 0 {
		t.Fatalf("empty city.toml should declare no imports, got %v:\n%s", cityCfg.Imports, cityData)
	}
	if len(cityCfg.Defaults.Rig.Imports) != 0 {
		t.Fatalf("empty city.toml should seed no default rig imports, got %v:\n%s", cityCfg.Defaults.Rig.Imports, cityData)
	}

	packData := f.Files[filepath.Join("/bright-lights", "pack.toml")]
	packCfg, err := config.Parse(packData)
	if err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}
	if len(packCfg.Agents) != 0 {
		t.Fatalf("empty pack.toml should declare no agents, got %d:\n%s", len(packCfg.Agents), packData)
	}
	// A bare empty city seeds no bundled role/session: the pack API installs
	// those later. A mayor named_session here would mean empty fell through to
	// the default (mayor) template instead of the bare EmptyCity shape.
	if len(packCfg.NamedSessions) != 0 {
		t.Fatalf("empty pack.toml should declare no named sessions, got %d:\n%s", len(packCfg.NamedSessions), packData)
	}
	for _, banned := range []string{"gastown", "gascity"} {
		if _, ok := packCfg.Imports[banned]; ok {
			t.Fatalf("empty pack.toml must not import role pack %q:\n%s", banned, packData)
		}
	}
	if _, ok := packCfg.Imports["core"]; !ok {
		t.Fatalf("empty pack.toml must pin the core infra import (control-dispatcher pool):\n%s", packData)
	}

	// No bundled agent prompt scaffolds: empty declares no agents, so init
	// must not materialize a mayor prompt the way the default template does.
	if _, ok := f.Files[filepath.Join("/bright-lights", "agents", "mayor", "prompt.template.md")]; ok {
		t.Fatalf("empty template must not scaffold a mayor prompt")
	}
}

// bytesDiscard returns a throwaway writer for command construction in tests.
func bytesDiscard() *bytes.Buffer { return &bytes.Buffer{} }
