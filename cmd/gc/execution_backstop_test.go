package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	sessionhybrid "github.com/gastownhall/gascity/internal/runtime/hybrid"
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
	provider runtime.Provider
	session  beads.Bead
	work     beads.Bead
	rec      *events.Fake
	// recorder, when set, is the recorder the tick hands the backstop in
	// place of rec.
	recorder events.Recorder
	drained  []string
	now      time.Time
	stdout   bytes.Buffer
	sessName string

	// workStore, when set, is the store handle the assigned-work snapshot
	// carries for each claim (and so the one revalidate reads through).
	workStore beads.Store
	// sessionStore, when set, is the store the backstop reads and writes the
	// session row through, in place of store.
	sessionStore beads.Store
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
		Title: "claimed step",
		Type:  "task",
		// A graph step the hook claimed for this seat: the stamps
		// executionLifecycleCandidate reads.
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:  "root-1",
			beadmeta.StepIDMetadataKey:      "step-1",
			beadmeta.SessionIDMetadataKey:   session.ID,
			beadmeta.SessionNameMetadataKey: f.sessName,
		},
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
		if f.workStore != nil {
			stores[i] = f.workStore
		}
	}
	sp := runtime.Provider(f.sp)
	if f.provider != nil {
		sp = f.provider
	}
	var rec events.Recorder = f.rec
	if f.recorder != nil {
		rec = f.recorder
	}
	sessionStore := f.store
	if f.sessionStore != nil {
		sessionStore = f.sessionStore
	}
	nudgeStalledPoolExecution(sp, f.cfg, sessionStore, sessions, work, stores, refs, false, f.now, rec,
		func(sessionBead beads.Bead) error {
			f.drained = append(f.drained, strings.TrimSpace(sessionBead.Metadata["session_name"]))
			return nil
		}, &f.stdout)
}

// executionBackstopIdleSnapshotProvider is the fixture runtime with a scripted
// point-in-time idle snapshot. calls counts snapshots taken, each of which is
// a pane capture on the real tmux provider.
type executionBackstopIdleSnapshotProvider struct {
	*runtime.Fake
	idle  bool
	err   error
	calls int
}

func (p *executionBackstopIdleSnapshotProvider) SnapshotIdle(string) (bool, error) {
	p.calls++
	return p.idle, p.err
}

// idleFor backdates the runtime's last-activity so the predicate observes an
// agent that has done nothing for d.
func (f *executionBackstopFixture) idleFor(t *testing.T, d time.Duration) {
	t.Helper()
	f.sp.SetActivity(f.sessName, f.now.Add(-d))
}

// tickStale runs idleClaimNudgeMaxAttempts+3 ticks a full grace-plus-backoff
// apart, each with no runtime activity for ten minutes: enough, when nothing
// holds the backstop, to observe, spend every attempt and escalate.
func (f *executionBackstopFixture) tickStale(t *testing.T) {
	t.Helper()
	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}
}

// setSessionMeta stamps metadata on the session bead, e.g. the provider and
// transport the idle-snapshot gate reads.
func (f *executionBackstopFixture) setSessionMeta(t *testing.T, meta map[string]string) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.session.ID, meta); err != nil {
		t.Fatalf("stamping session metadata: %v", err)
	}
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

// A quiet pane clock is not proof that an interactive agent ended its turn:
// long model turns and quiet tool calls can leave tmux activity stale while
// the provider's busy indicator still says the turn is active. For a provider
// family whose busy indicator the scan reads, that stronger observation (a
// (false, nil) snapshot) must hold the backstop before it writes or nudges, as
// must a snapshot that fails, and the hold names the snapshot rather than the
// activity clock.
func TestExecutionBackstopIsSilentWhenIdleSnapshotReportsBusy(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
		err  error
		hold string
	}{
		{name: "claude busy indicator present", meta: map[string]string{"provider": "claude"}, hold: executionHoldSnapshotNotIdle},
		{name: "codex busy indicator present", meta: map[string]string{"provider": "codex"}, hold: executionHoldSnapshotNotIdle},
		// A custom alias resolves its family through builtin_ancestor, the
		// same ladder that sets the pane's GC_PROVIDER.
		{name: "claude-based alias busy indicator present", meta: map[string]string{"provider": "team-claude", "builtin_ancestor": "claude"}, hold: executionHoldSnapshotNotIdle},
		{name: "snapshot failed", meta: map[string]string{"provider": "claude"}, err: errors.New("snapshot unavailable"), hold: executionHoldSnapshotError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.setSessionMeta(t, tc.meta)
			f.provider = &executionBackstopIdleSnapshotProvider{Fake: f.sp, idle: false, err: tc.err}

			f.tickStale(t)

			if got := f.nudgeCount(); got != 0 {
				t.Fatalf("nudges without affirmative idle snapshot = %d, want 0; stdout=%s", got, f.stdout.String())
			}
			if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
				t.Fatalf("persisted work marker = %q, want no pacing state without affirmative idle snapshot", got)
			}
			if len(f.drained) != 0 {
				t.Fatalf("drain requests = %v, want none while the snapshot shows the busy indicator or fails", f.drained)
			}
			if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, tc.hold+" ") {
				t.Fatalf("hold breadcrumb = %q, want %s", got, tc.hold)
			}
		})
	}
}

func TestExecutionBackstopAdvancesWhenIdleSnapshotReportsIdle(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.setSessionMeta(t, map[string]string{"provider": "claude"})
	snap := &executionBackstopIdleSnapshotProvider{Fake: f.sp, idle: true}
	f.provider = snap
	f.idleFor(t, 10*time.Minute)

	f.tick(t)
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after affirmative idle snapshot = %d, want 1; stdout=%s", got, f.stdout.String())
	}
	if snap.calls == 0 {
		t.Fatal("the snapshot was never consulted, so this row did not exercise the gate it names")
	}
}

// A pane showing neither the busy indicator nor a ready prompt is not a running
// turn. An idle TUI does not repaint, so once a broadcast message or an
// alternate screen displaces its prompt, every later snapshot is indeterminate
// for as long as the claim sits abandoned, and a hold there would never
// release. Even for a family whose busy indicator the scan reads, the backstop
// keeps the activity clock: it nudges to the attempt cap and escalates once.
// The composites cmd/gc installs pass the leaf's sentinel through unchanged.
func TestExecutionBackstopKeepsTheActivityClockWhenTheSnapshotIsIndeterminate(t *testing.T) {
	indeterminate := fmt.Errorf("scanning pane: %w", runtime.ErrIdleIndeterminate)
	for _, tc := range []struct {
		name string
		meta map[string]string
		wrap func(leaf runtime.Provider) runtime.Provider
	}{
		{name: "claude", meta: map[string]string{"provider": "claude"}},
		{name: "codex", meta: map[string]string{"provider": "codex"}},
		{name: "claude-based alias", meta: map[string]string{"provider": "team-claude", "builtin_ancestor": "claude"}},
		{name: "claude through auto", meta: map[string]string{"provider": "claude"}, wrap: func(leaf runtime.Provider) runtime.Provider {
			return sessionauto.New(leaf, runtime.NewFake())
		}},
		{name: "claude through hybrid", meta: map[string]string{"provider": "claude"}, wrap: func(leaf runtime.Provider) runtime.Provider {
			return sessionhybrid.New(leaf, runtime.NewFake(), func(string) bool { return false })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.setSessionMeta(t, tc.meta)
			snap := &executionBackstopIdleSnapshotProvider{Fake: f.sp, err: indeterminate}
			f.provider = snap
			if tc.wrap != nil {
				f.provider = tc.wrap(snap)
			}

			f.tickStale(t)

			if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
				t.Fatalf("delivered nudges = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
			}
			if len(f.drained) != 1 || f.drained[0] != f.sessName {
				t.Fatalf("drain requests = %v, want exactly one for %s", f.drained, f.sessName)
			}
			if snap.calls == 0 {
				t.Fatal("the snapshot was never consulted, so this row did not exercise the gate it names")
			}
		})
	}
}

// The scan is known to read only some families' busy indicators. For any other
// session, and for any session over ACP, a snapshot is evidence of nothing, so
// those sessions keep the activity clock: the backstop nudges, exhausts its
// attempts and escalates, without paying for a snapshot.
func TestExecutionBackstopKeepsTheActivityClockWhereTheSnapshotCannotReadBusy(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
	}{
		{name: "no recorded provider"},
		{name: "grok", meta: map[string]string{"provider": "grok"}},
		{name: "kimi", meta: map[string]string{"provider": "kimi"}},
		{name: "claude over ACP", meta: map[string]string{"provider": "claude", "transport": "acp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			if tc.meta != nil {
				f.setSessionMeta(t, tc.meta)
			}
			snap := &executionBackstopIdleSnapshotProvider{Fake: f.sp, idle: false}
			f.provider = snap

			f.tickStale(t)

			if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
				t.Fatalf("delivered nudges = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
			}
			if len(f.drained) != 1 || f.drained[0] != f.sessName {
				t.Fatalf("drain requests = %v, want exactly one for %s", f.drained, f.sessName)
			}
			if snap.calls != 0 {
				t.Fatalf("idle snapshots taken = %d, want 0 for a session the snapshot cannot read as busy", snap.calls)
			}
		})
	}
}

// The composites cmd/gc installs (auto once any agent selects ACP, hybrid for
// tmux plus k8s) always implement IdleSnapshotProvider and report a routed
// backend that cannot take a snapshot as ErrInteractionUnsupported. That is a
// missing capability, not a running turn: the backstop keeps the activity
// clock and still nudges and escalates the abandoned claim, even for a claude
// session the snapshot would otherwise gate.
func TestExecutionBackstopTreatsAnUnsupportedSnapshotAsCapabilityAbsent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider func(f *executionBackstopFixture) runtime.Provider
	}{
		{name: "auto default route", provider: func(f *executionBackstopFixture) runtime.Provider {
			return sessionauto.New(f.sp, runtime.NewFake())
		}},
		{name: "auto ACP route", provider: func(f *executionBackstopFixture) runtime.Provider {
			p := sessionauto.New(runtime.NewFake(), f.sp)
			p.RouteACP(f.sessName)
			return p
		}},
		{name: "hybrid remote route", provider: func(f *executionBackstopFixture) runtime.Provider {
			return sessionhybrid.New(runtime.NewFake(), f.sp, func(string) bool { return true })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.setSessionMeta(t, map[string]string{"provider": "claude"})
			f.provider = tc.provider(f)

			f.tickStale(t)

			if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
				t.Fatalf("delivered nudges = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
			}
			if len(f.drained) != 1 || f.drained[0] != f.sessName {
				t.Fatalf("drain requests = %v, want exactly one for %s", f.drained, f.sessName)
			}
		})
	}
}

// TestExecutionBackstopNamesTheSnapshotGate: the snapshot gate leaves its own
// breadcrumb, so an operator can tell "the pane still shows the busy indicator"
// from "the agent printed output moments ago". The snapshot is a pane capture,
// so it is taken only once activity is stale, and the breadcrumb clears once
// it reads idle and the backstop acts.
func TestExecutionBackstopNamesTheSnapshotGate(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.setSessionMeta(t, map[string]string{"provider": "claude"})
	snap := &executionBackstopIdleSnapshotProvider{Fake: f.sp}
	f.provider = snap

	// Fresh activity: the activity clock holds, and no snapshot is taken.
	for i := 0; i < 3; i++ {
		f.idleFor(t, time.Second)
		f.tick(t)
		f.now = f.now.Add(time.Minute)
	}
	if snap.calls != 0 {
		t.Fatalf("idle snapshots taken while activity was fresh = %d, want 0", snap.calls)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldNotQuiet+" ") {
		t.Fatalf("hold breadcrumb with fresh activity = %q, want %s", got, executionHoldNotQuiet)
	}

	// Stale activity, but the pane still shows the busy indicator.
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldSnapshotNotIdle+" ") {
		t.Fatalf("hold breadcrumb for a busy snapshot = %q, want %s", got, executionHoldSnapshotNotIdle)
	}

	// The snapshot cannot be taken.
	snap.err = errors.New("capture failed")
	f.now = f.now.Add(time.Minute)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldSnapshotError+" ") {
		t.Fatalf("hold breadcrumb for a failed snapshot = %q, want %s", got, executionHoldSnapshotError)
	}

	// The snapshot reads idle: the backstop acts and the breadcrumb clears.
	snap.err = nil
	snap.idle = true
	f.now = f.now.Add(time.Minute)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb after an idle snapshot = %q, want cleared", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("observed work marker = %q, want %q once the snapshot affirms idle", got, f.work.ID)
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

// driveToEscalation spends the attempt budget on the fixture's claim and
// confirms the escalation drained the session exactly once.
func driveToEscalation(t *testing.T, f *executionBackstopFixture) {
	t.Helper()
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	f.now = f.now.Add(idleClaimNudgeBackoff)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if len(f.drained) != 1 {
		t.Fatalf("escalation precondition: drain requests = %v, want exactly 1; stdout=%s", f.drained, f.stdout.String())
	}
}

func (f *executionBackstopFixture) setInstanceToken(t *testing.T, token string) {
	t.Helper()
	if err := f.store.SetMetadata(f.session.ID, "instance_token", token); err != nil {
		t.Fatalf("setting instance_token: %v", err)
	}
}

func (f *executionBackstopFixture) stalledEvents() int {
	n := 0
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			n++
		}
	}
	return n
}

// TestExecutionBackstopReEscalatesForANewIncarnation: the stalled latch
// belongs to the incarnation it escalated. The latched drain stopped the
// runtime, but the supervisor restarted before the bead closed and released
// the claim, so boot woke the same row as a NEW incarnation still holding the
// same claim. A latch that ignores the incarnation keeps the backstop silent
// for the whole remaining life of that seat.
func TestExecutionBackstopReEscalatesForANewIncarnation(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.setInstanceToken(t, "incarnation-1")
	driveToEscalation(t, f)
	if got := f.sessionMeta(t, executionClaimNudgeStalledTokenKey); got != "incarnation-1" {
		t.Fatalf("latch token = %q, want the escalated incarnation", got)
	}

	f.setInstanceToken(t, "incarnation-2")
	f.now = f.now.Add(2 * time.Minute)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	// v5.10x: the new incarnation starts its own window; it does not inherit
	// the old one's spent grace and escalate on its first quiet tick.
	if len(f.drained) != 1 || f.sessionMeta(t, executionClaimNudgeTokenKey) != "incarnation-2" || f.sessionMeta(t, executionClaimNudgeCountKey) != "0" {
		t.Fatalf("first tick of a new incarnation: drains %v, marker token %q count %q; want no drain and a fresh observation for incarnation-2",
			f.drained, f.sessionMeta(t, executionClaimNudgeTokenKey), f.sessionMeta(t, executionClaimNudgeCountKey))
	}
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	f.now = f.now.Add(idleClaimNudgeBackoff)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if len(f.drained) != 2 {
		t.Fatalf("drain requests after a new incarnation idles out its own window on the same claim = %v, want a second escalation", f.drained)
	}
	if got := f.stalledEvents(); got != 2 {
		t.Fatalf("execution.step_stalled events = %d, want one per escalated incarnation", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledTokenKey); got != "incarnation-2" {
		t.Fatalf("latch token after re-escalation = %q, want the new incarnation", got)
	}

	// The re-stamped latch holds for the new incarnation exactly as the first
	// one did for the old.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if len(f.drained) != 2 {
		t.Fatalf("drain requests after further ticks on the new incarnation = %v, want still 2", f.drained)
	}
}

// TestExecutionBackstopReEscalatesWhenARestartLostTheLatchedDrain: the drain
// tracker is in-memory, so a supervisor restart between the escalation and the
// drain landing loses the drain while the runtime (and so the incarnation)
// survives, and boot re-adopts the row holding the same claim. The same
// incarnation stays latched inside the retry window, so an in-flight drain is
// never re-requested tick after tick, but a latch that outlives the window
// with the claim still held means the drain is gone: escalate again.
func TestExecutionBackstopReEscalatesWhenARestartLostTheLatchedDrain(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.setInstanceToken(t, "incarnation-1")
	driveToEscalation(t, f)
	latchedAt := f.now

	// Same incarnation, inside the window: latched on every tick.
	for f.now.Add(idleClaimNudgeBackoff).Before(latchedAt.Add(executionStalledLatchRetryAfter)) {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if len(f.drained) != 1 {
		t.Fatalf("drain requests inside the latch window = %v, want exactly 1", f.drained)
	}

	// The window has passed with the claim still held: the drain was lost.
	f.now = latchedAt.Add(executionStalledLatchRetryAfter)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if len(f.drained) != 2 {
		t.Fatalf("drain requests once the latch outlived its window = %v, want a second escalation", f.drained)
	}
	if got := f.stalledEvents(); got != 2 {
		t.Fatalf("execution.step_stalled events = %d, want 2", got)
	}

	// The re-written latch restarts the window.
	f.now = f.now.Add(2 * time.Minute)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if len(f.drained) != 2 {
		t.Fatalf("drain requests right after re-latching = %v, want still 2", f.drained)
	}
}

// TestExecutionBackstopClearsTheLatchTokenWithTheClaim: the incarnation stamp
// is part of the latch, so it must not outlive the claim it latched.
func TestExecutionBackstopClearsTheLatchTokenWithTheClaim(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.setInstanceToken(t, "incarnation-1")
	driveToEscalation(t, f)

	closed := "closed"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("closing the claim: %v", err)
	}
	f.now = f.now.Add(time.Minute)
	f.tick(t)

	for _, key := range []string{executionClaimNudgeStalledKey, executionClaimNudgeStalledTokenKey} {
		if got := f.sessionMeta(t, key); got != "" {
			t.Fatalf("%s after the claim completed = %q, want cleared", key, got)
		}
	}
}

// TestExecutionBackstopRecordsWhichGateHeld: a hold leaves a durable
// breadcrumb naming its gate, so an operator can tell a backstop that is
// holding from one that is broken. Writes happen on transition only, and the
// breadcrumb clears once the backstop can act again.
func TestExecutionBackstopRecordsWhichGateHeld(t *testing.T) {
	f := newExecutionBackstopFixture(t)

	// Recent activity: the quiet gate holds.
	f.idleFor(t, time.Second)
	f.tick(t)
	first := f.sessionMeta(t, executionClaimHoldKey)
	if !strings.HasPrefix(first, executionHoldNotQuiet+" ") {
		t.Fatalf("hold breadcrumb for an active agent = %q, want %s", first, executionHoldNotQuiet)
	}
	// A standing hold is not rewritten every tick.
	f.now = f.now.Add(time.Minute)
	f.idleFor(t, time.Second)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); got != first {
		t.Fatalf("hold breadcrumb on a repeated hold = %q, want the original %q", got, first)
	}

	// Quiet long enough: the backstop acts and the breadcrumb clears.
	f.now = f.now.Add(time.Minute)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb after the backstop acted = %q, want cleared", got)
	}

	// The runtime is gone while the claim is still held.
	if err := f.sp.Stop(f.sessName); err != nil {
		t.Fatalf("stopping the fake session: %v", err)
	}
	f.now = f.now.Add(time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, backstopHoldRuntimeNotRunning+" ") {
		t.Fatalf("hold breadcrumb for a stopped claim holder = %q, want %s", got, backstopHoldRuntimeNotRunning)
	}

	// Once the claim is gone, a stopped runtime is not a hold at all.
	closed := "closed"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("closing the claim: %v", err)
	}
	f.now = f.now.Add(time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb for a stopped session with no claim = %q, want cleared", got)
	}
}

// TestExecutionBackstopRecordsAMultiClaimHold: a session holding several
// claims is held (the pacing state names one bead), and says so.
func TestExecutionBackstopRecordsAMultiClaimHold(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	second, err := f.store.Create(beads.Bead{Title: "second claimed step", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the second work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := f.store.Update(second.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("claiming the second work bead: %v", err)
	}
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldMultiClaim+" ") {
		t.Fatalf("hold breadcrumb for a multi-claim session = %q, want %s", got, executionHoldMultiClaim)
	}
}

// TestExecutionBackstopLeavesUnclaimedStoppedSessionsUntouched: a pool
// session that is simply asleep with no claim is not a backstop hold, and
// recording one would cost a session-bead write per sleep for every slot.
func TestExecutionBackstopLeavesUnclaimedStoppedSessionsUntouched(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	closed := "closed"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("closing the claim: %v", err)
	}
	if err := f.sp.Stop(f.sessName); err != nil {
		t.Fatalf("stopping the fake session: %v", err)
	}
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb for an unclaimed asleep session = %q, want none", got)
	}
}

// unreadableStore fails every point read, the way a store whose live handle is
// unreachable does. Revalidation must hold on it, not clear.
type unreadableStore struct {
	beads.Store
}

func (unreadableStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("live read unavailable")
}

// TestExecutionBackstopRecordsARevalidateHoldWithoutFlapping: when the live
// re-read before delivery cannot confirm the claim, the hold is named, and a
// standing hold is written once rather than cleared and rewritten every tick.
func TestExecutionBackstopRecordsARevalidateHoldWithoutFlapping(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.workStore = unreadableStore{Store: f.store}
	f.idleFor(t, 10*time.Minute)
	f.tick(t) // observe
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	first := f.sessionMeta(t, executionClaimHoldKey)
	if !strings.HasPrefix(first, backstopHoldRevalidate+" ") {
		t.Fatalf("hold breadcrumb when revalidation cannot read the claim = %q, want %s", first, backstopHoldRevalidate)
	}
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges under a revalidate hold = %d, want 0", got)
	}

	for i := 0; i < 3; i++ {
		f.now = f.now.Add(time.Minute)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
		if got := f.sessionMeta(t, executionClaimHoldKey); got != first {
			t.Fatalf("hold breadcrumb on tick %d of a standing revalidate hold = %q, want the original %q", i, got, first)
		}
	}

	// The live read recovers: the backstop delivers and the breadcrumb clears.
	f.workStore = nil
	f.now = f.now.Add(time.Minute)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges once revalidation recovers = %d, want 1; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
		t.Fatalf("hold breadcrumb after delivery = %q, want cleared", got)
	}
}

// TestExecutionBackstopNeverDrainsASeatExecutingSinceItsWake (v5.10x, the
// maintainer-city loop). Kills an escalation of a seat whose claimed step
// started this incarnation: with no nudge configured and its pane quiet past
// every window (a human-approval wait), a step_started of its claim, from
// it, since its last_woke_at, holds it as executing and nothing drains it;
// the same step started before its wake (an earlier incarnation's) is no
// evidence, and the seat escalates.
func TestExecutionBackstopNeverDrainsASeatExecutingSinceItsWake(t *testing.T) {
	for _, sinceWake := range []bool{true, false} {
		t.Run(map[bool]string{true: "started since the wake", false: "started before the wake"}[sinceWake], func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Agents[0].Nudge = ""
			woke := f.now.Add(-time.Minute)
			if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			at := woke.Add(10 * time.Second)
			if !sinceWake {
				at = woke.Add(-time.Minute)
			}
			f.rec.Record(events.Event{Type: events.ExecutionStepStarted, Ts: at, Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})
			for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
				f.idleFor(t, 10*time.Minute)
				f.tick(t)
				f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
			}
			switch {
			case sinceWake && (len(f.drained) != 0 || !strings.HasPrefix(f.sessionMeta(t, executionClaimHoldKey), executionHoldExecuting)):
				t.Fatalf("a seat executing since its wake: drains %v, hold %q; want no drain, held executing", f.drained, f.sessionMeta(t, executionClaimHoldKey))
			case !sinceWake && len(f.drained) != 1:
				t.Fatalf("a step started before the wake: drains %v, want one escalation", f.drained)
			}
		})
	}
}

// TestExecutionBackstopNewIncarnationInheritsNoSpentWindow (v5.10x, the
// maintainer-city loop's row). Kills a new incarnation escalating on its
// first quiet tick because the marker its predecessor left (same claim,
// observed hours ago, no token) reads as an expired window: it is observed
// afresh for the current incarnation, and only its own window escalates it.
func TestExecutionBackstopNewIncarnationInheritsNoSpentWindow(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	f.setInstanceToken(t, "incarnation-117")
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		executionClaimNudgeWorkKey: f.work.ID, executionClaimNudgeStoreRefKey: "", executionClaimNudgeCountKey: "0",
		executionClaimNudgeAtKey: f.now.Add(-8 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if len(f.drained) != 0 || f.sessionMeta(t, executionClaimNudgeTokenKey) != "incarnation-117" {
		t.Fatalf("first quiet tick of the incarnation: drains %v, marker token %q; want no drain and a fresh observation", f.drained, f.sessionMeta(t, executionClaimNudgeTokenKey))
	}
	f.now = f.now.Add(idleClaimNudgeGrace)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if len(f.drained) != 1 {
		t.Fatalf("after the incarnation's own grace with no nudge to send: drains %v, want one escalation", f.drained)
	}
}

// TestExecutionBackstopKeepsAStepStartedPastTheEventTail (mc-muv2x,
// maintainer-city 18:41Z). Kills a veto that reads only the event log's
// tail: a review seat started its step at its wake, worked 74 minutes, and
// was drained when its step_started had scrolled out of the tail. The first
// pass that sees the event (the seat still busy) stamps the row, and the
// veto reads the stamp, so with the event gone and the pane quiet past
// every window the seat is held executing and never drained.
func TestExecutionBackstopKeepsAStepStartedPastTheEventTail(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	f.setInstanceToken(t, "incarnation-1")
	woke := f.now.Add(-80 * time.Minute)
	if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	f.rec.Record(events.Event{Type: events.ExecutionStepStarted, Ts: woke.Add(2 * time.Minute), Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})
	f.idleFor(t, 0) // busy: no window, no veto, but the sighting stamps
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimStartedKey); got != "incarnation-1|"+f.work.ID {
		t.Fatalf("after a busy pass that saw the step start: %s = %q, want the incarnation and claim", executionClaimStartedKey, got)
	}
	f.rec = events.NewFake() // the event scrolled out of the tail
	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}
	if len(f.drained) != 0 || !strings.HasPrefix(f.sessionMeta(t, executionClaimHoldKey), executionHoldExecuting) {
		t.Fatalf("a seat whose step started past the tail: drains %v, hold %q; want no drain, held executing", f.drained, f.sessionMeta(t, executionClaimHoldKey))
	}
}

// TestExecutionBackstopKeepsTheStampAcrossAOnePassClaimGap (mc-muv2x
// review). Kills the claim marker's clear wiping the stamp: the cache folds
// blocked, deferred, review and testing into open, so a stamped claim can
// drop out of the in_progress snapshot for one pass (here, marked blocked
// during a wait) and come back after its step_started left the tail. The
// stamp survives the gap, and the seat is held executing, never drained.
func TestExecutionBackstopKeepsTheStampAcrossAOnePassClaimGap(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	f.setInstanceToken(t, "incarnation-1")
	woke := f.now.Add(-80 * time.Minute)
	if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	f.rec.Record(events.Event{Type: events.ExecutionStepStarted, Ts: woke.Add(2 * time.Minute), Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})
	f.idleFor(t, 0) // busy: the sighting stamps
	f.tick(t)
	blocked, inProgress := "blocked", "in_progress"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &blocked}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	f.idleFor(t, 0)
	f.tick(t) // no claim this pass: the marker clears
	if got := f.sessionMeta(t, executionClaimStartedKey); got != "incarnation-1|"+f.work.ID {
		t.Fatalf("after a pass with the claim out of the snapshot: %s = %q, want the stamp kept", executionClaimStartedKey, got)
	}
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	f.rec = events.NewFake() // the event scrolled out of the tail
	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}
	if len(f.drained) != 0 || !strings.HasPrefix(f.sessionMeta(t, executionClaimHoldKey), executionHoldExecuting) {
		t.Fatalf("a seat back from a one-pass claim gap: drains %v, hold %q; want no drain, held executing", f.drained, f.sessionMeta(t, executionClaimHoldKey))
	}
}

// TestExecutionBackstopIgnoresAnotherIncarnationsStartedStamp (mc-muv2x).
// Kills a stamp outliving the incarnation or claim it names: a stamp of the
// previous incarnation, or of this incarnation's earlier claim, with no step
// started for the current one, is no evidence. It earns no pool default nudge,
// and the seat escalates.
func TestExecutionBackstopIgnoresAnotherIncarnationsStartedStamp(t *testing.T) {
	for name, stamp := range map[string]func(f *executionBackstopFixture) string{
		"another incarnation": func(f *executionBackstopFixture) string { return "incarnation-1|" + f.work.ID },
		"another claim":       func(*executionBackstopFixture) string { return "incarnation-2|an-earlier-bead" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Agents[0].Nudge = ""
			f.setInstanceToken(t, "incarnation-2")
			if err := f.store.SetMetadata(f.session.ID, executionClaimStartedKey, stamp(f)); err != nil {
				t.Fatal(err)
			}
			f.tickStale(t)
			if len(f.drained) != 1 {
				t.Fatalf("with only a stamp of %s: drains %v, want one escalation", name, f.drained)
			}
			if got := f.nudgesDelivered(); len(got) != 0 {
				t.Fatalf("with only a stamp of %s: nudges %q, want none", name, got)
			}
		})
	}
}

// tailCountingRecorder counts its tail reads.
type tailCountingRecorder struct {
	*events.Fake
	tails int
}

func (r *tailCountingRecorder) ListTail(filter events.Filter, limit int) ([]events.Event, error) {
	r.tails++
	return r.Fake.ListTail(filter, limit)
}

// TestExecutionBackstopReadsNoTailForANonGraphClaim (mc-muv2x review). Kills
// a seat holding work that can never carry a step_started re-reading the
// event tail, up to 8 MiB, on every pass, forever. Only a claim the hook's
// lifecycle gate admits (hookClaimLifecycleCandidate: a rooted graph step,
// stamped with this seat's session, not a control kind) is ever stamped; any
// other reads no tail and escalates on the activity clock as before the
// veto, even rooted. A graph step's claim reads the tail until stamped, then
// stops.
func TestExecutionBackstopReadsNoTailForANonGraphClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		meta  map[string]string // overrides on the fixture's graph step
		tails int
	}{
		{name: "a graph step", tails: 1},
		{name: "a non-graph claim", meta: map[string]string{beadmeta.RootBeadIDMetadataKey: ""}},
		{name: "a rooted claim with no step", meta: map[string]string{beadmeta.StepIDMetadataKey: ""}},
		{name: "a rooted claim made outside the hook", meta: map[string]string{beadmeta.SessionIDMetadataKey: ""}},
		{name: "a rooted claim of another seat", meta: map[string]string{beadmeta.SessionIDMetadataKey: "another-session"}},
		{name: "a rooted claim of another session name", meta: map[string]string{beadmeta.SessionNameMetadataKey: "test-city--worker-9"}},
		{name: "a rooted control bead", meta: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Agents[0].Nudge = ""
			if len(tc.meta) > 0 {
				if err := f.store.SetMetadataBatch(f.work.ID, tc.meta); err != nil {
					t.Fatal(err)
				}
			}
			woke := f.now.Add(-time.Minute)
			if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			counting := &tailCountingRecorder{Fake: events.NewFake()}
			counting.Record(events.Event{Type: events.ExecutionStepStarted, Ts: woke.Add(time.Second), Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})
			f.recorder = counting
			for i := 0; i < 5; i++ {
				f.idleFor(t, 0) // busy passes
				f.tick(t)
				f.now = f.now.Add(10 * time.Second)
			}
			if counting.tails != tc.tails {
				t.Fatalf("%s read the event tail %d times over 5 passes, want %d", tc.name, counting.tails, tc.tails)
			}
			if tc.tails == 0 {
				f.tickStale(t)
				if len(f.drained) != 1 {
					t.Fatalf("%s, quiet: drains %v, want one escalation on the activity clock", tc.name, f.drained)
				}
			}
		})
	}
}

// The ga-ba7k1f rows. Execution evidence (v5.10x, mc-muv2x) proves a seat
// started its step this incarnation, not that it is still executing it: a
// model turn that dies on a provider error ("API Error: Connection lost
// mid-response") leaves the seat idle at its prompt with its stamp and its
// claim. On Oct 14 the stamp held six maintainer-city seats as executing with
// 0 nudges for 5.6 to 8 hours. These rows own the rule that the stamp vetoes
// the escalation (the drain) and never the idempotent nudge.

// startedSinceWake gives the fixture's seat the production shape the v5.10x
// veto protects: incarnation-1, woken 20 minutes ago, its claimed graph step
// started from it after the wake, and one busy pass that stamps the row
// (mc-muv2x). The pane reads busy at the fixture's clock.
func (f *executionBackstopFixture) startedSinceWake(t *testing.T) {
	t.Helper()
	f.setInstanceToken(t, "incarnation-1")
	woke := f.now.Add(-20 * time.Minute)
	if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
		t.Fatalf("setting last_woke_at: %v", err)
	}
	f.rec.Record(events.Event{Type: events.ExecutionStepStarted, Ts: woke.Add(time.Minute), Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})
	f.idleFor(t, 0)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimStartedKey); got != "incarnation-1|"+f.work.ID {
		t.Fatalf("precondition: %s = %q, want the seat stamped started", executionClaimStartedKey, got)
	}
}

// nudgesDelivered returns the text of every nudge the runtime received.
func (f *executionBackstopFixture) nudgesDelivered() []string {
	var out []string
	for _, call := range f.sp.Calls {
		if call.Method == "Nudge" {
			out = append(out, call.Message)
		}
	}
	return out
}

// TestExecutionBackstopNudgesAStartedSeatWhoseTurnDied (ga-ba7k1f, the Oct 14
// stall). Kills execution evidence vetoing the nudge: the seat started its
// step, its turn then died on an API error, and it sits at its prompt holding
// the claim. Once the pane has read quiet past the window it gets one claim
// nudge: the configured one, or the pool default when its template configures
// none, as no maintainer-city pool template does (the stamp vetoes the drain,
// so the nudge is the seat's only way back). The hook hands the seat its own
// claim, so the nudge is idempotent; a seat that answers is left alone. The
// default carries no --drain-ack (mc-ilr3b): a started seat must never ack a
// drain on the nudge's no-work answer.
func TestExecutionBackstopNudgesAStartedSeatWhoseTurnDied(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nudge string
		meta  map[string]string
		snap  error // set: a claude pane whose idle snapshot returns this
		want  string
	}{
		{name: "no nudge configured", want: defaultHeldClaimNudge},
		{name: "a configured nudge", nudge: "gc hook --claim --json", want: "gc hook --claim --json"},
		// A configured nudge that would ack the drain is replaced by the
		// default: the started rung never carries --drain-ack.
		{name: "a configured --drain-ack nudge", nudge: "gc hook --claim --drain-ack --json", want: defaultHeldClaimNudge},
		// The production pane: broadcasts have scrolled the idle prompt
		// away, so the snapshot can read neither a prompt nor a busy line.
		{name: "claude pane with its prompt displaced", meta: map[string]string{"provider": "claude"}, snap: runtime.ErrIdleIndeterminate, want: defaultHeldClaimNudge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Agents[0].Nudge = tc.nudge
			if tc.meta != nil {
				f.setSessionMeta(t, tc.meta)
			}
			if tc.snap != nil {
				f.provider = &executionBackstopIdleSnapshotProvider{Fake: f.sp, err: tc.snap}
			}
			f.startedSinceWake(t)

			died := f.now // the API error ends the turn; the pane goes quiet
			f.sp.SetActivity(f.sessName, died)
			f.now = died.Add(idleClaimNudgeGrace + 5*time.Second)
			f.tick(t) // quiet past the grace: the window starts
			f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
			f.tick(t) // the window has run

			want := tc.want
			if want == defaultHeldClaimNudge {
				want = fmt.Sprintf(defaultHeldClaimNudge, f.work.ID) // the started-rung text names the claim
			}
			if got := f.nudgesDelivered(); len(got) != 1 || got[0] != want || strings.Contains(got[0], "--drain-ack") {
				t.Fatalf("nudges delivered = %q, want exactly [%q], with no --drain-ack; stdout=%s", got, want, f.stdout.String())
			}
			if len(f.drained) != 0 || f.stalledEvents() != 0 {
				t.Fatalf("drains %v, step_stalled %d; want neither for a seat with execution evidence", f.drained, f.stalledEvents())
			}
			if got := f.sessionMeta(t, executionClaimHoldKey); got != "" {
				t.Fatalf("hold breadcrumb after the nudge = %q, want cleared", got)
			}

			// The seat answers and works again: nothing more is sent.
			for i := 0; i < 10; i++ {
				f.now = f.now.Add(idleClaimNudgeBackoff)
				f.idleFor(t, 5*time.Second)
				f.tick(t)
			}
			if got := f.nudgesDelivered(); len(got) != 1 {
				t.Fatalf("nudges delivered once the seat worked again = %d, want still 1", len(got))
			}
		})
	}
}

// TestExecutionBackstopNeverDrainsAStartedSeatWaitingOnAHuman (ga-ba7k1f,
// keeps v5.10x). A human-approval step holds its claim at an idle prompt until
// a person replies, which can take days. Its pane looks exactly like a turn
// that died, so it is nudged too, and that is safe: the hook hands it back its
// own claim, it re-checks for the reply and ends its turn again. The nudges
// stop at the attempt cap, and the escalation stays vetoed: never drained, no
// step_stalled, held as executing for the whole wait.
func TestExecutionBackstopNeverDrainsAStartedSeatWaitingOnAHuman(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	f.startedSinceWake(t)

	quietSince := f.now
	delivered := 0
	for end := f.now.Add(8 * time.Hour); f.now.Before(end); f.now = f.now.Add(time.Minute) {
		f.sp.SetActivity(f.sessName, quietSince)
		f.tick(t)
		if n := len(f.nudgesDelivered()); n > delivered {
			// It answers: a short re-check turn, then idle again.
			delivered = n
			quietSince = f.now.Add(20 * time.Second)
		}
	}

	if delivered != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges over an 8h approval wait = %d, want the attempt cap %d; stdout=%s", delivered, idleClaimNudgeMaxAttempts, f.stdout.String())
	}
	if len(f.drained) != 0 || f.stalledEvents() != 0 {
		t.Fatalf("drains %v, step_stalled %d; want neither for a seat waiting on a human", f.drained, f.stalledEvents())
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldExecuting+" ") {
		t.Fatalf("hold breadcrumb once the nudges are spent = %q, want %s", got, executionHoldExecuting)
	}
}

// TestExecutionBackstopNeverNudgesAStartedSeatWhileItWorks (ga-ba7k1f). Kills
// the nudge reaching a seat that is visibly working: a long tool run whose
// output keeps the pane active, and a claude seat whose pane has gone quiet
// while its busy indicator still shows. For two hours neither is nudged,
// drained, or even observed, and the hold names the gate.
func TestExecutionBackstopNeverNudgesAStartedSeatWhileItWorks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		meta     map[string]string
		busySnap bool
		quietFor time.Duration
		hold     string
	}{
		{name: "tool output streaming", quietFor: 5 * time.Second, hold: executionHoldNotQuiet},
		{name: "claude busy indicator over a quiet pane", meta: map[string]string{"provider": "claude"}, busySnap: true, quietFor: 10 * time.Minute, hold: executionHoldSnapshotNotIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Agents[0].Nudge = ""
			if tc.meta != nil {
				f.setSessionMeta(t, tc.meta)
			}
			if tc.busySnap {
				f.provider = &executionBackstopIdleSnapshotProvider{Fake: f.sp, idle: false}
			}
			f.startedSinceWake(t)

			for end := f.now.Add(2 * time.Hour); f.now.Before(end); f.now = f.now.Add(30 * time.Second) {
				f.idleFor(t, tc.quietFor)
				f.tick(t)
			}

			if got := f.nudgesDelivered(); len(got) != 0 {
				t.Fatalf("nudges to a working seat = %q, want none", got)
			}
			if len(f.drained) != 0 {
				t.Fatalf("drain requests for a working seat = %v, want none", f.drained)
			}
			if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
				t.Fatalf("pacing marker = %q, want no window opened on a working seat", got)
			}
			if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, tc.hold+" ") {
				t.Fatalf("hold breadcrumb = %q, want %s", got, tc.hold)
			}
		})
	}
}

// TestExecutionBackstopNeverReentersTheV2c9DrainLoop (ga-ba7k1f, the v5.10x
// regression row). The v2c9 loop: a workflow seat with no nudge configured
// woke, started its human-approval step, went quiet, was escalated 0/3 after
// the grace, drained, and woken onto the same claim again, about 56 times per
// seat. Here the seat runs five incarnations of that cycle, each one woken onto
// the claim with its predecessor's spent marker still on the row. Letting the
// nudge through must not let the drain back in: every incarnation gets its own
// bounded nudges, and none is drained.
func TestExecutionBackstopNeverReentersTheV2c9DrainLoop(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	for incarnation := 1; incarnation <= 5; incarnation++ {
		f.setInstanceToken(t, fmt.Sprintf("incarnation-%d", incarnation))
		woke := f.now
		if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
			t.Fatalf("setting last_woke_at: %v", err)
		}
		f.rec.Record(events.Event{Type: events.ExecutionStepStarted, Ts: woke.Add(10 * time.Second), Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})
		before := len(f.nudgesDelivered())
		for end := woke.Add(time.Hour); f.now.Before(end); f.now = f.now.Add(time.Minute) {
			f.sp.SetActivity(f.sessName, woke.Add(20*time.Second)) // started, then quiet
			f.tick(t)
		}
		if got := len(f.nudgesDelivered()) - before; got != idleClaimNudgeMaxAttempts {
			t.Fatalf("incarnation %d: nudges = %d, want its own attempt cap %d", incarnation, got, idleClaimNudgeMaxAttempts)
		}
		if len(f.drained) != 0 {
			t.Fatalf("incarnation %d: drains %v, want none: the v2c9 loop is back", incarnation, f.drained)
		}
	}
	if got := f.stalledEvents(); got != 0 {
		t.Fatalf("execution.step_stalled events = %d, want none", got)
	}
}

// TestExecutionBackstopRidesOutNonTurnPaneOutput (ga-ba7k1f, the flap). On the
// stalled seats execution_claim_hold flipped between executing and not_quiet
// every ~15 minutes, and every seat flipped in the same second: the host's
// disk monitor wall(1)s an alert into every tty, tmux panes included, and
// #{window_activity} cannot tell that output from a model turn. Such a burst
// may hold a pass as not_quiet, but it never touches the pacing (the window
// and the attempt count stand), so the stalled seat is nudged as soon as its
// pane has been quiet for the grace again.
func TestExecutionBackstopRidesOutNonTurnPaneOutput(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	f.startedSinceWake(t)
	died := f.now
	f.sp.SetActivity(f.sessName, died)
	f.now = died.Add(idleClaimNudgeGrace + 5*time.Second)
	f.tick(t) // the window starts
	windowAt := f.sessionMeta(t, executionClaimNudgeAtKey)

	// A broadcast lands just before the nudge falls due.
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	broadcast := f.now.Add(-2 * time.Second)
	f.sp.SetActivity(f.sessName, broadcast)
	f.tick(t)
	if got := f.nudgesDelivered(); len(got) != 0 {
		t.Fatalf("nudges on the pass with fresh output = %q, want none", got)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldNotQuiet+" ") {
		t.Fatalf("hold breadcrumb on the broadcast pass = %q, want %s", got, executionHoldNotQuiet)
	}
	if f.sessionMeta(t, executionClaimNudgeAtKey) != windowAt || f.sessionMeta(t, executionClaimNudgeCountKey) != "0" {
		t.Fatalf("pacing after a not_quiet pass: at %q count %q, want the window %q and count 0 untouched",
			f.sessionMeta(t, executionClaimNudgeAtKey), f.sessionMeta(t, executionClaimNudgeCountKey), windowAt)
	}

	// Quiet for a grace after the burst: nudged.
	f.now = broadcast.Add(idleClaimNudgeGrace + time.Second)
	f.tick(t)
	if got := f.nudgesDelivered(); len(got) != 1 {
		t.Fatalf("nudges once the pane is quiet again = %d, want 1; stdout=%s", len(got), f.stdout.String())
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests = %v, want none", f.drained)
	}
}

// stampRefusingStore fails every write of the execution-evidence stamp, the
// way a store hiccup can on the pass that first sees a step start.
type stampRefusingStore struct {
	beads.Store
}

func (s stampRefusingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs[executionClaimStartedKey]; ok {
		return errors.New("store unavailable")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestExecutionBackstopNeverDrainsAStartedSeatWhoseStampDidNotPersist
// (ga-ba7k1f). Kills the drain reaching a started seat through the
// nothing-to-send shortcut: the seat's step_started is in the tail, so each
// pass sees it started, but the stamp never lands, and the pool default nudge
// is chosen from the stamp. With no nudge configured there is then nothing to
// send, and the engine would escalate on the spot. Execution evidence vetoes
// that escalation like any other: the seat is held, never drained.
func TestExecutionBackstopNeverDrainsAStartedSeatWhoseStampDidNotPersist(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = ""
	f.sessionStore = stampRefusingStore{Store: f.store}
	f.setInstanceToken(t, "incarnation-1")
	woke := f.now.Add(-20 * time.Minute)
	if err := f.store.SetMetadata(f.session.ID, "last_woke_at", woke.Format(time.RFC3339)); err != nil {
		t.Fatalf("setting last_woke_at: %v", err)
	}
	f.rec.Record(events.Event{Type: events.ExecutionStepStarted, Ts: woke.Add(time.Minute), Actor: f.session.ID, SessionID: f.session.ID, Subject: f.work.ID})

	f.tickStale(t)

	if got := f.sessionMeta(t, executionClaimStartedKey); got != "" {
		t.Fatalf("%s = %q, want the stamp never persisted", executionClaimStartedKey, got)
	}
	if len(f.drained) != 0 || f.stalledEvents() != 0 {
		t.Fatalf("drains %v, step_stalled %d; want neither for a seat seen started", f.drained, f.stalledEvents())
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldExecuting+" ") {
		t.Fatalf("hold breadcrumb = %q, want %s", got, executionHoldExecuting)
	}
}
