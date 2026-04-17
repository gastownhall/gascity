package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestDoRegister(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"my-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Registered city") {
		t.Errorf("expected registration message, got: %s", stdout.String())
	}

	// Verify it's in the registry.
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	// Registry.Register resolves symlinks (e.g. /var → /private/var on macOS).
	resolvedCityPath, _ := filepath.EvalSymlinks(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Errorf("expected 1 entry at %s, got %v", resolvedCityPath, entries)
	}
}

func TestDoRegisterWithNameOverrideRewritesWorkspaceName(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"workspace-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := "[pack]\nname = \"pack-name\"\nschema = 2\n"
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegisterWithOptions([]string{cityPath}, "machine-alias", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "machine-alias") {
		t.Fatalf("stdout = %q, want machine-local alias", stdout.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "machine-alias" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "machine-alias")
	}

	gotCityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotCityToml), `name = "machine-alias"`) {
		t.Fatalf("city.toml should persist the registered name, got:\n%s", string(gotCityToml))
	}
	gotPackToml, err := os.ReadFile(filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPackToml) != packToml {
		t.Fatalf("pack.toml changed during register --name:\n%s", string(gotPackToml))
	}
}

func TestDoRegisterWithoutNameStillUsesWorkspaceName(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"workspace-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "workspace-name" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "workspace-name")
	}
}

func TestDoRegisterWithoutNameFallsBackToPackNameAndPersistsWorkspaceName(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "pack-name" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "pack-name")
	}

	gotCityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotCityToml), `name = "pack-name"`) {
		t.Fatalf("city.toml should persist pack.name fallback, got:\n%s", string(gotCityToml))
	}
}

func TestDoRegisterWithoutNameErrorsWhenWorkspaceAndPackNameMissing(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "missing [pack].name") {
		t.Fatalf("stderr = %q, want missing [pack].name", stderr.String())
	}
}

func TestDoRegisterNotCity(t *testing.T) {
	dir := t.TempDir()
	notCity := filepath.Join(dir, "not-a-city")
	if err := os.MkdirAll(notCity, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{notCity}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not a city directory") {
		t.Errorf("expected 'not a city directory' error, got: %s", stderr.String())
	}
}

func TestDoUnregister(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	// Register first.
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doUnregister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after unregister, got %d", len(entries))
	}
}

func TestDoCities(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	// Empty list.
	var stdout, stderr bytes.Buffer
	code := doCities(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No cities registered") {
		t.Errorf("expected empty message, got: %s", stdout.String())
	}

	// Register a city and list again.
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code = doCities(&stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "bright-lights") {
		t.Errorf("expected 'bright-lights' in output, got: %s", stdout.String())
	}
}
