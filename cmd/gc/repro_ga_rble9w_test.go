package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRepro_ga_rble9w_PinnedMayorHandoffNeverCyclesAndFalselyReportsSuccess(t *testing.T) {
	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "mayor", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "mayor")
	env.desiredState[sessionName] = TemplateParams{
		Command:          "true",
		SessionName:      sessionName,
		TemplateName:     "mayor",
		ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"},
	}
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "mayor",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		// no restart_requested and no continuation_reset_pending: models
		// persistRestart() not landing the marker
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", "1"); err != nil { // gc handoff's tmux flag
		t.Fatalf("SetMeta(GC_RESTART_REQUESTED): %v", err)
	}
	dops := newDrainOps(env.sp)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"mayor": 1}, dops)

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("expected repro: pinned mayor still running (bug), but it was cycled")
	}
	flag, err := env.sp.GetMeta(sessionName, "GC_RESTART_REQUESTED")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if strings.TrimSpace(flag) != "" {
		t.Fatalf("GC_RESTART_REQUESTED = %q; repro expects reconciler to have cleared it without cycling", flag)
	}
	if got := env.stderr.String(); !strings.Contains(got, "skipping abrupt restart-requested kill for pinned named session") {
		t.Fatalf("stderr = %q, want pinned collateral-skip diagnostic", got)
	}
	// Fixed behavior: the flag was cleared by the reconciler's collateral-skip
	// above, but the pinned mayor session is still actually running, so
	// waitForControllerRestart must not report success — it polls to the
	// timeout and returns 1, instead of the pre-fix false success (0).
	var waitErr bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := waitForControllerRestart(ctx, dops, env.sp, sessionName, "gc handoff", 10*time.Millisecond, 100*time.Millisecond, &waitErr)
	if code != 1 {
		t.Fatalf("waitForControllerRestart code = %d, want 1 (still running, not a false success); stderr=%q", code, waitErr.String())
	}
}
