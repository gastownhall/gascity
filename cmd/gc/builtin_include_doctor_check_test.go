package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestLegacySystemPacksInclude(t *testing.T) {
	cityPath := "/city"
	for _, tt := range []struct {
		include string
		want    bool
	}{
		{include: ".gc/system/packs/core", want: true},
		{include: ".gc/system/packs/maintenance", want: true},
		{include: "./.gc/system/packs/bd", want: true},
		{include: "/city/.gc/system/packs/core", want: true},
		{include: "packs/maintenance", want: false},
		{include: "rigs/demo/pack", want: false},
		{include: "", want: false},
	} {
		if got := legacySystemPacksInclude(cityPath, tt.include); got != tt.want {
			t.Errorf("legacySystemPacksInclude(%q) = %v, want %v", tt.include, got, tt.want)
		}
	}
}

func writeBuiltinImportTestCity(t *testing.T, cityToml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuiltinImportDoctorCheck_AddsMissingImports(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n")

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for missing core import; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "missing-builtin-import | core") {
		t.Fatalf("Run() details = %v, want missing-builtin-import for core", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if !strings.Contains(string(packData), "[imports.core]") {
		t.Fatalf("pack.toml after fix missing [imports.core]:\n%s", packData)
	}
	lockData, err := os.ReadFile(filepath.Join(dir, "packs.lock"))
	if err != nil {
		t.Fatalf("packs.lock after fix: %v", err)
	}
	if !strings.Contains(string(lockData), "commit") {
		t.Fatalf("packs.lock after fix has no entries:\n%s", lockData)
	}

	r = check.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

func TestBuiltinImportDoctorCheck_MigratesLegacySystemPacksIncludes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinImportTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/maintenance", ".gc/system/packs/core", "rigs/demo/pack"]

[beads]
provider = "file"
`)
	if err := os.MkdirAll(filepath.Join(dir, "rigs", "demo", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rigs", "demo", "pack", "pack.toml"), []byte("[pack]\nname = \"demo-pack\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	check := newBuiltinImportDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for legacy includes; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "legacy-system-packs-include") {
		t.Fatalf("Run() details = %v, want legacy-system-packs-include entry", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	cityData, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cityData), ".gc/system/packs") {
		t.Fatalf("city.toml after fix still references .gc/system/packs:\n%s", cityData)
	}
	if !strings.Contains(string(cityData), "rigs/demo/pack") {
		t.Fatalf("city.toml after fix lost the non-builtin include:\n%s", cityData)
	}
	packData, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("pack.toml after fix: %v", err)
	}
	if !strings.Contains(string(packData), "[imports.core]") {
		t.Fatalf("pack.toml after fix missing [imports.core]:\n%s", packData)
	}

	r = check.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

func TestBuiltinImportDoctorCheck_OKAfterInit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc init = %d; stderr=%s", code, stderr.String())
	}
	r := newBuiltinImportDoctorCheck(dir).Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestStatusWarnsOnMissingBuiltinImports pins the user-visible migration
// warning end to end: a city.toml without the builtin imports must surface
// the once-per-city warning on a real command's stderr, even though earlier
// silent config pre-loads (io.Discard writers) run first in the same process
// and must not consume the warning slot.
func TestStatusWarnsOnMissingBuiltinImports(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte("workspace_name = \"legacy\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", dir, "status"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc status = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not import required builtin pack(s) core") {
		t.Fatalf("stderr missing builtin-import warning: %q", stderr.String())
	}
}
