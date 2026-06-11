package main

import (
	"bytes"
	"path/filepath"
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
