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
