package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestStartupBackoffUntilSchedule pins the retry cadence a template gets while
// its cold start keeps failing. The first failure retries immediately (a single
// transient flake must not be penalised); from the second the hold doubles and
// is capped, so a start path that stays wedged is retried on the order of twice
// an hour instead of every 65 seconds (sr-xf2xj: 2,306 identical failures in
// 42h).
func TestStartupBackoffUntilSchedule(t *testing.T) {
	now := time.Date(2026, 8, 25, 5, 3, 26, 0, time.UTC)
	cases := []struct {
		count int
		want  time.Duration
	}{
		{0, 0},
		{1, 0},
		{2, 30 * time.Second},
		{3, time.Minute},
		{4, 2 * time.Minute},
		{5, 4 * time.Minute},
		{6, 8 * time.Minute},
		{7, 16 * time.Minute},
		{8, 30 * time.Minute},
		{20, 30 * time.Minute},
		{4000, 30 * time.Minute},
	}
	for _, tc := range cases {
		got := startupBackoffUntil(tc.count, now)
		if tc.want == 0 {
			if !got.IsZero() {
				t.Errorf("startupBackoffUntil(%d) = %v, want zero (no hold)", tc.count, got)
			}
			continue
		}
		if want := now.Add(tc.want); !got.Equal(want) {
			t.Errorf("startupBackoffUntil(%d) = %v, want %v (+%v)", tc.count, got, want, tc.want)
		}
	}
}

// TestStartupBackoffNeverShortensTheThresholdQuarantine pins that the capped
// backoff schedule is never weaker than the fixed quarantine it now sits
// alongside: at the threshold failure the effective hold must still be at
// least defaultQuarantineDuration.
func TestStartupBackoffNeverShortensTheThresholdQuarantine(t *testing.T) {
	now := time.Date(2026, 8, 25, 5, 3, 26, 0, time.UTC)
	ep := sessionpkg.StartupHealthEpisode{
		ConsecutiveCount: defaultMaxWakeAttempts,
		QuarantinedUntil: now.Add(defaultQuarantineDuration),
		BackoffUntil:     startupBackoffUntil(defaultMaxWakeAttempts, now),
	}
	if hold := ep.StartHoldUntil(); hold.Before(now.Add(defaultQuarantineDuration)) {
		t.Errorf("StartHoldUntil() = %v, want >= %v (backoff must never shorten the quarantine)",
			hold, now.Add(defaultQuarantineDuration))
	}
}

// TestPendingCreateFailureAccruesBackoffHoldBelowThreshold pins that the
// second consecutive cold-start failure records a backoff hold — the ONLY
// thing standing between a wedged start path and an unbounded retry loop
// below the quarantine threshold. The quarantine field must stay clear so
// `gc doctor` keeps reporting only genuine crash loops.
func TestPendingCreateFailureAccruesBackoffHoldBelowThreshold(t *testing.T) {
	h := newSessionChaosHarness(t, 20260906001)
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < 2; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.ConsecutiveCount != 2 {
		t.Fatalf("ConsecutiveCount = %d, want 2", episode.ConsecutiveCount)
	}
	if !episode.QuarantinedUntil.IsZero() {
		t.Errorf("QuarantinedUntil = %v below threshold, want zero", episode.QuarantinedUntil)
	}
	want := episode.LastFailureAt.Add(defaultStartupBackoffBase)
	if !episode.BackoffUntil.Equal(want) {
		t.Errorf("BackoffUntil = %v, want %v (last failure + %v)", episode.BackoffUntil, want, defaultStartupBackoffBase)
	}
}

// TestStartupHealthBackoffBlocksProviderStartUntilExpiry pins that the start
// gate honours a sub-threshold backoff hold and releases on expiry.
func TestStartupHealthBackoffBlocksProviderStartUntilExpiry(t *testing.T) {
	h := newSessionChaosHarness(t, 20260906002)
	h.createSessionIntent()
	h.assertCreatingIntent()

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	backoffUntil := h.env.clk.Now().Add(defaultStartupBackoffBase)
	if err := is.SaveStartupHealthEpisode(sessionpkg.StartupHealthEpisode{
		SessionName:      h.sessionName,
		ConsecutiveCount: 2,
		LastFailureAt:    h.env.clk.Now(),
		BackoffUntil:     backoffUntil,
	}); err != nil {
		t.Fatalf("SaveStartupHealthEpisode: %v", err)
	}

	startsBefore := h.countRuntimeCalls("Start")
	for i := 0; i < 5; i++ {
		h.reconcileTick()
	}
	if got := h.countRuntimeCalls("Start"); got != startsBefore {
		t.Fatalf("Start called %d more time(s) during backoff; want 0", got-startsBefore)
	}

	h.env.clk.Advance(backoffUntil.Add(time.Second).Sub(h.env.clk.Now()))
	h.reconcileTick()
	if got := h.countRuntimeCalls("Start"); got <= startsBefore {
		t.Fatalf("Start not attempted after backoff expiry (calls before=%d after=%d)", startsBefore, got)
	}
}

// TestStartupHealthAlertEmittedOncePerEpisode pins the escalation the bead
// asks for: reaching the consecutive-failure threshold emits exactly one
// session.startup_health_alert, and further failures in the same episode do
// not re-emit it. Before this, session.cold_start_timeout was emitted per
// attempt and nothing consumed it, so 42 hours of failure raised no signal.
func TestStartupHealthAlertEmittedOncePerEpisode(t *testing.T) {
	h := newSessionChaosHarness(t, 20260906003)
	rec := &capturingRecorder{}
	h.env.rec = rec
	h.createSessionIntent()
	h.assertCreatingIntent()
	sessionName := h.sessionName

	h.env.sp.StartErrors[sessionName] = errors.New("provider start failure")
	for i := 0; i < defaultMaxWakeAttempts+1; i++ {
		if i > 0 {
			h.createReplacementPendingCreateBead()
		}
		if tick := runDesiredPendingCreateTicks(t, h); tick == -1 {
			t.Fatalf("failure %d: pending-create claim never released within 30 ticks", i+1)
		}
	}

	alerts := rec.eventsOfType(events.SessionStartupHealthAlert)
	if len(alerts) != 1 {
		t.Fatalf("recorded %d %s events, want exactly 1", len(alerts), events.SessionStartupHealthAlert)
	}
	if alerts[0].Subject != sessionName {
		t.Errorf("alert Subject = %q, want %q", alerts[0].Subject, sessionName)
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: h.env.store})
	episode, err := is.LoadStartupHealthEpisode(sessionName)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.AlertDisposition != sessionpkg.AlertDispositionSent {
		t.Errorf("AlertDisposition = %q, want %q", episode.AlertDisposition, sessionpkg.AlertDispositionSent)
	}
}
