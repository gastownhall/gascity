package beads

import (
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
