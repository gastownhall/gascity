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

// lastNudge returns the text of the most recent nudge the fake delivered to the
// seat under test, or "" if none was delivered.
func (f *executionBackstopFixture) lastNudge(t *testing.T) string {
	t.Helper()
	msg := ""
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method == "Nudge" && call.Name == f.sessName {
			msg = call.Message
		}
	}
	return msg
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

// TestExecutionBackstopFallsBackToTheDefaultNudgeThenEscalates is the
// production row this backstop was missing. `agent.Nudge` is optional, and in a
// real city every workflow pool template leaves it unset — maintainer-city had
// it on 18 of 260 agents and on none of its four pool roles — as do the named
// seats ga-lez12 rescues. Before the fallback, the shared engine skipped empty
// content with a bare `continue`, so the state machine parked on its observe
// marker forever: never an attempt, never the cap, never the drain that is the
// ONLY thing that releases the claim. A seat holding an in-progress bead still
// satisfies poolDesired, so no replacement spawns and the whole pool starves
// behind it. Observed: 20 of 21 live markers frozen at count=0, the oldest claim
// held 3.5 days.
//
// The fix is the default-nudge fallback the claim lane already carries: an agent
// that is KNOWN but configures no nudge is still nudged with defaultPoolClaimNudge
// through the bounded attempts, and only then — the seat having had its chance to
// answer — does it escalate to the drain.
func TestExecutionBackstopFallsBackToTheDefaultNudgeThenEscalates(t *testing.T) {
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

	// The bounded nudge phase delivers the DEFAULT claim nudge, not silence.
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("default-nudge deliveries = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
	}
	if msg := f.lastNudge(t); msg != defaultPoolClaimNudge {
		t.Fatalf("delivered nudge text = %q, want the default %q", msg, defaultPoolClaimNudge)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests before the attempts were spent = %v, want none", f.drained)
	}

	// Only after the bounded attempts are spent does it escalate to the drain.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if len(f.drained) != 1 || f.drained[0] != f.sessName {
		t.Fatalf("drain requests = %v, want exactly one for %s; stdout=%s", f.drained, f.sessName, f.stdout.String())
	}
	if f.sessionMeta(t, executionClaimNudgeStalledKey) == "" {
		t.Fatal("escalation was not latched; a later tick would drain the session all over again")
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

// TestExecutionBackstopDrainsColdWhenNoNudgeIsResolvable pins the one case the
// default-nudge fallback deliberately does NOT rescue: a seat whose template
// resolves to no agent at all, so there is genuinely no nudge to send. Parking
// on the observe marker forever would hold the seat's close gate open and starve
// the pool, so with nothing deliverable the engine's empty-content path escalates
// to the drain straight out of the grace window — no nudge, exactly one drain.
func TestExecutionBackstopDrainsColdWhenNoNudgeIsResolvable(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	// Point the seat at a template with no matching [agent], so both the
	// configured nudge and the default fallback resolve to nothing.
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{"template": "ghost-template"}); err != nil {
		t.Fatalf("re-stamping the session template: %v", err)
	}
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, escalate nothing yet
	if len(f.drained) != 0 {
		t.Fatalf("drain requests inside the grace window = %v, want none", f.drained)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("delivered nudges with no resolvable nudge = %d, want 0", got)
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

// asNamedSeat re-stamps the seeded session bead as a configured named
// interactive seat — the run-operator, an olivia PM, a design reviewer — rather
// than a pool slot: it clears pool_managed and sets the configured-named
// markers, exactly as session_beads.go does for an isConfiguredNamed session.
// The runtime session_name (the claim's assignee here) is unchanged, so
// resolution still finds the claim by identity.
func (f *executionBackstopFixture) asNamedSeat(t *testing.T) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		"pool_managed":              "",
		"configured_named_session":  "true",
		"configured_named_identity": "run-operator",
	}); err != nil {
		t.Fatalf("re-stamping the session bead as a named seat: %v", err)
	}
	current, err := f.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("re-reading the named-seat session bead: %v", err)
	}
	f.session = current
}

// TestExecutionBackstopRecoversAStalledNamedInteractiveSeat is the pilot-killer
// row (ga-lez12). The stall that aborted every pilot run happened on an
// interactive Claude-harness seat — the run-operator holding finalize-work, an
// olivia PM holding canonicalize-issue — not on a pool slot. Those seats carry
// configured_named_session, and pool_managed is explicitly cleared for them
// (session_beads.go), so the pool-only governs scope left them with NO recovery:
// the seat held the in-progress step, made zero progress, the step's retry
// exhausted (on_exhausted=hard_fail), and the scope aborted (gc.on_fail=
// abort_scope). This row proves the same bounded observe -> nudge -> backoff ->
// drain recovery now covers a stalled named seat, converging it onto the
// recycle -> dead-assignee reopen -> re-attempt chain instead of hard-failing.
func TestExecutionBackstopRecoversAStalledNamedInteractiveSeat(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.asNamedSeat(t)
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, do not nudge yet
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("named seat grace clock did not start: work marker = %q, want %q", got, f.work.ID)
	}

	// The bounded nudge phase re-delivers the seat's own claim nudge.
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("delivered nudges to a stalled named seat = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
	}

	// Exhaustion hands the seat to the drain that already converges.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if len(f.drained) != 1 || f.drained[0] != f.sessName {
		t.Fatalf("drain requests for a stalled named seat = %v, want exactly one for %s; stdout=%s", f.drained, f.sessName, f.stdout.String())
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

// TestExecutionBackstopHoldsWhileAHumanIsAttached is the never-act-under-a-
// human's-hands guard (ga-lez12). A named interactive seat is exactly the kind
// of session an operator attaches to and drives by hand; the run-operator and
// the reviewers are attended for long stretches. While a terminal is attached
// the seat can hold an in-progress claim and report no I/O activity for many
// minutes — a human reading, thinking, or typing slowly — yet nudging it would
// inject keystrokes into the operator's session and draining it would rip the
// session out from under them. The backstop must HOLD (never nudge, never drain,
// and write no pacing state) for as long as the attach lasts, then resume its
// ordinary cadence once the human detaches.
func TestExecutionBackstopHoldsWhileAHumanIsAttached(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.asNamedSeat(t)
	f.sp.SetAttached(f.sessName, true) // a human is driving this seat by hand

	// Drive well past the grace window AND the full attempt budget AND the
	// exhaustion that would otherwise drain: an attached seat sees none of it.
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges to an attached seat = %d, want 0 (never act under a human's hands); stdout=%s", got, f.stdout.String())
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests for an attached seat = %v, want none", f.drained)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write while attached", got)
	}
}

// TestExecutionBackstopDoesNotDrainAnIntermittentlyWorkingSeat is the
// activity-decay row (ga-lez12). A human-paced interactive seat works in bursts:
// it answers a nudge, runs for a bit, then pauses to read or think. Each pause
// can exceed the grace window, so a bounded nudge->backoff->drain machine that
// only clears its attempt count when the claim LEAVES in_progress will march a
// legitimately-working seat to the drain on cumulative quiet alone. The fix
// re-arms the window whenever the runtime reports fresh activity SINCE the last
// attempt: a seat that keeps doing work between nudges is alive and must not be
// force-drained. A genuinely dead seat shows no such activity and still drains.
func TestExecutionBackstopDoesNotDrainAnIntermittentlyWorkingSeat(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.asNamedSeat(t)

	f.idleFor(t, 10*time.Minute)
	f.tick(t) // first sighting: start the grace clock

	// Far more cycles than the attempt cap. Each cycle the seat is quiet right
	// now (past the grace) but showed activity since the last attempt — the
	// signature of a seat working in bursts. Without decay the attempt count
	// reaches the cap within maxAttempts cycles and the seat is drained.
	for i := 0; i < idleClaimNudgeMaxAttempts+4; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.sp.SetActivity(f.sessName, f.now.Add(-(idleClaimNudgeGrace + time.Second)))
		f.tick(t)
	}

	if len(f.drained) != 0 {
		t.Fatalf("drain requests for an intermittently-working seat = %v, want none (renewed activity re-arms the window); stdout=%s", f.drained, f.stdout.String())
	}
	stalled := 0
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			stalled++
		}
	}
	if stalled != 0 {
		t.Fatalf("execution.step_stalled events = %d, want 0 for a working seat; stdout=%s", stalled, f.stdout.String())
	}
}

// TestExecutionBackstopFallsBackToTheDefaultNudgeForANamedSeatThenResumes is the
// stall -> nudge -> resume row (ga-lez12). The named seats that stalled the
// pilot (run-operator, olivia, the reviewers) configure NO [agent] nudge, and
// the execution lane read the raw configured nudge with no default fallback — so
// widening the backstop to cover named seats would have drained them COLD, never
// delivering the one keystroke the confirmed cause says resumes them. This row
// pins the fallback: with no configured nudge the lane still delivers the default
// claim nudge, the seat answers and completes its claim, and it is never drained.
func TestExecutionBackstopFallsBackToTheDefaultNudgeForANamedSeatThenResumes(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.asNamedSeat(t)
	f.cfg.Agents[0].Nudge = "" // the real named seats configure no nudge

	f.idleFor(t, 10*time.Minute)
	f.tick(t) // observe
	if len(f.drained) != 0 {
		t.Fatalf("drain requests inside the grace window = %v, want none", f.drained)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("default-nudge deliveries = %d, want exactly 1 (nudge before drain); stdout=%s", got, f.stdout.String())
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests after the first default nudge = %v, want none yet", f.drained)
	}
	if msg := f.lastNudge(t); msg != defaultPoolClaimNudge {
		t.Fatalf("delivered nudge text = %q, want the default %q", msg, defaultPoolClaimNudge)
	}

	// The seat answers: it executes and completes the claim. The backstop clears
	// and never drains.
	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("completing the claimed work: %v", err)
	}
	f.now = f.now.Add(idleClaimNudgeBackoff + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if len(f.drained) != 0 {
		t.Fatalf("drain requests after the seat resumed and completed = %v, want none", f.drained)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want cleared after completion", got)
	}
}
