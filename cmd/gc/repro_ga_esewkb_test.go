package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestReproGaEsewkb_PinnedAlwaysExplicitResetRestartsWithinOnePass reproduces
// gastownhall/gascity#7691: a running, pinned, always-mode named session with
// an explicit controller reset (restart_requested + continuation_reset_pending,
// as written by `gc runtime request-restart`) is killed by the reconciler but
// not started again on the same pass; the start depends on a later pass. The
// expected behavior is that the session is back up after the one pass that
// killed it. useFreshReads routes the reconciler through a fresh-readable
// provider view, as production tmux is: the same-pass start is gated on a
// fresh read proving the killed name released.
func TestReproGaEsewkb_PinnedAlwaysExplicitResetRestartsWithinOnePass(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.useFreshReads()

	env.reconcile([]beads.Bead{session})

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated: the explicit reset must have been processed this pass", got.Metadata["session_key"])
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned always-mode session %q is not running after the pass that killed it (state=%q, last_woke_at=%q); its restart waits for a later reconcile pass", sessionName, got.Metadata["state"], got.Metadata["last_woke_at"])
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Fatalf("last_woke_at empty after restart pass (state=%q), want the same-pass wake recorded", got.Metadata["state"])
	}
}
