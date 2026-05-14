package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestReconcileSessionBeads_DeadActiveSessionGetsFreshStart verifies that when
// a named-always session was previously active (state=active, started_config_hash
// set) and its runtime died, the reconciler clears started_config_hash so the
// next start is fresh. This ensures provider SessionStart hooks fire (e.g.
// Claude's matcher:"startup" that runs gc prime).
//
// The lifecycle projection (shouldResetContinuation) detects dead active
// sessions and sets ResetContinuation=true, which healStatePatch consumes to
// clear the hash. Our reconciler-level fix provides an accelerated same-tick
// path. Either way, after reconciliation the old session key must not survive
// so the wake uses --session-id (fresh) not --resume.
func TestReconcileSessionBeads_DeadActiveSessionGetsFreshStart(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Providers:     map[string]config.ProviderSpec{"test-provider": {Command: "test-cmd", ProcessNames: []string{"agent-cli"}}},
		Agents:        []config.Agent{{Name: "worker", Provider: "test-provider", StartCommand: "test-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "test-cmd",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
		Hints:                   agent.StartupHints{ProcessNames: []string{"agent-cli"}},
	}

	session := env.createSessionBead(sessionName, "worker")
	startedHash := runtime.CoreFingerprint(runtime.Config{Command: "test-cmd", ProcessNames: []string{"agent-cli"}})
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"last_woke_at":               env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
		"session_key":                "old-conversation-key",
		"started_config_hash":        startedHash,
	})
	// Runtime is NOT started — session is dead but metadata still says active.

	env.reconcile([]beads.Bead{session})
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after first reconcile: %v", session.ID, err)
	}

	// The old conversation key must not survive — dead active sessions must
	// get a fresh conversation start so SessionStart hooks fire.
	if got.Metadata["session_key"] == "old-conversation-key" {
		t.Fatal("session_key = old-conversation-key, want rotated or cleared — dead active session must get fresh start")
	}

	// The session may need a second reconcile tick to wake (healState
	// transitions state to asleep on the first pass, then the wake loop
	// picks it up on the next tick).
	woken := env.reconcile([]beads.Bead{got})

	if woken < 1 || !env.sp.IsRunning(sessionName) {
		t.Fatalf("session should be running after reconcile wakes dead active session (woken=%d, running=%v)", woken, env.sp.IsRunning(sessionName))
	}
}
