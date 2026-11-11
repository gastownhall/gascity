package convergence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestConditionEnvEnvironNamesTheControllerAsActor pins ga-rjych4.19: a check
// script's gc and bd writes are attributed through BEADS_ACTOR (gc's
// eventActor reads it when no session identity is set). An environment built
// without it made gc record those writes as actor "human".
func TestConditionEnvEnvironNamesTheControllerAsActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "")

	lookup := environLookup(t, ConditionEnv{BeadID: "b-1", CityPath: "/city"})

	if got := lookup["BEADS_ACTOR"]; got != ConditionActor {
		t.Fatalf("BEADS_ACTOR = %q, want %q", got, ConditionActor)
	}
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID"} {
		if value, ok := lookup[key]; ok {
			t.Errorf("%s = %q: a check script is not an agent session", key, value)
		}
	}
}

// TestConditionEnvEnvironKeepsAmbientBeadsActor matches the controller's own
// bd calls (applyControllerBdEnv): an operator-set BEADS_ACTOR wins.
func TestConditionEnvEnvironKeepsAmbientBeadsActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "ops-controller")

	lookup := environLookup(t, ConditionEnv{BeadID: "b-1", CityPath: "/city"})

	if got := lookup["BEADS_ACTOR"]; got != "ops-controller" {
		t.Fatalf("BEADS_ACTOR = %q, want the ambient ops-controller", got)
	}
}

func TestRunConditionScriptWritesAreNotAttributedToHuman(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "")
	dir := t.TempDir()
	script := filepath.Join(dir, "actor.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"ACTOR=${BEADS_ACTOR:-human}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	result := RunCondition(context.Background(), script, ConditionEnv{BeadID: "b-1", CityPath: dir}, 5*time.Second, 0)

	if result.Outcome != GatePass {
		t.Fatalf("Outcome = %q, want pass; stderr: %s", result.Outcome, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "ACTOR="+ConditionActor) {
		t.Fatalf("check script ran as %q, want ACTOR=%s", strings.TrimSpace(result.Stdout), ConditionActor)
	}
}
