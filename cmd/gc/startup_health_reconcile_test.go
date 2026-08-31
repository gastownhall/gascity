package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// createReplacementPendingCreateBead mints a new bead sharing the harness's
// current session name, simulating a reconciler-materialized replacement
// after the prior bead closed as failed-create (ga-o04bfr.1.1 requires the
// episode to accrue across distinct replacement bead IDs).
//
// This cannot go through session.Manager.CreateSession: h.sessionName is
// always the auto-derived, "s-"-prefixed name from the original
// createSessionIntent() call, and ValidateExplicitName permanently rejects
// that reserved prefix for a caller-supplied ExplicitName (by design, so
// external callers can't spoof an auto-generated identity). Passing the
// name via ExtraMeta instead doesn't work either: createBeadOnly's
// auto-name-derivation unconditionally overwrites "session_name" whenever
// ExplicitName is empty. So the replacement bead is built directly against
// the store, mirroring the exact metadata shape createBeadOnly produces for
// an auto-named bead-only create (see internal/session/manager.go), with
// session_name forced to the reused value.
func (h *sessionChaosHarness) createReplacementPendingCreateBead() {
	h.t.Helper()
	now := h.env.clk.Now().UTC()
	created, err := h.env.store.Create(beads.Bead{
		Title: "Chaos worker",
		Type:  sessionpkg.BeadType,
		Labels: []string{
			sessionpkg.LabelSession,
			"template:" + h.template,
		},
		Metadata: map[string]string{
			"template":                  h.template,
			"state":                     string(sessionpkg.StateStartPending),
			"provider":                  "fake",
			"work_dir":                  "",
			"command":                   h.command,
			"resume_flag":               "",
			"resume_style":              "",
			"resume_command":            "",
			"session_id_flag":           "",
			"generation":                fmt.Sprintf("%d", sessionpkg.DefaultGeneration),
			"continuation_epoch":        fmt.Sprintf("%d", sessionpkg.DefaultContinuationEpoch),
			"instance_token":            sessionpkg.NewInstanceToken(),
			"pending_create_claim":      "true",
			"pending_create_started_at": now.Format(time.RFC3339),
			"session_origin":            "ephemeral",
			"session_name":              h.sessionName,
		},
	})
	if err != nil {
		h.t.Fatalf("creating replacement pending-create bead: %v", err)
	}
	h.sessionID = created.ID
	h.setDesired(true)
}

func TestPendingCreateFailuresAccrueStartupHealthEpisodeAcrossReplacementBeads(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830001)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}
	delete(h.env.sp.StartErrors, sessionName)

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.ConsecutiveCount != defaultMaxWakeAttempts {
		t.Errorf("ConsecutiveCount = %d, want %d", episode.ConsecutiveCount, defaultMaxWakeAttempts)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Error("QuarantinedUntil is zero, want set after reaching the failure threshold")
	}

	bead := h.mustBead()
	if got := bead.Metadata["wake_attempts"]; got != "" {
		t.Errorf("wake_attempts = %q, want empty (pending-create lane must not touch the wake-failure lane)", got)
	}
	if got := bead.Metadata["churn_count"]; got != "" {
		t.Errorf("churn_count = %q, want empty (pending-create lane must not touch the churn lane)", got)
	}

	restarted, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode (simulated restart re-read): %v", err)
	}
	if restarted != episode {
		t.Errorf("episode after simulated restart = %+v, want unchanged %+v", restarted, episode)
	}
}

func TestQuarantinedStartupHealthBlocksProviderStartUntilExpiry(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830002)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Fatal("episode not quarantined after reaching the failure threshold; cannot test quarantine gating")
	}

	// The 5th failure's bead already rolled back to closed/failed-create (like
	// every prior one), so no open bead remains for this session name.
	// reconcileTick (unlike production's syncSessionBeads) never materializes a
	// replacement on its own — simulate the one production would create for a
	// still-desired name, exactly as the loop above does for failures 2-5, so
	// there is a live candidate to exercise the quarantine gate against.
	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)
	startsBefore := h.countRuntimeCalls("Start")
	for i := 0; i < 5; i++ {
		h.reconcileTick()
	}
	if got := h.countRuntimeCalls("Start"); got != startsBefore {
		t.Fatalf("Start called %d more time(s) before quarantine expiry; want 0 (quarantine must block retry)", got-startsBefore)
	}

	h.env.clk.Advance(episode.QuarantinedUntil.Add(time.Second).Sub(h.env.clk.Now()))
	h.reconcileTick()
	if got := h.countRuntimeCalls("Start"); got <= startsBefore {
		t.Fatalf("Start not attempted after quarantine expiry (calls before=%d after=%d)", startsBefore, got)
	}
}

func TestStartupHealthEpisodeClearsOnFirstSuccessfulStart(t *testing.T) {
	h := newSessionChaosHarness(t, 20260830003)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	const belowThreshold = defaultMaxWakeAttempts - 2
	for i := 0; i < belowThreshold; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h, 30); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	before, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if before.ConsecutiveCount != belowThreshold {
		t.Fatalf("ConsecutiveCount = %d, want %d before the successful start", before.ConsecutiveCount, belowThreshold)
	}

	h.createReplacementPendingCreateBead()
	delete(h.env.sp.StartErrors, sessionName)
	started := false
	for i := 0; i < 10; i++ {
		h.reconcileTick()
		if h.countRuntimeCalls("Start") > 0 {
			started = true
			break
		}
	}
	if !started {
		t.Fatal("Start was never attempted after clearing the injected error")
	}

	after, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode after successful start: %v", err)
	}
	if after.ConsecutiveCount != 0 {
		t.Errorf("ConsecutiveCount after successful start = %d, want 0 (episode must clear on recovery)", after.ConsecutiveCount)
	}
}
