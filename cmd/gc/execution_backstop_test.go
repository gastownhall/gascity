package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// The execution-backstop rows. A claim that never becomes execution is the
// residual failure after the trigger handoff is deterministic: the agent ends
// its turn holding the bead, and nothing in the fleet re-delivers the claim
// nudge because every existing backstop clears the instant the bead flips to
// in_progress. These rows own the rule that it converges in bounded minutes AND
// that a working agent never sees a keystroke.

type executionBackstopFixture struct {
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	session  beads.Bead
	work     beads.Bead
	rec      *events.Fake
	drained  []string
	now      time.Time
	stdout   bytes.Buffer
	sessName string
}

func newExecutionBackstopFixture(t *testing.T) *executionBackstopFixture {
	t.Helper()
	f := &executionBackstopFixture{
		sessName: "test-city--worker-1",
		now:      time.Now().UTC(),
		rec:      events.NewFake(),
		sp:       runtime.NewFake(),
	}
	f.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:  "worker",
			Nudge: "gc hook --claim --drain-ack --json",
		}},
	}
	f.store = beads.NewMemStore()
	session, err := f.store.Create(beads.Bead{
		Title:  "session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": f.sessName,
			"template":     "worker",
			"pool_managed": "true",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("seeding the session bead: %v", err)
	}
	f.session = session
	work, err := f.store.Create(beads.Bead{
		Title:    "claimed step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root-1"},
	})
	if err != nil {
		t.Fatalf("seeding the work bead: %v", err)
	}
	// Claim it the way the hook does, so the row under test is a real
	// in-progress assignment rather than a hand-built status string.
	inProgress := "in_progress"
	if err := f.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("claiming the work bead: %v", err)
	}
	claimed, err := f.store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-reading the claimed work bead: %v", err)
	}
	f.work = claimed
	if err := f.sp.Start(context.Background(), f.sessName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("starting the fake session: %v", err)
	}
	return f
}

// tick runs one reconcile tick of the backstop at the fixture's current clock.
func (f *executionBackstopFixture) tick(t *testing.T) {
	t.Helper()
	sessions, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	work, err := f.store.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("listing work: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for i := range work {
		stores[i] = f.store
	}
	nudgeStalledPoolExecution(f.sp, f.cfg, f.store, sessions, work, stores, refs, false, f.now, f.rec,
		func(sessionBead beads.Bead) error {
			f.drained = append(f.drained, strings.TrimSpace(sessionBead.Metadata["session_name"]))
			return nil
		}, &f.stdout)
}

// idleFor backdates the runtime's last-activity so the predicate observes an
// agent that has done nothing for d.
func (f *executionBackstopFixture) idleFor(t *testing.T, d time.Duration) {
	t.Helper()
	f.sp.SetActivity(f.sessName, f.now.Add(-d))
}

func (f *executionBackstopFixture) sessionMeta(t *testing.T, key string) string {
	t.Helper()
	current, err := f.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("re-reading the session bead: %v", err)
	}
	return strings.TrimSpace(current.Metadata[key])
}

func (f *executionBackstopFixture) nudgeCount() int {
	return strings.Count(f.stdout.String(), "execution-claim-nudge: nudged")
}

// TestExecutionBackstopNudgesAnIdleClaimHolderExactlyOnce is the core row: the
// slot holds an in-progress claim, the provider reports no activity past the
// grace, and the configured claim nudge is re-delivered once with the attempt
// durably reserved first.
func TestExecutionBackstopNudgesAnIdleClaimHolderExactlyOnce(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, do not nudge
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges after the first sighting = %d, want 0 (observe first)", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("persisted work marker = %q, want %q", got, f.work.ID)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the grace = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1", got)
	}

	// Inside the backoff nothing else is delivered.
	f.now = f.now.Add(time.Second)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges inside the backoff = %d, want still 1", got)
	}
}

// Control A: a WORKING agent. The provider reports recent activity, so the
// predicate holds — zero nudges and, just as importantly, zero writes, because a
// backstop that marks a busy session is a backstop that will eventually nudge it
// (the #312 churn failure this shape exists to avoid).
func TestExecutionBackstopIsSilentForAWorkingAgent(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, time.Second)

	for i := 0; i < 5; i++ {
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + time.Minute)
		f.idleFor(t, time.Second)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges to a working agent = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no write at all for a working agent", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeAtKey); got != "" {
		t.Fatalf("persisted timestamp = %q, want no write at all for a working agent", got)
	}
}

// Control B: the claim closes mid-grace. The marker is cleared and no nudge is
// ever delivered — a different outcome from Control A (which writes nothing at
// all) and from the core row (which nudges).
func TestExecutionBackstopClearsWhenTheClaimCompletes(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got == "" {
		t.Fatal("the grace clock did not start")
	}

	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("closing the claimed work: %v", err)
	}
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges after the claim completed = %d, want 0", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want cleared", got)
	}
}

// Control C: a controller restart mid-backoff. The state machine lives on the
// session bead, so a fresh process resumes the same attempt count instead of
// replaying the sequence from zero — the #312/test-5il regression, which is what
// a purely in-memory grace map produced on every restart.
func TestExecutionBackstopDoesNotReplayAfterAControllerRestart(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges before the restart = %d, want 1", got)
	}

	// A restart keeps the store and the runtime; only in-process state is lost.
	f.stdout.Reset()
	f.now = f.now.Add(time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges immediately after a restart = %d, want 0 (the backoff is persisted)", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "1" {
		t.Fatalf("attempt count after a restart = %q, want the persisted 1", got)
	}
}

// TestExecutionBackstopEscalatesOnceWhenAttemptsAreExhausted: after the bounded
// attempts, the stall becomes an observable typed fact and the session is handed
// to the drain path that already converges (recycle -> dead-assignee reopen).
// Both happen exactly once, however many ticks follow.
func TestExecutionBackstopEscalatesOnceWhenAttemptsAreExhausted(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("delivered nudges = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
	}

	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}

	var stalled []events.Event
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			stalled = append(stalled, e)
		}
	}
	if len(stalled) != 1 {
		t.Fatalf("execution.step_stalled events = %d, want exactly 1", len(stalled))
	}
	if stalled[0].Subject != f.work.ID || stalled[0].SessionID != f.session.ID {
		t.Fatalf("stalled event = subject %q session %q, want the claimed bead and its holder", stalled[0].Subject, stalled[0].SessionID)
	}
	if len(f.drained) != 1 || f.drained[0] != f.sessName {
		t.Fatalf("drain requests = %v, want exactly one for %s", f.drained, f.sessName)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("delivered nudges after exhaustion = %d, want no further delivery", got)
	}
}

// TestExecutionStepStalledStaysOffTheExportAllowlist is the explicit egress
// decision, pinned rather than implied. execution.step_stalled is a controller
// liveness signal, not one of the four execution FACTS the export contract
// carries (which are validated on both sides for ref+run_id+step topology it
// does not have). Widening the default-deny allowlist is a separate, reviewable
// change; this row fails if it happens silently.
func TestExecutionStepStalledStaysOffTheExportAllowlist(t *testing.T) {
	if eventexport.IsAllowed(events.ExecutionStepStalled) {
		t.Fatal("execution.step_stalled is on the redacted-export allowlist; that is an egress-surface change and needs its own review")
	}
}

// TestExecutionBackstopEscalatesWhenTheAgentHasNoNudgeConfigured is the
// production row this backstop was missing. `agent.Nudge` is optional, and in a
// real city every workflow pool template leaves it unset — maintainer-city had
// it on 18 of 260 agents and on none of its four pool roles. The shared engine
// skipped empty content with a bare `continue`, so the state machine parked on
// its observe marker forever: it never reserved an attempt, never reached the
// attempt cap, and so never ran the drain that is the ONLY thing that releases
// the claim. A seat holding an in-progress bead still satisfies poolDesired, so
// no replacement spawns and the whole pool starves behind it. Observed: 20 of 21
// live markers frozen at count=0, the oldest claim held 3.5 days.
//
// With nothing to deliver there is nothing to wait for — the bounded attempts
// exist to give the agent a chance to ANSWER a nudge. The grace window still
// applies, and then it escalates.
func TestExecutionBackstopEscalatesWhenTheAgentHasNoNudgeConfigured(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = "" // the maintainer-city pool templates, verbatim
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, escalate nothing yet
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("observed work marker = %q, want %q", got, f.work.ID)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests inside the grace window = %v, want none", f.drained)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("delivered nudges with no configured nudge = %d, want 0", got)
	}
	if len(f.drained) != 1 || f.drained[0] != f.sessName {
		t.Fatalf("drain requests = %v, want exactly one for %s; stdout=%s", f.drained, f.sessName, f.stdout.String())
	}
	if f.sessionMeta(t, executionClaimNudgeStalledKey) == "" {
		t.Fatal("escalation was not latched; a later tick would drain the session all over again")
	}

	// Latched: however many ticks follow, exactly one drain and one event.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if len(f.drained) != 1 {
		t.Fatalf("drain requests after further ticks = %v, want exactly one", f.drained)
	}
	stalled := 0
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			stalled++
		}
	}
	if stalled != 1 {
		t.Fatalf("execution.step_stalled events = %d, want exactly 1", stalled)
	}
}
