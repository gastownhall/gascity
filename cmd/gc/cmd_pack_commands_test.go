package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestRegisterPackCommands_UncachedPacksNoLogNoise(t *testing.T) {
	cityPath := t.TempDir()

	cityTOML := `[workspace]
name = "test"
includes = ["mypk"]

[packs.mypk]
source = "https://example.com/repo.git"
ref = "main"
path = "packs/mypk"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	_, _ = quietLoadCityConfig(cityPath)

	if bytes.Contains(logBuf.Bytes(), []byte("not found, skipping")) {
		t.Fatalf("quietLoadCityConfig produced log noise: %s", logBuf.String())
	}
}

func TestCoreCommandNames(t *testing.T) {
	root := &cobra.Command{Use: "gc"}
	root.AddCommand(&cobra.Command{Use: "start", Aliases: []string{"up"}})
	root.AddCommand(&cobra.Command{Use: "stop"})
	root.AddCommand(&cobra.Command{Use: "doctor"})

	names := coreCommandNames(root)
	for _, want := range []string{"start", "up", "stop", "doctor", "help", "completion"} {
		if !names[want] {
			t.Fatalf("core names missing %q", want)
		}
	}
	if names["nonexistent"] {
		t.Fatal("core names should not contain nonexistent")
	}
}

func TestPackCommandTemplateExpansion(t *testing.T) {
	result := expandScriptTemplate("{{.CityRoot}}/bin/run.sh", "/home/user/city", "mytown", "/packs/p1")
	if result != "/home/user/city/bin/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "/home/user/city/bin/run.sh")
	}
}

func TestPackCommandTemplateExpansionConfigDir(t *testing.T) {
	result := expandScriptTemplate("{{.ConfigDir}}/scripts/run.sh", "/city", "mytown", "/packs/p1")
	if result != "/packs/p1/scripts/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "/packs/p1/scripts/run.sh")
	}
}

func TestPackCommandTemplateNoTemplate(t *testing.T) {
	result := expandScriptTemplate("commands/run.sh", "/city", "mytown", "/packs/p1")
	if result != "commands/run.sh" {
		t.Fatalf("expanded = %q, want %q", result, "commands/run.sh")
	}
}

func TestPackCommandTemplateBadTemplate(t *testing.T) {
	result := expandScriptTemplate("{{.Bad", "/city", "mytown", "/packs/p1")
	if result != "{{.Bad" {
		t.Fatalf("expected graceful fallback, got %q", result)
	}
}

func TestNewRootCmdExposesRootPackCommands(t *testing.T) {
	dir := t.TempDir()
	cityDir := filepath.Join(dir, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, "commands", "hello"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "commands", "hello", "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cityDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	backstage := findSubcommand(root, "backstage")
	if backstage == nil {
		t.Fatal("missing root pack namespace command")
	}
	if findSubcommand(backstage, "hello") == nil {
		t.Fatal("missing root pack hello command")
	}
}
