package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

func TestRetiredBuiltinPackForInclude(t *testing.T) {
	cityPath := "/city"
	for _, tt := range []struct {
		include string
		want    string
	}{
		{include: ".gc/system/packs/maintenance", want: "maintenance"},
		{include: "./.gc/system/packs/maintenance", want: "maintenance"},
		{include: "/city/.gc/system/packs/maintenance", want: "maintenance"},
		{include: ".gc/system/packs/core", want: ""},
		{include: "packs/maintenance", want: ""},
		{include: "", want: ""},
	} {
		if got := retiredBuiltinPackForInclude(cityPath, tt.include); got != tt.want {
			t.Errorf("retiredBuiltinPackForInclude(%q) = %q, want %q", tt.include, got, tt.want)
		}
	}
}

func writeBuiltinIncludeTestCity(t *testing.T, cityToml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuiltinIncludeDoctorCheck_AddsMissingIncludes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinIncludeTestCity(t, "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n")

	check := newBuiltinIncludeDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for missing core include; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "missing-builtin-include | core") {
		t.Fatalf("Run() details = %v, want missing-builtin-include for core", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), ".gc/system/packs/core") {
		t.Fatalf("city.toml after fix missing core include:\n%s", data)
	}

	r = check.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

func TestBuiltinIncludeDoctorCheck_RemovesRetiredMaintenanceInclude(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinIncludeTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/maintenance", ".gc/system/packs/core"]

[beads]
provider = "file"
`)

	check := newBuiltinIncludeDoctorCheck(dir)
	r := check.Run(nil)
	if r.Status != doctor.StatusError {
		t.Fatalf("Run() status = %v, want error for retired maintenance include; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "retired-builtin-include") {
		t.Fatalf("Run() details = %v, want retired-builtin-include entry", r.Details)
	}

	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix(): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), ".gc/system/packs/maintenance") {
		t.Fatalf("city.toml after fix still references retired maintenance pack:\n%s", data)
	}
	if !strings.Contains(string(data), ".gc/system/packs/core") {
		t.Fatalf("city.toml after fix lost core include:\n%s", data)
	}

	r = check.Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() after fix status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

func TestBuiltinIncludeDoctorCheck_OKWithExplicitIncludes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := writeBuiltinIncludeTestCity(t, `[workspace]
name = "demo"
includes = [".gc/system/packs/core"]

[beads]
provider = "file"
`)

	r := newBuiltinIncludeDoctorCheck(dir).Run(nil)
	if r.Status != doctor.StatusOK {
		t.Fatalf("Run() status = %v, want OK; message=%s details=%v", r.Status, r.Message, r.Details)
	}
}

// TestStatusWarnsOnMissingBuiltinIncludes pins the user-visible migration
// warning end to end: a city.toml without the explicit builtin includes must
// surface the once-per-city warning on a real command's stderr, even though
// earlier silent config pre-loads (io.Discard writers) run first in the same
// process and must not consume the warning slot.
func TestStatusWarnsOnMissingBuiltinIncludes(t *testing.T) {
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
	if !strings.Contains(stderr.String(), "does not include required builtin pack(s) core") {
		t.Fatalf("stderr missing builtin-include warning: %q", stderr.String())
	}
}
