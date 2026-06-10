package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestInstallBeadHooksRemovesExistingHooks verifies that installBeadHooks
// removes any existing on_create/on_update/on_close hooks, cleaning up
// deployments that were installed by an older gc binary.
func TestInstallBeadHooksRemovesExistingHooks(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range beadEventHookNames {
		p := filepath.Join(hooksDir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := installBeadHooks(dir, ""); err != nil {
		t.Fatalf("installBeadHooks: %v", err)
	}

	for _, name := range beadEventHookNames {
		if _, err := os.Stat(filepath.Join(hooksDir, name)); !os.IsNotExist(err) {
			t.Errorf("hook %s should be removed after installBeadHooks (stat err=%v)", name, err)
		}
	}
}

// TestInstallBeadHooksLeavesNonGCHooks verifies that non-gc hooks (e.g., git
// pre-commit) are not removed when installBeadHooks runs.
func TestInstallBeadHooksLeavesNonGCHooks(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".beads", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitHook := filepath.Join(hooksDir, "pre-commit")
	if err := os.WriteFile(gitHook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installBeadHooks(dir, ""); err != nil {
		t.Fatalf("installBeadHooks: %v", err)
	}

	if _, err := os.Stat(gitHook); err != nil {
		t.Errorf("non-gc hook pre-commit must be left untouched: %v", err)
	}
}

// TestInstallBeadHooksIdempotentNoHooks verifies that installBeadHooks
// succeeds when no hooks exist (no directory, no files).
func TestInstallBeadHooksIdempotentNoHooks(t *testing.T) {
	dir := t.TempDir()
	if err := installBeadHooks(dir, ""); err != nil {
		t.Fatalf("installBeadHooks on empty dir: %v", err)
	}
	if err := installBeadHooks(dir, ""); err != nil {
		t.Fatalf("second installBeadHooks: %v", err)
	}
}

// TestInstallBeadHooksInitIntegration verifies that gc init does NOT install
// bd event-forwarding hooks; autoclose now runs in the controller.
func TestInstallBeadHooksInitIntegration(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--skip-provider-readiness", "--provider", "claude", cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc init = %d; stderr: %s", code, stderr.String())
	}

	for _, name := range beadEventHookNames {
		hookPath := filepath.Join(cityPath, ".beads", "hooks", name)
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Errorf("gc init must not install bd event hook %s (stat err=%v)", name, err)
		}
	}
}

// TestInstallBeadHooksRigAddIntegration verifies that gc rig add does NOT
// install bd event-forwarding hooks.
func TestInstallBeadHooksRigAddIntegration(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_SESSION", "fake")

	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "rig", "add", rigPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc rig add = %d; stderr: %s", code, stderr.String())
	}

	for _, name := range beadEventHookNames {
		hookPath := filepath.Join(rigPath, ".beads", "hooks", name)
		if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
			t.Errorf("gc rig add must not install bd event hook %s (stat err=%v)", name, err)
		}
	}
}
