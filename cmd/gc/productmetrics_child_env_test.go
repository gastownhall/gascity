package main

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/execenv"
)

func TestProductMetricsChildEnvAdapterCanonicalizesExplicitEnvironment(t *testing.T) {
	cmd := exec.Command("unused")
	cmd.Env = []string{
		"KEEP=first",
		execenv.UsageMetricsDisableEnv + "=0",
		"BD_DISABLE_METRICS=1",
		"OTEL_SERVICE_NAME=gascity-test",
		execenv.UsageMetricsDisableEnv + "=yes",
		"KEEP=second",
	}
	want := []string{
		"KEEP=first",
		"BD_DISABLE_METRICS=1",
		"OTEL_SERVICE_NAME=gascity-test",
		"KEEP=second",
		execenv.UsageMetricsDisabledEntry,
	}

	disableProductMetricsForChild(cmd)
	if !slices.Equal(cmd.Env, want) {
		t.Fatalf("child environment = %#v, want %#v", cmd.Env, want)
	}
	disableProductMetricsForChild(cmd)
	if !slices.Equal(cmd.Env, want) {
		t.Fatalf("child environment after second application = %#v, want %#v", cmd.Env, want)
	}
}

func TestProductMetricsChildEnvAdapterMaterializesInheritedEnvironment(t *testing.T) {
	t.Setenv(execenv.UsageMetricsDisableEnv, "0")
	t.Setenv("BD_DISABLE_METRICS", "keep-beads-setting")
	t.Setenv("OTEL_SERVICE_NAME", "keep-otel-setting")

	cmd := exec.Command("unused")
	if cmd.Env != nil {
		t.Fatalf("new command Env = %#v, want nil inheritance marker", cmd.Env)
	}
	disableProductMetricsForChild(cmd)

	for _, want := range []string{
		execenv.UsageMetricsDisabledEntry,
		"BD_DISABLE_METRICS=keep-beads-setting",
		"OTEL_SERVICE_NAME=keep-otel-setting",
	} {
		if !slices.Contains(cmd.Env, want) {
			t.Fatalf("materialized child environment missing %q", want)
		}
	}
	count := 0
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, execenv.UsageMetricsDisableEnv+"=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("materialized child environment has %d %s entries, want exactly one", count, execenv.UsageMetricsDisableEnv)
	}
}

func TestProductMetricsChildEnvAdapterUsesCmdEnvironForDirPWD(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("os/exec does not synthesize PWD from Cmd.Dir on this platform")
	}
	staleDir := t.TempDir()
	childDir := t.TempDir()
	t.Setenv("PWD", staleDir)

	cmd := exec.Command("unused")
	cmd.Dir = childDir
	disableProductMetricsForChild(cmd)

	pwdEntries := 0
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "PWD=") {
			pwdEntries++
			if entry != "PWD="+childDir {
				t.Fatalf("child PWD entry = %q, want %q", entry, "PWD="+childDir)
			}
		}
	}
	if pwdEntries != 1 {
		t.Fatalf("child environment has %d PWD entries, want exactly one", pwdEntries)
	}
}

func TestProductMetricsChildEnvAdapterPreservesExplicitEmptyEnvironment(t *testing.T) {
	t.Setenv("H1_AMBIENT_SENTINEL", "must-not-be-inherited")
	cmd := exec.Command("unused")
	cmd.Dir = t.TempDir()
	cmd.Env = []string{}

	disableProductMetricsForChild(cmd)

	want := []string{execenv.UsageMetricsDisabledEntry}
	if !slices.Equal(cmd.Env, want) {
		t.Fatalf("explicit-empty child environment = %#v, want %#v", cmd.Env, want)
	}
}
