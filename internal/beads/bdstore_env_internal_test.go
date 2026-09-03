package beads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envValues(env []string, key string) []string {
	var out []string
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, strings.TrimPrefix(e, prefix))
		}
	}
	return out
}

func TestExecEnvForBd_InjectsAutoBackupOptOut(t *testing.T) {
	// Every bd subprocess spawned through the runner must carry the
	// auto-backup opt-out (ga-yfbs28): bd's PersistentPostRun backup_export
	// sync has no retention and stuck-loops on broken remote state (the
	// 2026-06-08 town-wide wedge, ga-0eq). The projected-env opt-out only
	// covers gc env projections; this is the choke point for everything
	// else (hook claim, store bridge, t3bridge, libstore, provider
	// lifecycle).
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, nil)
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [false]", vals)
	}
}

func TestExecEnvForBd_OverridesInheritedEnable(t *testing.T) {
	// A BD_BACKUP_ENABLED=true inherited from the parent process must not
	// leak through: gc policy forces the opt-out on gc-managed bd calls,
	// matching applyBdAutoBackupOptOut's unconditional projection.
	base := []string{"PATH=/usr/bin", "BD_BACKUP_ENABLED=true"}
	got := execEnvFor("bd", base, nil)
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [false] (inherited true must be replaced)", vals)
	}
}

func TestExecEnvForBd_ExplicitCallerOverrideWins(t *testing.T) {
	// An explicit per-call override is a deliberate caller decision (e.g. a
	// backup-focused test fixture) and must beat the injected baseline.
	base := []string{"PATH=/usr/bin"}
	got := execEnvFor("bd", base, map[string]string{"BD_BACKUP_ENABLED": "true"})
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want exactly [true] (explicit override wins)", vals)
	}
}

func TestExecEnvForBd_MergesOtherOverrides(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/u"}
	got := execEnvFor("bd", base, map[string]string{"BEADS_DIR": "/x/.beads"})
	if vals := envValues(got, "BEADS_DIR"); len(vals) != 1 || vals[0] != "/x/.beads" {
		t.Errorf("BEADS_DIR values = %v, want [/x/.beads]", vals)
	}
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "false" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want [false] alongside other overrides", vals)
	}
	if vals := envValues(got, "HOME"); len(vals) != 1 || vals[0] != "/home/u" {
		t.Errorf("HOME values = %v, want [/home/u] preserved", vals)
	}
}

func TestExecEnvForNonBd_LeavesEnvAlone(t *testing.T) {
	// The runner also execs dolt directly; non-bd commands keep the
	// caller-visible environment untouched.
	base := []string{"PATH=/usr/bin", "BD_BACKUP_ENABLED=true"}
	got := execEnvFor("dolt", base, nil)
	if vals := envValues(got, "BD_BACKUP_ENABLED"); len(vals) != 1 || vals[0] != "true" {
		t.Errorf("BD_BACKUP_ENABLED values = %v, want [true] untouched for non-bd commands", vals)
	}
}

func TestExecCommandRunnerWithoutAmbientBeadsWithholdsNamespaceBeforeOverrides(t *testing.T) {
	t.Setenv("BEADS_DB", "ambient-database")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/ambient/helper")

	runner := ExecCommandRunnerWithEnvWithoutAmbientBeads(map[string]string{
		"BEADS_DIR":                     "/selected/.beads",
		"BEADS_DOLT_CREDENTIAL_COMMAND": "/selected/gc internal beads-credential",
	})
	out, err := runner(t.TempDir(), "sh", "-c", `env | sort`)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, forbidden := range []string{
		"BEADS_DB=",
		"BEADS_DOLT_SERVER_HOST=",
		"BEADS_DOLT_CREDENTIAL_COMMAND=/ambient/helper",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("child environment contains withheld entry %q", forbidden)
		}
	}
	for _, want := range []string{
		"BEADS_DIR=/selected/.beads",
		"BEADS_DOLT_CREDENTIAL_COMMAND=/selected/gc internal beads-credential",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("child environment does not contain explicit override %q", want)
		}
	}
}

func TestExecCommandRunnerWithEnv_AbsoluteBDBinKeepsLogicalBdPolicy(t *testing.T) {
	// BD_BIN selects the physical executable for an otherwise logical `bd`
	// command. The logical identity must remain intact so the runner keeps the
	// bd-only environment policy rather than treating the pinned path as an
	// unrelated program.
	pinned := filepath.Join(t.TempDir(), "workspace-bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nprintf 'pinned:%s\\n' \"$BD_BACKUP_ENABLED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": pinned})(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run workspace-pinned bd: %v", err)
	}
	if got, want := string(out), "pinned:false\n"; got != want {
		t.Fatalf("workspace-pinned bd output = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithEnv_RelativeBDBinUsesAmbientBd(t *testing.T) {
	ambientDir := t.TempDir()
	ambient := filepath.Join(ambientDir, "bd")
	if err := os.WriteFile(ambient, []byte("#!/bin/sh\nprintf 'ambient:%s\\n' \"$BD_BACKUP_ENABLED\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ambientDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := ExecCommandRunnerWithEnv(map[string]string{"BD_BIN": "workspace-bd"})(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run ambient bd: %v", err)
	}
	if got, want := string(out), "ambient:false\n"; got != want {
		t.Fatalf("ambient bd output = %q, want %q", got, want)
	}
}

func TestExecCommandRunnerWithExactEnvContextExcludesParentValues(t *testing.T) {
	t.Setenv("BEADS_DB", "ambient.db")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "ambient-credential-helper")

	pinned := filepath.Join(t.TempDir(), "workspace-bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nprintf '%s|%s|%s\\n' \"${BEADS_DB-unset}\" \"${BEADS_DOLT_CREDENTIAL_COMMAND-unset}\" \"$BEADS_DIR\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := ExecCommandRunnerWithExactEnvContext(context.Background(), map[string]string{
		"BD_BIN":    pinned,
		"BEADS_DIR": "/selected/.beads",
	})
	out, err := runner(t.TempDir(), "bd")
	if err != nil {
		t.Fatalf("run workspace-pinned bd: %v", err)
	}
	if got, want := string(out), "unset|unset|/selected/.beads\n"; got != want {
		t.Fatalf("exact runner output = %q, want %q", got, want)
	}
}
