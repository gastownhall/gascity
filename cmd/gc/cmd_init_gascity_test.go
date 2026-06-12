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
