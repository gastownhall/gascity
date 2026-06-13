package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDefaultWizardConfigUsesGascityTemplate(t *testing.T) {
	wiz := defaultWizardConfig()
	if wiz.configName != "gascity" {
		t.Fatalf("defaultWizardConfig().configName = %q, want gascity", wiz.configName)
	}
}

func TestNormalizeInitTemplateDefaultUsesGascity(t *testing.T) {
	got, err := normalizeInitTemplate("", false)
	if err != nil {
		t.Fatalf("normalizeInitTemplate(empty, false): %v", err)
	}
	if got != "gascity" {
		t.Fatalf("normalizeInitTemplate(empty, false) = %q, want gascity", got)
	}
}

func TestInitWizardConfigProviderFlagDefaultsToGascity(t *testing.T) {
	wiz, err := initWizardConfig("codex", "")
	if err != nil {
		t.Fatalf("initWizardConfig: %v", err)
	}
	if wiz.configName != "gascity" {
		t.Fatalf("initWizardConfig provider default configName = %q, want gascity", wiz.configName)
	}
	if wiz.defaultProvider != "codex" {
		t.Fatalf("initWizardConfig defaultProvider = %q, want codex", wiz.defaultProvider)
	}
}

func TestRunWizardBlankTemplateChoiceUsesGascity(t *testing.T) {
	stubWizardProviderReadiness(t, "claude")
	stdin := strings.NewReader("\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.configName != "gascity" {
		t.Fatalf("runWizard(blank template).configName = %q, want gascity", wiz.configName)
	}
	if wiz.defaultProvider != "claude" {
		t.Fatalf("runWizard(blank template).defaultProvider = %q, want claude", wiz.defaultProvider)
	}
	out := stdout.String()
	if !strings.Contains(out, "gascity") || !strings.Contains(out, "(default)") {
		t.Fatalf("wizard output should advertise gascity as default:\n%s", out)
	}
}

func TestDoInitDefaultTemplateImportsGascityPack(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	packData := f.Files[filepath.Join("/bright-lights", "pack.toml")]
	packCfg, err := config.Parse(packData)
	if err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}
	if _, ok := packCfg.Imports["gascity"]; !ok {
		t.Fatalf("default pack.toml imports = %v, want gascity entry:\n%s", packCfg.Imports, packData)
	}
}

func TestDoInitExplicitMinimalTemplateDoesNotImportGascityPack(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wizardConfig{configName: "minimal"}, "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	packData := f.Files[filepath.Join("/bright-lights", "pack.toml")]
	packCfg, err := config.Parse(packData)
	if err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}
	if _, ok := packCfg.Imports["gascity"]; ok {
		t.Fatalf("explicit minimal pack.toml imports gascity unexpectedly:\n%s", packData)
	}
}

// TestDoInitWithGascityTemplate pins the gascity wizard template: a minimal
// mayor city whose pack.toml imports the public gascity skills pack pinned
// to the registry release, written alongside the explicit builtin includes.
func TestDoInitWithGascityTemplate(t *testing.T) {
	f := fsys.NewFake()

	wiz := defaultWizardConfig()
	wiz.configName = "gascity"
	wiz.provider = "claude"
	wiz.providers = []string{"claude"}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wiz, "", &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	packData := f.Files[filepath.Join("/bright-lights", "pack.toml")]
	packCfg, err := config.Parse(packData)
	if err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}
	imp, ok := packCfg.Imports["gascity"]
	if !ok {
		t.Fatalf("pack.toml imports = %v, want gascity entry:\n%s", packCfg.Imports, packData)
	}
	if imp.Source != config.PublicGascityPackSource {
		t.Errorf("gascity import source = %q, want %q", imp.Source, config.PublicGascityPackSource)
	}
	if imp.Version != config.PublicGascityPackVersion {
		t.Errorf("gascity import version = %q, want %q", imp.Version, config.PublicGascityPackVersion)
	}
}

// TestInitTemplateHelpAndErrorAdvertiseAcceptedTemplates keeps the public
// --template flag help and the unknown-template error synchronized with the
// set normalizeInitTemplate accepts. gascity regressed here once: the parser
// accepted it but both strings omitted it, making it undiscoverable from the
// command contract.
func TestInitTemplateHelpAndErrorAdvertiseAcceptedTemplates(t *testing.T) {
	accepted := []string{"minimal", "gastown", "gascity", "custom"}

	// Every advertised template round-trips through the normalizer.
	for _, tmpl := range accepted {
		got, err := normalizeInitTemplate(tmpl, true)
		if err != nil {
			t.Errorf("normalizeInitTemplate(%q, true) error = %v, want nil", tmpl, err)
		}
		if got != tmpl {
			t.Errorf("normalizeInitTemplate(%q, true) = %q, want %q", tmpl, got, tmpl)
		}
	}

	// The --template flag help advertises every accepted template.
	flag := newInitCmd(io.Discard, io.Discard).Flags().Lookup("template")
	if flag == nil {
		t.Fatal("init command has no --template flag")
	}
	for _, tmpl := range accepted {
		if !strings.Contains(flag.Usage, tmpl) {
			t.Errorf("--template flag help %q missing accepted template %q", flag.Usage, tmpl)
		}
	}

	// The unknown-template error advertises every accepted template.
	_, err := normalizeInitTemplate("definitely-not-a-template", true)
	if err == nil {
		t.Fatal("normalizeInitTemplate(unknown, true) = nil error, want unknown-template error")
	}
	for _, tmpl := range accepted {
		if !strings.Contains(err.Error(), tmpl) {
			t.Errorf("unknown-template error %q missing accepted template %q", err.Error(), tmpl)
		}
	}
}
