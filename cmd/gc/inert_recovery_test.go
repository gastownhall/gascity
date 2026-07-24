package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Pane fixtures mirroring the ga-qox / ci-emg incident: a transport failure that
// aborted the turn and returned the CLI to its idle prompt.
const (
	codexTransportInertPane = "● Reading the requirements.\n\n" +
		"⚠ stream error: Falling back from WebSockets to HTTPS transport. request timed out\n" +
		"⚠ stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)\n\n› "

	claudeTransportInertPane = "● Fetching upstream.\n\n" +
		"API Error: request to https://api.anthropic.com/v1/messages failed, reason: getaddrinfo ENOTFOUND api.anthropic.com\n\n❯ "

	inertMidTurnPane = "● Retrying.\n\n" +
		"stream disconnected before completion: error sending request for url (https://chatgpt.com/backend-api/codex/responses)\n" +
		"· Herding the model… (2m 28s · esc to interrupt)"

	inertOrdinaryIdlePane = "● Done. Committed 3 files.\n\n› "
)

func inertTestCfg() *config.City {
	return &config.City{Agents: []config.Agent{
		{Name: "polecat", Nudge: "Run gc hook --claim --json now; if it returns work, execute it."},
		{Name: "mayor", Nudge: "Run gc prime and resume coordination."},
	}}
}

func inertSession(id, name, template string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name": name,
			"template":     template,
		},
	}
}

// inertFake starts a running fake session with a canned last-activity time and
// pane content.
func inertFake(t *testing.T, name string, activity time.Time, pane string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.TODO(), name, runtime.Config{}); err != nil {
		t.Fatalf("fake start: %v", err)
	}
	sp.SetActivity(name, activity)
	sp.SetPeekOutput(name, pane)
	return sp
}

// nudgeFailProvider wraps the fake so Nudge records the call but returns an
// error, exercising the "persist count even when Nudge errors" path.
type nudgeFailProvider struct{ *runtime.Fake }

func (p nudgeFailProvider) Nudge(name string, content []runtime.ContentBlock) error {
	_ = p.Fake.Nudge(name, content) // still record the call for CountCalls
	return fmt.Errorf("nudge failed: transport down")
}

// inertMetadataFailStore injects bounded SetMetadataBatch failures while
// delegating every other store operation to the real in-memory store.
type inertMetadataFailStore struct {
	beads.Store
	failures int
	calls    int
}

func (s *inertMetadataFailStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return errors.New("metadata unavailable")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// A desired session inert on a Codex transport failure is observed on the first
// quiet tick, then nudged once the grace elapses.
func TestRecoverInertSessions_NudgesAfterGrace(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, codexTransportInertPane)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	desired := map[string]bool{"worker-1": true}
	ckpt := map[string]time.Time{}
	var out bytes.Buffer

	// Tick 1: past the quiet grace but first sighting → observe, no nudge.
	t1 := base.Add(inertRecoveryQuietGrace + time.Second)
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t1, &out)
	if n := sp.CountCalls("Nudge", "worker-1"); n != 0 {
		t.Fatalf("first sighting must observe, not nudge; nudges=%d", n)
	}
	if got := sessions[0].Metadata[sessionpkg.InertRecoveryFingerprintKey]; got != "stream_disconnected" {
		t.Fatalf("expected fingerprint marker, got %q", got)
	}

	// Tick 2: grace elapsed since the observation → nudge, attempt count 1.
	t2 := t1.Add(inertRecoveryGrace + time.Second)
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t2, &out)
	if n := sp.CountCalls("Nudge", "worker-1"); n != 1 {
		t.Fatalf("expected exactly one nudge after grace, got %d", n)
	}
	if !bytes.Contains(out.Bytes(), []byte("nudged worker-1 to resume")) {
		t.Fatalf("expected recovery telemetry, got: %q", out.String())
	}
	if got := sessions[0].Metadata[sessionpkg.InertRecoveryAttemptsKey]; got != "1" {
		t.Fatalf("expected attempt count 1, got %q", got)
	}

	// Tick 3: inside the backoff → no second nudge.
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t2.Add(30*time.Second), &out)
	if n := sp.CountCalls("Nudge", "worker-1"); n != 1 {
		t.Fatalf("must respect backoff; nudges=%d", n)
	}
}

// Claude ENOTFOUND coverage: a DNS failure is recovered the same way.
func TestRecoverInertSessions_ClaudeENOTFOUND(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "claude-1", base, claudeTransportInertPane)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-1", "claude-1", "polecat")}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	desired := map[string]bool{"claude-1": true}
	ckpt := map[string]time.Time{}
	var out bytes.Buffer

	t1 := base.Add(inertRecoveryQuietGrace + time.Second)
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t1, &out)
	if got := sessions[0].Metadata[sessionpkg.InertRecoveryFingerprintKey]; got != "dns_lookup_failure" {
		t.Fatalf("expected dns_lookup_failure marker, got %q", got)
	}
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t1.Add(inertRecoveryGrace+time.Second), &out)
	if n := sp.CountCalls("Nudge", "claude-1"); n != 1 {
		t.Fatalf("expected Claude ENOTFOUND recovery nudge, got %d", n)
	}
}

// Regression (ga-qox): the incident's crashed session was ATTACHED — a human was
// waiting on the dead turn and could not revive it. An attached session must be
// recovered on the same gating as any other; attachment is NOT an exemption.
func TestRecoverInertSessions_AttachedMayorStillRecovers(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "mayor", base, codexTransportInertPane)
	sp.SetAttached("mayor", true)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-mayor", "mayor", "mayor")}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	desired := map[string]bool{"mayor": true}
	ckpt := map[string]time.Time{}
	var out bytes.Buffer

	t1 := base.Add(inertRecoveryQuietGrace + time.Second)
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t1, &out) // observe
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, t1.Add(inertRecoveryGrace+time.Second), &out)
	if n := sp.CountCalls("Nudge", "mayor"); n != 1 {
		t.Fatalf("attached session must still recover; nudges=%d", n)
	}
}

// An ordinary idle prompt (no transport failure) is never nudged or marked.
func TestRecoverInertSessions_OrdinaryIdleNotRecovered(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, inertOrdinaryIdlePane)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	desired := map[string]bool{"worker-1": true}
	ckpt := map[string]time.Time{}
	var out bytes.Buffer

	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, base.Add(inertRecoveryQuietGrace+time.Second), &out)
	if n := sp.CountCalls("Nudge", "worker-1"); n != 0 {
		t.Fatalf("ordinary idle must not be nudged; nudges=%d", n)
	}
	if got := sessions[0].Metadata[sessionpkg.InertRecoveryFingerprintKey]; got != "" {
		t.Fatalf("ordinary idle must not be marked, got %q", got)
	}
}

// An active turn is not restarted: (a) fresh activity is skipped before any peek,
// and (b) a mid-turn pane (failure text but still working, no prompt) is not
// nudged.
func TestRecoverInertSessions_ActiveTurnNotRecovered(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	cfg := inertTestCfg()

	t.Run("fresh activity is not even peeked", func(t *testing.T) {
		sp := inertFake(t, "worker-1", base, codexTransportInertPane)
		sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
		store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
		recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, map[string]time.Time{}, base.Add(5*time.Second), &bytes.Buffer{})
		if n := sp.CountCalls("Peek", "worker-1"); n != 0 {
			t.Fatalf("fresh activity must be skipped before peek; peeks=%d", n)
		}
	})

	t.Run("mid-turn pane is peeked but not nudged", func(t *testing.T) {
		sp := inertFake(t, "worker-1", base, inertMidTurnPane)
		sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
		store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
		recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, map[string]time.Time{}, base.Add(inertRecoveryQuietGrace+time.Second), &bytes.Buffer{})
		if n := sp.CountCalls("Nudge", "worker-1"); n != 0 {
			t.Fatalf("mid-turn session must not be nudged; nudges=%d", n)
		}
	})
}

// Checkpoint: one ordinary completed turn causes at most one inspection.
func TestRecoverInertSessions_OneInspectionPerTurn(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, inertOrdinaryIdlePane)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	desired := map[string]bool{"worker-1": true}
	ckpt := map[string]time.Time{}

	// Two ticks with the SAME activity → exactly one peek.
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, base.Add(inertRecoveryQuietGrace+time.Second), &bytes.Buffer{})
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, base.Add(inertRecoveryQuietGrace+2*time.Second), &bytes.Buffer{})
	if n := sp.CountCalls("Peek", "worker-1"); n != 1 {
		t.Fatalf("unchanged activity must inspect once, got %d peeks", n)
	}

	// A new turn (new activity) → one more inspection.
	next := base.Add(time.Minute)
	sp.SetActivity("worker-1", next)
	recoverInertSessions(sp, cfg, store, sessions, desired, ckpt, next.Add(inertRecoveryQuietGrace+time.Second), &bytes.Buffer{})
	if n := sp.CountCalls("Peek", "worker-1"); n != 2 {
		t.Fatalf("new activity must trigger a fresh inspection, got %d peeks", n)
	}
}

// After the attempt cap the lane gives up quietly — bounded, no nudge storm.
func TestRecoverInertSessions_GivesUpAtCap(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, codexTransportInertPane)
	cfg := inertTestCfg()
	s := inertSession("s-1", "worker-1", "polecat")
	s.Metadata[sessionpkg.InertRecoveryFingerprintKey] = "stream_disconnected"
	s.Metadata[sessionpkg.InertRecoveryAttemptsKey] = strconv.Itoa(inertRecoveryMaxAttempts)
	s.Metadata[sessionpkg.InertRecoveryAtKey] = base.Format(time.RFC3339)
	sessions := []beads.Bead{s}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	var out bytes.Buffer

	checkpoint := map[string]time.Time{}
	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, checkpoint, base.Add(time.Hour), &out)
	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, checkpoint, base.Add(time.Hour+time.Second), &out)
	if n := sp.CountCalls("Nudge", "worker-1"); n != 0 {
		t.Fatalf("must not nudge past the cap; nudges=%d", n)
	}
	if n := sp.CountCalls("Peek", "worker-1"); n != 1 {
		t.Fatalf("unchanged exhausted episode must not be peeked every tick; peeks=%d", n)
	}
}

// Once an episode is marked, unchanged activity does not need another pane
// read until its grace/backoff deadline. Activity changes still bypass this
// checkpoint so recovery can be detected and the marker cleared.
func TestRecoverInertSessions_SkipsPeekUntilMarkedDeadline(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, codexTransportInertPane)
	cfg := inertTestCfg()
	s := inertSession("s-1", "worker-1", "polecat")
	s.Metadata[sessionpkg.InertRecoveryFingerprintKey] = "stream_disconnected"
	s.Metadata[sessionpkg.InertRecoveryAttemptsKey] = "1"
	s.Metadata[sessionpkg.InertRecoveryAtKey] = base.Format(time.RFC3339)
	sessions := []beads.Bead{s}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	checkpoint := map[string]time.Time{"worker-1": base}

	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, checkpoint, base.Add(time.Minute), &bytes.Buffer{})
	if n := sp.CountCalls("Peek", "worker-1"); n != 0 {
		t.Fatalf("inside backoff with unchanged activity must skip peek; peeks=%d", n)
	}

	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, checkpoint, base.Add(inertRecoveryBackoff+time.Second), &bytes.Buffer{})
	if n := sp.CountCalls("Peek", "worker-1"); n != 1 {
		t.Fatalf("past backoff must re-inspect before retry; peeks=%d", n)
	}
}

// The attempt count and cooldown are persisted even when the nudge itself fails,
// so a failed nudge still advances the backoff and cannot re-nudge every tick.
func TestRecoverInertSessions_PersistsCountWhenNudgeErrors(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	fake := inertFake(t, "worker-1", base, codexTransportInertPane)
	sp := nudgeFailProvider{Fake: fake}
	cfg := inertTestCfg()
	s := inertSession("s-1", "worker-1", "polecat")
	// Already observed one grace ago; this tick decides to nudge.
	s.Metadata[sessionpkg.InertRecoveryFingerprintKey] = "stream_disconnected"
	s.Metadata[sessionpkg.InertRecoveryAttemptsKey] = "0"
	s.Metadata[sessionpkg.InertRecoveryAtKey] = base.Format(time.RFC3339)
	sessions := []beads.Bead{s}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	var out bytes.Buffer

	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, map[string]time.Time{}, base.Add(inertRecoveryGrace+time.Second), &out)
	if n := fake.CountCalls("Nudge", "worker-1"); n != 1 {
		t.Fatalf("nudge should have been attempted once, got %d", n)
	}
	if got := sessions[0].Metadata[sessionpkg.InertRecoveryAttemptsKey]; got != "1" {
		t.Fatalf("attempt count must persist despite nudge error, got %q", got)
	}
	if !bytes.Contains(out.Bytes(), []byte("failed")) {
		t.Fatalf("expected failure telemetry, got: %q", out.String())
	}
}

// The attempt marker is the storm-prevention boundary. If it cannot be
// persisted, the runtime must not receive a nudge that the next tick cannot
// account for.
func TestRecoverInertSessions_DoesNotNudgeWhenAttemptMarkerFails(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, codexTransportInertPane)
	cfg := inertTestCfg()
	s := inertSession("s-1", "worker-1", "polecat")
	s.Metadata[sessionpkg.InertRecoveryFingerprintKey] = "stream_disconnected"
	s.Metadata[sessionpkg.InertRecoveryAttemptsKey] = "0"
	s.Metadata[sessionpkg.InertRecoveryAtKey] = base.Format(time.RFC3339)
	sessions := []beads.Bead{s}
	backing := beads.NewMemStoreFrom(0, sessions, nil)
	failing := &inertMetadataFailStore{Store: backing, failures: 1}
	store := beads.SessionStore{Store: failing}
	var out bytes.Buffer

	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, map[string]time.Time{}, base.Add(inertRecoveryGrace+time.Second), &out)
	if n := sp.CountCalls("Nudge", "worker-1"); n != 0 {
		t.Fatalf("must not nudge without a persisted attempt marker; nudges=%d", n)
	}
	if failing.calls != 1 {
		t.Fatalf("metadata writes = %d, want 1", failing.calls)
	}
	if !bytes.Contains(out.Bytes(), []byte("marking")) {
		t.Fatalf("expected metadata failure telemetry, got: %q", out.String())
	}
}

// A failed first-observation write must not poison the activity checkpoint.
// The next tick with unchanged pane activity retries the inspection and can
// arm the durable state machine once persistence is healthy again.
func TestRecoverInertSessions_RetriesObservationAfterMarkerFailure(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, codexTransportInertPane)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
	backing := beads.NewMemStoreFrom(0, sessions, nil)
	failing := &inertMetadataFailStore{Store: backing, failures: 1}
	store := beads.SessionStore{Store: failing}
	desired := map[string]bool{"worker-1": true}
	checkpoint := map[string]time.Time{}
	var out bytes.Buffer
	now := base.Add(inertRecoveryQuietGrace + time.Second)

	recoverInertSessions(sp, cfg, store, sessions, desired, checkpoint, now, &out)
	recoverInertSessions(sp, cfg, store, sessions, desired, checkpoint, now.Add(time.Second), &out)

	if n := sp.CountCalls("Peek", "worker-1"); n != 2 {
		t.Fatalf("failed observation marker must be retried; peeks=%d", n)
	}
	if failing.calls != 2 {
		t.Fatalf("metadata writes = %d, want 2", failing.calls)
	}
	stored, err := backing.Get("s-1")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	if got := stored.Metadata[sessionpkg.InertRecoveryFingerprintKey]; got != "stream_disconnected" {
		t.Fatalf("expected durable marker after retry, got %q", got)
	}
}

// Once a session recovers (its pane no longer shows a transport failure), the
// marker is cleared so the next episode starts fresh.
func TestRecoverInertSessions_ClearsMarkerOnRecovery(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, inertOrdinaryIdlePane) // recovered pane
	cfg := inertTestCfg()
	s := inertSession("s-1", "worker-1", "polecat")
	s.Metadata[sessionpkg.InertRecoveryFingerprintKey] = "stream_disconnected"
	s.Metadata[sessionpkg.InertRecoveryAttemptsKey] = "1"
	s.Metadata[sessionpkg.InertRecoveryAtKey] = base.Format(time.RFC3339)
	sessions := []beads.Bead{s}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}

	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{"worker-1": true}, map[string]time.Time{}, base.Add(inertRecoveryQuietGrace+time.Second), &bytes.Buffer{})
	if got := sessions[0].Metadata[sessionpkg.InertRecoveryFingerprintKey]; got != "" {
		t.Fatalf("marker should be cleared on recovery, got %q", got)
	}
}

// A dead or no-longer-desired session cannot belong to the same recovery
// episode. Clear its marker without peeking so a later process with the same
// session bead starts with a fresh attempt budget.
func TestRecoverInertSessions_ClearsMarkerWhenSessionIsNoLongerEligible(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		running bool
		desired bool
	}{
		{name: "stopped", running: false, desired: true},
		{name: "not desired", running: true, desired: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := inertFake(t, "worker-1", base, codexTransportInertPane)
			if !tt.running {
				if err := sp.Stop("worker-1"); err != nil {
					t.Fatalf("stop fake: %v", err)
				}
			}
			s := inertSession("s-1", "worker-1", "polecat")
			s.Metadata[sessionpkg.InertRecoveryFingerprintKey] = "stream_disconnected"
			s.Metadata[sessionpkg.InertRecoveryAttemptsKey] = "2"
			s.Metadata[sessionpkg.InertRecoveryAtKey] = base.Format(time.RFC3339)
			sessions := []beads.Bead{s}
			backing := beads.NewMemStoreFrom(0, sessions, nil)
			desired := map[string]bool{}
			if tt.desired {
				desired["worker-1"] = true
			}

			recoverInertSessions(sp, inertTestCfg(), beads.SessionStore{Store: backing}, sessions, desired, map[string]time.Time{}, base.Add(time.Hour), &bytes.Buffer{})

			if n := sp.CountCalls("Peek", "worker-1"); n != 0 {
				t.Fatalf("ineligible session must clear without peek; peeks=%d", n)
			}
			stored, err := backing.Get("s-1")
			if err != nil {
				t.Fatalf("load stored session: %v", err)
			}
			if got := stored.Metadata[sessionpkg.InertRecoveryFingerprintKey]; got != "" {
				t.Fatalf("stale marker was not cleared, got %q", got)
			}
		})
	}
}

// A session the orchestrator does not want running is ignored entirely.
func TestRecoverInertSessions_SkipsNonDesired(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	sp := inertFake(t, "worker-1", base, codexTransportInertPane)
	cfg := inertTestCfg()
	sessions := []beads.Bead{inertSession("s-1", "worker-1", "polecat")}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}

	recoverInertSessions(sp, cfg, store, sessions, map[string]bool{}, map[string]time.Time{}, base.Add(time.Hour), &bytes.Buffer{})
	if n := sp.CountCalls("Peek", "worker-1"); n != 0 {
		t.Fatalf("non-desired session must be skipped before peek; peeks=%d", n)
	}
	if n := sp.CountCalls("Nudge", "worker-1"); n != 0 {
		t.Fatalf("non-desired session must not be nudged; nudges=%d", n)
	}
}
