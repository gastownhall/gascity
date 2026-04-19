package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoBeadsHealth_FileProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Beads provider: healthy") {
		t.Errorf("should show healthy message: %s", stdout.String())
	}
}

func TestDoBeadsHealth_FileProviderQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(true, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("quiet mode should produce no stdout, got: %s", stdout.String())
	}
}

func TestDoBeadsHealth_ExecProviderHealthy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := writeTestScript(t, "", 0, "")
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "exec:"+script)

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Beads provider: healthy") {
		t.Errorf("should show healthy message: %s", stdout.String())
	}
}

func TestDoBeadsHealth_ExecProviderUnhealthy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Script always fails → health and recover both fail.
	script := writeTestScript(t, "", 1, "server down")
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "exec:"+script)

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "recovery failed") {
		t.Errorf("stderr should mention recovery failure: %s", stderr.String())
	}
}

func TestDoBeadsHealth_BdSkip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	MaterializeBuiltinPacks(dir) //nolint:errcheck
	cityFlag = dir
	defer func() { cityFlag = "" }()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")

	var stdout, stderr bytes.Buffer
	code := doBeadsHealth(false, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d, want 0; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Beads provider: healthy") {
		t.Errorf("GC_DOLT=skip should pass: %s", stdout.String())
	}
}
