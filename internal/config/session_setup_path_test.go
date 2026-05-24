package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSessionSetupScriptPath_RelativeUsesSourceDir(t *testing.T) {
	got := ResolveSessionSetupScriptPath("/home/user/city", "/home/user/city/packs/gastown", "scripts/setup.sh")
	if got != "/home/user/city/packs/gastown/scripts/setup.sh" {
		t.Errorf("got %q, want source-dir-relative absolute path", got)
	}
}

func TestResolveSessionSetupScriptPath_DoubleSlashUsesCityRoot(t *testing.T) {
	got := ResolveSessionSetupScriptPath("/home/user/city", "/home/user/city/packs/gastown", "//scripts/setup.sh")
	if got != "/home/user/city/scripts/setup.sh" {
		t.Errorf("got %q, want city-root path", got)
	}
}

func TestResolveSessionSetupScriptPath_LegacyCityRelativeStillWorks(t *testing.T) {
	got := ResolveSessionSetupScriptPath("/home/user/city", "/home/user/city/packs/gastown", "packs/gastown/scripts/setup.sh")
	if got != "/home/user/city/packs/gastown/scripts/setup.sh" {
		t.Errorf("got %q, want legacy city-root-relative path to remain supported", got)
	}
}

func TestResolveSessionSetupScriptPath_LegacySharedCityRelativeFallback(t *testing.T) {
	cityPath := t.TempDir()
	sourceDir := filepath.Join(cityPath, "packs", "feature")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityScript := filepath.Join(cityPath, "packs", "shared", "scripts", "setup.sh")
	if err := os.MkdirAll(filepath.Dir(cityScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cityScript, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := ResolveSessionSetupScriptPath(cityPath, sourceDir, "packs/shared/scripts/setup.sh")
	if got != cityScript {
		t.Errorf("got %q, want legacy shared city-root-relative path to remain supported", got)
	}
}

func TestResolveSessionSetupScriptPath_Absolute(t *testing.T) {
	got := ResolveSessionSetupScriptPath("/home/user/city", "/home/user/city/packs/gastown", "/usr/local/bin/setup.sh")
	if got != "/usr/local/bin/setup.sh" {
		t.Errorf("got %q, want unchanged absolute path", got)
	}
}

func TestResolveSessionSetupScriptPath_Empty(t *testing.T) {
	got := ResolveSessionSetupScriptPath("/home/user/city", "/home/user/city/packs/gastown", "")
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
