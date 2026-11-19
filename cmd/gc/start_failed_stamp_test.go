package main

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The start-failure stamp (#7680, change 4): last_start_failed_at is written
// in the same write that undoes a failed attempt's PreWake on exactly the
// arms that leave the row retryable, and wake fairness reads it as the later
// of its chain and the stamp, so a session that just failed a start sorts
// behind its class's steady state instead of back to its front (its
// last_woke_at was cleared).

// TestReconcileSessionBeads_StartFailureStampsLastStartFailedAt pins the
// legacy twin: the configured named session's transient start failure stamps
// last_start_failed_at in the same write that clears the attempt's in-flight
// lease, and the stamp does not hold the retry back — the next pass starts
// the session again.
func TestReconcileSessionBeads_StartFailureStampsLastStartFailedAt(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "helper",
			StartCommand: "true",
		}},
		NamedSessions: []config.NamedSession{{Template: "helper", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "helper")
	env.sp.StartErrors = map[string]error{sessionName: errors.New("tmux server degraded: refusing new-session to avoid socket clobber")}
	env.desiredState[sessionName] = TemplateParams{
		Command:      "test-cmd",
		SessionName:  sessionName,
		TemplateName: "helper",
	}

	session := env.createSessionBead(sessionName, "helper")
	env.setSessionMetadata(&session, map[string]string{
		"session_name_explicit":      "true",
		"pending_create_claim":       "true",
		"state":                      "creating",
		"continuation_epoch":         "1",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "helper",
		namedSessionModeMetadata:     "always",
	})

	before := env.clk.Now()
	if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, nil, nil); woken != 0 {
		t.Fatalf("first tick woken = %d, want 0 (the start fails)", woken)
	}
	failed, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s) after failed start: %v", session.ID, err)
	}
	stamp := failed.Metadata["last_start_failed_at"]
	at, parseErr := time.Parse(time.RFC3339, stamp)
	if parseErr != nil || at.Before(before) || at.After(env.clk.Now().Add(time.Second)) {
		t.Fatalf("last_start_failed_at = %q, want this failed attempt's time (between %s and %s), stamped in the same write that clears the in-flight lease", stamp, before, env.clk.Now())
	}
	if got := failed.Metadata["last_woke_at"]; got != "" {
		t.Errorf("last_woke_at = %q after the start failed, want empty (the attempt is over)", got)
	}

	// The stamp is advisory: it must not hold the retry back.
	delete(env.sp.StartErrors, sessionName)
	env.clk.Advance(time.Second)
	if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{failed}, nil, nil); woken != 1 {
		t.Fatalf("second tick woken = %d, want 1 (the stamp is advisory; the row must be retried)\nstderr:\n%s", woken, env.stderr.String())
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q not running after the retry tick", sessionName)
	}
}

// TestWakeFairnessTimeAdvancesToTheStartFailedStamp pins the fairness key's
// later-of rule: a candidate that failed a start recently sorts behind a
// same-class peer whose last_woke_at is just as old but that did not fail —
// the stamp keeps the failed row from returning to the front of the wake
// queue its cleared last_woke_at would give it (#7680). An older stamp
// (superseded by a newer last_woke_at) stays inert.
func TestWakeFairnessTimeAdvancesToTheStartFailedStamp(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 18, 5, 0, time.UTC)
	woke := now.Add(-2 * time.Hour).Format(time.RFC3339)
	plain := startCandidate{info: session.Info{LastWokeAt: woke}}
	stamped := startCandidate{info: session.Info{LastWokeAt: woke, LastStartFailedAt: now.Add(-time.Minute).Format(time.RFC3339)}}
	if got := wakeFairnessTime(plain); !got.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("plain candidate's fairness time %v, want its last_woke_at", got)
	}
	if got := wakeFairnessTime(stamped); !got.Equal(now.Add(-time.Minute)) {
		t.Fatalf("stamped candidate's fairness time %v, want the later failure stamp", got)
	}
	// The stamp is advisory, not a penalty: one older than the chain key
	// (a later successful wake) leaves the chain key in charge.
	superseded := startCandidate{info: session.Info{
		LastWokeAt:        now.Add(-time.Minute).Format(time.RFC3339),
		LastStartFailedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
	}}
	if got := wakeFairnessTime(superseded); !got.Equal(now.Add(-time.Minute)) {
		t.Fatalf("superseded stamp won %v, want the newer last_woke_at", got)
	}
	ready := []startCandidate{stamped, plain}
	sortCandidatesByWakeFairness(ready)
	if ready[0].info.LastStartFailedAt != "" || ready[1].info.LastStartFailedAt == "" {
		t.Fatalf("order %v, want the un-stamped peer first: a row that just failed a start does not return to the front of its class", ready)
	}
}

// TestAbandonStampsStartFailedOnRetryArms is the v2 twin of the legacy
// stamp: the prepare arm, the configured-named pending arm, and the
// any-other restore write last_start_failed_at in the same write that
// clears or restores last_woke_at. A relaunch (its marker kept, nothing
// restored), a terminal park and a rate-limit quarantine write no stamp:
// those rows are not retryable in place.
func TestAbandonStampsStartFailedOnRetryArms(t *testing.T) {
	k := startKit(t, intentStart, "creating")
	wantStamp := gatherNow.Format(time.RFC3339)
	creating := poolRow("gc-1", "worker", 1, "creating", "generation", "4", "instance_token", "tok-4",
		"last_woke_at", gatherNow.Format(time.RFC3339), "session_key", "k-1", "started_config_hash", "h-1",
		"wake_attempts", "2", "sleep_reason", "", "wake_request", "")
	undo := preWakeUndo{
		token:   "tok-4",
		written: session.MetadataPatch{"state": "creating", "last_woke_at": gatherNow.Format(time.RFC3339), "wake_request": ""},
		restore: session.MetadataPatch{"state": "asleep", "last_woke_at": "", "wake_request": "explicit"},
	}
	other := startFailureRead{startFailure: startFailure{cause: causeStartError, err: errors.New("boom"), undo: undo, display: "worker"}}
	prepare := other
	prepare.cause = causePrepare
	named := other
	named.pending, named.configured = true, true
	for name, f := range map[string]startFailureRead{"prepare": prepare, "configured named pending": named, "any other failure": other} {
		t.Run(name, func(t *testing.T) {
			step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, f))
			if step.Write["last_start_failed_at"] != wantStamp {
				t.Fatalf("write %v, want the failure stamp %q in the same write", step.Write, wantStamp)
			}
		})
	}
	relaunch := other
	relaunch.relaunched = true
	if step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, relaunch)); step.Write["last_start_failed_at"] != "" {
		t.Fatalf("a relaunch stamped %q; its marker is kept and nothing is restored", step.Write["last_start_failed_at"])
	}
	terminal := other
	terminal.terminal = "model_not_found"
	if step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, terminal)); step.Write["last_start_failed_at"] != "" {
		t.Fatalf("a terminal park stamped %q; the parked row is not retryable in place", step.Write["last_start_failed_at"])
	}
	limited := other
	limited.rateLimit = true
	if step := abandonStep(abandonView(k, creating, txRuntime{Class: rtAbsent}, limited)); step.Write["last_start_failed_at"] != "" {
		t.Fatalf("a rate-limit quarantine stamped %q; the quarantined row is not retryable in place", step.Write["last_start_failed_at"])
	}
}

// TestStartFailedStampAbsentOnRefusedDeferredAndRolledBackArms pins the
// design's NOT list (#7680, change 4): the arms that do not leave the row
// retryable in place must not write last_start_failed_at. The capacity
// refusal and the async refresh release run the PLAIN restore; the deferral
// and panic-recovery paths run the PLAIN lease clear; a superseded PreWake
// restores only the circuit-breaker snapshot; a pending-create rollback
// closes the row through the failed-create close patches, whose terminal
// metadata is no place for a fairness stamp. Only the stamping siblings write
// it (AC-6).
func TestStartFailedStampAbsentOnRefusedDeferredAndRolledBackArms(t *testing.T) {
	now := time.Date(2026, 10, 12, 11, 18, 5, 0, time.UTC)
	woke := now.Add(-time.Minute).Format(time.RFC3339)
	m := beads.NewMemStore()
	sessFront := sessionFrontDoor(m)
	mkRow := func(id string) beads.Bead {
		t.Helper()
		bead, err := m.Create(beads.Bead{
			ID:     id,
			Title:  "w",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name":   "w",
				"template":       "w",
				"state":          "creating",
				"instance_token": "tok-4",
				"last_woke_at":   woke,
				"wake_request":   "",
			},
		})
		if err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
		return bead
	}

	// Capacity refusal and async refresh release: the plain restore puts the
	// row back without the stamp — the endpoint or the refresh refused, the
	// session did not fail.
	bead := mkRow("gc-1")
	prepared := preparedStart{
		candidate: startCandidate{info: sessiontest.SeedBead(t, bead), tp: TemplateParams{TemplateName: "w", SessionName: "w"}},
		preWakeUndo: preWakeUndo{
			token:   "tok-4",
			written: session.MetadataPatch{"state": "creating", "last_woke_at": woke, "wake_request": ""},
			restore: session.MetadataPatch{"state": "asleep", "last_woke_at": "", "wake_request": "explicit"},
		},
	}
	restorePreWakeState(prepared, sessFront, io.Discard)
	b, err := m.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["state"] != "asleep" || b.Metadata["last_woke_at"] != "" {
		t.Fatalf("plain restore wrote state=%q last_woke_at=%q; the restore itself must still run", b.Metadata["state"], b.Metadata["last_woke_at"])
	}
	if _, stamped := b.Metadata["last_start_failed_at"]; stamped {
		t.Fatal("the plain restorePreWakeState stamped last_start_failed_at; only the marking sibling (a failed start) may")
	}

	// Deferral and panic recovery release the lease with the plain clear,
	// which writes only last_woke_at="".
	mkRow("gc-2")
	clearPendingStartInFlightLease("gc-2", sessFront, io.Discard)
	b, err = m.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["last_woke_at"] != "" {
		t.Fatal("the plain clear did not clear last_woke_at; the pin is vacuous")
	}
	if _, stamped := b.Metadata["last_start_failed_at"]; stamped {
		t.Fatal("the plain clearPendingStartInFlightLease stamped last_start_failed_at; only the marking sibling (a failed start) may")
	}

	// A superseded PreWake restores the circuit-breaker entry snapshot and
	// nothing else.
	mkRow("gc-3")
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	snap := cb.entrySnapshot("ident")
	if _, err := recordSessionCircuitBreakerRestart(sessFront, "gc-3", cb, "ident", now); err != nil {
		t.Fatalf("recording a breaker restart: %v", err)
	}
	cb.restoreEntrySnapshot(sessFront, "gc-3", "ident", snap, now, io.Discard)
	b, err = m.Get("gc-3")
	if err != nil {
		t.Fatal(err)
	}
	if _, hasState := b.Metadata[session.SessionCircuitStateMetadataKey]; !hasState {
		t.Fatal("the breaker restore wrote no circuit metadata; the pin is vacuous")
	}
	if _, stamped := b.Metadata["last_start_failed_at"]; stamped {
		t.Fatal("a superseded PreWake stamped last_start_failed_at; the row moved, not failed")
	}

	// Pending-create rollback closes the row: the failed-create close patches
	// (legacy's, and v2's rollbackPatch twin) carry no stamp — the row ends,
	// and its successor is a new row with a fresh fairness chain.
	if _, stamped := failedCreateClosePatch(session.Info{ID: "gc-4"}, now)["last_start_failed_at"]; stamped {
		t.Fatal("legacy's failed-create close patch stamps last_start_failed_at; the rolled-back row closes")
	}
	rollback, _ := rollbackPatch(session.Info{ID: "gc-4"}, now, nil)
	if _, stamped := rollback["last_start_failed_at"]; stamped {
		t.Fatal("v2's rollbackPatch stamps last_start_failed_at; the rolled-back row closes")
	}
}
