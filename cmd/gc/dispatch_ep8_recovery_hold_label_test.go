package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// This file expresses the ga-o48vn0 round-2 acceptance criterion: crash/boot
// recovery (buildOnDeath / buildOnBoot, internal/config/workquery.go) is
// intentionally hold-label-blind -- reopening crashed or ownerless work is
// not a dispatch decision, so those hooks reopen a held bead unconditionally.
// What was never proven end-to-end is the *handoff*: once recovery reopens a
// held bead, a different agent's subsequent route-scoped (Tier 3) hook must
// still exclude it via filterReadyByRoute. Round 1 covered each half in
// isolation (config's lifecycle-hook tests; cmd/gc's
// dispatch_control_ready_hold_label_test.go); this composes both halves.

// runLifecycleHookShellForTest executes a generated on_death/on_boot shell
// command against a fake `bd` stubbed onto PATH. It reuses
// shellWorkQueryWithEnv (cmd_hook.go) -- the same subprocess path production
// hook dispatch already runs work queries through -- instead of a new
// exec.Command literal, so this composition doesn't add a second
// independently-spawned subprocess call site next to the existing one.
func runLifecycleHookShellForTest(t *testing.T, command string, bdScript string) string {
	t.Helper()

	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	if err := os.WriteFile(bdPath, []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	logPath := filepath.Join(tmp, "bd.log")

	env := []string{
		"PATH=" + tmp + ":" + os.Getenv("PATH"),
		"BD_LOG=" + logPath,
	}
	if _, err := shellWorkQueryWithEnv(command, tmp, env); err != nil {
		t.Fatalf("run lifecycle hook: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	return string(data)
}

func TestBuildOnDeathReopensHeldBeadThenRouteScopedHookExcludesIt(t *testing.T) {
	crashed := config.Agent{Name: "builder-1", Dir: "gascity", PoolName: "gascity/builder"}

	log := runLifecycleHookShellForTest(t, crashed.EffectiveOnDeath(), `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '%s\n' "$*" >> "$BD_LOG"
    printf '[{"id":"ga-held-work","type":"task","labels":["hold:mayor"],"metadata":{"gc.run_target":"gascity/builder"}}]'
    ;;
  update)
    printf '%s\n' "$*" >> "$BD_LOG"
    ;;
  *)
    exit 1
    ;;
esac
`)
	if !strings.Contains(log, "update ga-held-work --assignee  --status open") {
		t.Fatalf("on_death hook log = %q, want the hold-labeled bead reopened unconditionally (buildOnDeath is correctly hold-blind: recovery is not a dispatch decision)", log)
	}

	// The crashed session's on_death hook just reopened ga-held-work as
	// open/unassigned, but recovery does not and must not strip labels: it
	// still carries hold:mayor. Model what a DIFFERENT agent's subsequent
	// route-scoped (Tier 3) hook sees when it evaluates ready work.
	readyAfterRecovery := []beads.Bead{
		{ID: "ga-held-work", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/builder"}, Labels: []string{beadmeta.HoldMayorLabel}},
	}
	served := filterReadyByRoute(readyAfterRecovery, beadmeta.RunTargetMetadataKey, "gascity/builder")
	if len(served) != 0 {
		t.Fatalf("filterReadyByRoute after on_death recovery = %v, want empty (a different agent's route-scoped hook must not be served a bead recovery just reopened while it is still held)", beadIDs(served))
	}
}

func TestBuildOnBootReopensHeldBeadThenRouteScopedHookExcludesIt(t *testing.T) {
	rebooted := config.Agent{Name: "builder-1", Dir: "gascity", PoolName: "gascity/builder"}

	log := runLifecycleHookShellForTest(t, rebooted.EffectiveOnBoot(), `#!/bin/sh
set -eu
case "$1" in
  list)
    printf '%s\n' "$*" >> "$BD_LOG"
    case "$*" in
      *"--metadata-field gc.routed_to=gascity/builder"*) printf '[{"id":"ga-held-boot","type":"wisp","labels":["hold:external"],"metadata":{"gc.routed_to":"gascity/builder"}}]' ;;
      *) printf '[]' ;;
    esac
    ;;
  update)
    printf '%s\n' "$*" >> "$BD_LOG"
    ;;
  *)
    exit 1
    ;;
esac
`)
	if !strings.Contains(log, "update ga-held-boot --status open") {
		t.Fatalf("on_boot hook log = %q, want the hold-labeled bead reopened unconditionally (buildOnBoot is correctly hold-blind: recovery is not a dispatch decision)", log)
	}

	// The rebooted session's on_boot hook just reopened ga-held-boot, but it
	// still carries hold:external. Model what a DIFFERENT agent's subsequent
	// route-scoped (Tier 3) hook sees when it evaluates ready work.
	readyAfterRecovery := []beads.Bead{
		{ID: "ga-held-boot", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gascity/builder"}, Labels: []string{beadmeta.HoldExternalLabel}},
	}
	served := filterReadyByRoute(readyAfterRecovery, beadmeta.RoutedToMetadataKey, "gascity/builder")
	if len(served) != 0 {
		t.Fatalf("filterReadyByRoute after on_boot recovery = %v, want empty (a different agent's route-scoped hook must not be served a bead reboot recovery just reopened while it is still held)", beadIDs(served))
	}
}
