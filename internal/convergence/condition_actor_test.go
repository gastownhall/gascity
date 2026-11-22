package convergence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/execenv"
)

// TestConditionEnvEnvironDefaultsBeadsActorToConditionActor pins
// ga-rjych4.19: a check script's gc and bd writes are attributed through
// BEADS_ACTOR (gc's eventActor reads it when no session identity is set). An
// environment built without it made gc record those writes as actor "human".
// A process that sets no BEADS_ACTOR, such as a standalone controller, gives
// the script ConditionActor.
func TestConditionEnvEnvironDefaultsBeadsActorToConditionActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", "")

	lookup := environLookup(t, ConditionEnv{BeadID: "b-1", CityPath: "/city"})

	if got := lookup["BEADS_ACTOR"]; got != ConditionActor {
		t.Fatalf("BEADS_ACTOR = %q, want %q", got, ConditionActor)
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

// TestConditionEnvEnvironRunsASessionHostedScriptAsTheSessionsActor builds
// the environment of a process inside a managed session, as the control
// dispatcher runs ralph checks: every session-scoped identity key is set, and
// GC_ALIAS, GC_AGENT and BEADS_ACTOR name the session, as the session
// lifecycle sets them. The script runs as the session's BEADS_ACTOR and gets
// none of the other keys, so gc attributes its writes through BEADS_ACTOR
// alone and the script is not handed the session's id, alias or tokens.
func TestConditionEnvEnvironRunsASessionHostedScriptAsTheSessionsActor(t *testing.T) {
	const identity = "rig/core.control-dispatcher"
	for _, key := range execenv.SessionScopedEnvKeys {
		t.Setenv(key, "session-value-of-"+key)
	}
	for _, key := range []string{"GC_ALIAS", "GC_AGENT", "BEADS_ACTOR"} {
		t.Setenv(key, identity)
	}

	lookup := environLookup(t, ConditionEnv{BeadID: "b-1", CityPath: "/city"})

	if got := lookup["BEADS_ACTOR"]; got != identity {
		t.Fatalf("BEADS_ACTOR = %q, want the host session's %q", got, identity)
	}
	for _, key := range execenv.SessionScopedEnvKeys {
		if key == "BEADS_ACTOR" {
			continue
		}
		if value, ok := lookup[key]; ok {
			t.Errorf("%s = %q: a check script gets the host session's BEADS_ACTOR and no other session identity", key, value)
		}
	}
}

// TestRunConditionScriptWritesAreNotAttributedToHuman runs a real script
// through RunCondition from a process that sets no BEADS_ACTOR: the script
// sees ConditionActor, not the "human" that gc falls back to without one.
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
