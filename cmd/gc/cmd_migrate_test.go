package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoImportMigrateDryRun(t *testing.T) {
	cityDir := t.TempDir()
	writeMigrateTestFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"
includes = ["../packs/gastown"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`)
	writeMigrateTestFile(t, cityDir, "prompts/mayor.md", "hello\n")

	origCityFlag := cityFlag
	t.Cleanup(func() { cityFlag = origCityFlag })
	cityFlag = cityDir

	var stdout, stderr bytes.Buffer
	if code := doImportMigrate(true, &stdout, &stderr); code != 0 {
		t.Fatalf("doImportMigrate(dry-run) = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No side effects executed (--dry-run).") {
		t.Fatalf("stdout missing dry-run footer:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(cityDir, "pack.toml")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create pack.toml, err=%v", err)
	}
}

func TestDoImportMigrateWritesFiles(t *testing.T) {
	cityDir := t.TempDir()
	writeMigrateTestFile(t, cityDir, "city.toml", `
[workspace]
name = "legacy-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
fallback = true
`)
	writeMigrateTestFile(t, cityDir, "prompts/mayor.md", "Hello {{.Agent}}\n")

	origCityFlag := cityFlag
	t.Cleanup(func() { cityFlag = origCityFlag })
	cityFlag = cityDir

	var stdout, stderr bytes.Buffer
	if code := doImportMigrate(false, &stdout, &stderr); code != 0 {
		t.Fatalf("doImportMigrate = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Applied changes for") {
		t.Fatalf("stdout missing applied-changes header:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "warning: dropped fallback field") {
		t.Fatalf("stdout missing fallback warning:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(cityDir, "pack.toml")); err != nil {
		t.Fatalf("expected pack.toml after migrate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "agents", "mayor", "prompt.template.md")); err != nil {
		t.Fatalf("expected migrated prompt file: %v", err)
	}
}

func writeMigrateTestFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimLeft(contents, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
