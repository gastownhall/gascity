package main

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCityInitExactOutput_DefaultScaffold(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := doInit(fsys.NewFake(), "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr, false)

	if code != 0 {
		t.Fatalf("doInit code = %d, want 0", code)
	}
	const wantStdout = "[1/8] Creating runtime scaffold\n" +
		"[2/8] Installing hooks (Claude Code)\n" +
		"[3/8] Writing default prompts\n" +
		"[4/8] Writing pack.toml\n" +
		"[5/8] Writing city configuration\n" +
		"Welcome to Gas City!\n" +
		"Initialized city \"bright-lights\" with default mayor agent.\n"
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestCityInitExactOutput_CommandProviderSkipReadiness(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	disableBootstrapForTests(t)

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	var stdout, stderr bytes.Buffer
	code := cmdInitWithOptions([]string{filepath.Join(t.TempDir(), "bright-lights")}, "codex", "", "", &stdout, &stderr, true, false)

	if code != 0 {
		t.Fatalf("cmdInitWithOptions code = %d, want 0", code)
	}
	const wantStdout = "[1/8] Creating runtime scaffold\n" +
		"[2/8] Installing hooks (Claude Code)\n" +
		"[3/8] Writing default prompts\n" +
		"[4/8] Writing pack.toml\n" +
		"[5/8] Writing city configuration\n" +
		"Welcome to Gas City!\n" +
		"Initialized city \"bright-lights\" with default provider \"codex\".\n" +
		"[6/8] Skipping provider readiness checks\n" +
		"[7/8] Registering city with supervisor\n"
	if stdout.String() != wantStdout {
		t.Fatalf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
