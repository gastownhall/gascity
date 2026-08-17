package main

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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
			"session_name":     f.sessName,
			"template":         "worker",
			"pool_managed":     "true",
			"state":            "active",
			"generation":       "1",
			"instance_token":   "instance-1",
			"awake_started_at": f.now.Add(-time.Hour).Format(time.RFC3339),
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
	f.tickWithDrain(t, func(sessionBead beads.Bead, _ backstopTarget) (backstopResolution, error) {
		// Model beginSessionDrainInfo: repeated enqueue requests for a session
		// already in the in-memory tracker are successful no-ops.
		if len(f.drained) == 0 {
			f.drained = append(f.drained, strings.TrimSpace(sessionBead.Metadata["session_name"]))
		}
		return backstopResolutionOutstanding, nil
	})
}

func (f *executionBackstopFixture) tickWithDrain(t *testing.T, requestDrain executionStalledDrainRequester) {
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
		// Production passes the canonical demand store ref; the distributed
		// stalled fence treats it as load-bearing identity.
		refs[i] = "city"
	}
	nudgeStalledPoolExecution(f.sp, f.cfg, f.store, sessions, work, stores, refs, false, "", f.now, f.rec, requestDrain, &f.stdout)
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

func (f *executionBackstopFixture) stalledEventCount() int {
	count := 0
	for _, event := range f.rec.Events {
		if event.Type == events.ExecutionStepStalled {
			count++
		}
	}
	return count
}

func (f *executionBackstopFixture) runToExhaustion(t *testing.T, requestDrain executionStalledDrainRequester) {
	t.Helper()
	f.idleFor(t, 10*time.Minute)
	f.tickWithDrain(t, requestDrain) // first sighting starts the grace clock
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tickWithDrain(t, requestDrain)
	}
	f.now = f.now.Add(idleClaimNudgeBackoff)
	f.idleFor(t, 10*time.Minute)
	f.tickWithDrain(t, requestDrain)
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
	for key, want := range map[string]string{
		executionClaimNudgeGenerationKey:     "1",
		executionClaimNudgeInstanceTokenKey:  "instance-1",
		executionClaimNudgeAwakeStartedAtKey: f.now.Add(-time.Hour).Format(time.RFC3339),
		executionClaimNudgeAssigneeKey:       f.sessName,
		executionClaimNudgeRevisionKey:       strconv.FormatInt(f.work.Revision, 10),
		executionClaimNudgeClaimFenceKey:     strconv.FormatInt(f.work.ClaimFence, 10),
	} {
		if got := f.sessionMeta(t, key); got != want {
			t.Fatalf("persisted %s = %q, want %q", key, got, want)
		}
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

// A durable exhaustion latch belongs to one runtime incarnation, not to the
// reusable session bead ID. Re-waking the same bead while it still holds the
// same claim must clear the old authority and start a fresh observation window.
func TestExecutionBackstopDoesNotTransferLatchToNewSessionIncarnation(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.runToExhaustion(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		return backstopResolutionOutstanding, nil
	})
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got == "" {
		t.Fatal("precondition: exhaustion did not persist the stalled latch")
	}

	newAwake := f.now.Add(time.Minute).UTC().Format(time.RFC3339)
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		"generation":       "2",
		"instance_token":   "instance-2",
		"awake_started_at": newAwake,
	}); err != nil {
		t.Fatalf("advancing the session incarnation: %v", err)
	}

	drainCalls := 0
	f.now = f.now.Add(time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tickWithDrain(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		drainCalls++
		return backstopResolutionOutstanding, nil
	})
	if drainCalls != 0 {
		t.Fatalf("old-incarnation drain requests = %d, want 0", drainCalls)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got != "" {
		t.Fatalf("old stalled latch = %q, want cleared", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("old work marker = %q, want cleared with the stale incarnation", got)
	}

	// The following tick may observe the unchanged claim, but it must persist a
	// fresh zero-attempt marker tied to the new runtime epoch.
	f.tickWithDrain(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		drainCalls++
		return backstopResolutionOutstanding, nil
	})
	if drainCalls != 0 {
		t.Fatalf("new-incarnation observation requested a drain: %d", drainCalls)
	}
	for key, want := range map[string]string{
		executionClaimNudgeGenerationKey:     "2",
		executionClaimNudgeInstanceTokenKey:  "instance-2",
		executionClaimNudgeAwakeStartedAtKey: newAwake,
		executionClaimNudgeCountKey:          "0",
	} {
		if got := f.sessionMeta(t, key); got != want {
			t.Fatalf("fresh %s = %q, want %q", key, got, want)
		}
	}
	if got := f.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events = %d, want the old event exactly once", got)
	}
}

// ID+store is not claim identity: a close/reopen/re-claim can produce the same
// row, store, and assignee while representing brand-new work ownership. The
// revision/fence marker prevents the old exhaustion budget from draining it.
func TestExecutionBackstopDoesNotTransferLatchAcrossClaimABA(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.runToExhaustion(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		return backstopResolutionOutstanding, nil
	})
	oldRevision := f.sessionMeta(t, executionClaimNudgeRevisionKey)
	oldFence := f.sessionMeta(t, executionClaimNudgeClaimFenceKey)

	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("closing the old claim: %v", err)
	}
	if err := f.store.Reopen(f.work.ID); err != nil {
		t.Fatalf("reopening the claim row: %v", err)
	}
	inProgress := "in_progress"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("re-claiming the same row for the same assignee: %v", err)
	}
	fresh, err := f.store.Get(f.work.ID)
	if err != nil {
		t.Fatalf("reading the fresh claim incarnation: %v", err)
	}
	if strconv.FormatInt(fresh.Revision, 10) == oldRevision && strconv.FormatInt(fresh.ClaimFence, 10) == oldFence {
		t.Fatalf("test fixture did not advance either claim authority: revision=%d fence=%d", fresh.Revision, fresh.ClaimFence)
	}

	drainCalls := 0
	f.now = f.now.Add(time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tickWithDrain(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		drainCalls++
		return backstopResolutionOutstanding, nil
	})
	if drainCalls != 0 {
		t.Fatalf("ABA claim inherited a drain request: %d", drainCalls)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got != "" {
		t.Fatalf("old stalled latch after ABA = %q, want cleared", got)
	}

	f.tickWithDrain(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		drainCalls++
		return backstopResolutionOutstanding, nil
	})
	if got := f.sessionMeta(t, executionClaimNudgeRevisionKey); got != strconv.FormatInt(fresh.Revision, 10) {
		t.Fatalf("fresh revision marker = %q, want %d", got, fresh.Revision)
	}
	if got := f.sessionMeta(t, executionClaimNudgeClaimFenceKey); got != strconv.FormatInt(fresh.ClaimFence, 10) {
		t.Fatalf("fresh claim-fence marker = %q, want %d", got, fresh.ClaimFence)
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "0" {
		t.Fatalf("fresh claim attempt count = %q, want 0", got)
	}
	if got := f.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events = %d, want no ABA replay", got)
	}
}

// This is the exact revalidate-to-enqueue interleaving: exhausted() performs
// its first live claim read, then invokes the request boundary; the test closes
// the claim at that boundary before the retained guard can enqueue. The second
// live read must retire the latch and never invoke the enqueue callback.
func TestExecutionBackstopClosesRevalidateToEnqueueGap(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	requestCalls := 0
	enqueueCalls := 0
	f.runToExhaustion(t, func(sessionBead beads.Bead, target backstopTarget) (backstopResolution, error) {
		requestCalls++
		if err := f.store.Close(target.ID); err != nil {
			t.Fatalf("completing the claim in the drain boundary gap: %v", err)
		}
		info, err := sessionFrontDoor(f.store).Get(sessionBead.ID)
		if err != nil {
			return backstopResolutionHold, err
		}
		guard := executionStalledDrainActionGuard("", f.store, sessionFrontDoor(f.store), info, target)
		return guard(func(_ sessionpkg.Info) error {
			enqueueCalls++
			return nil
		})
	})

	if requestCalls != 1 {
		t.Fatalf("drain-boundary calls = %d, want exactly 1", requestCalls)
	}
	if enqueueCalls != 0 {
		t.Fatalf("enqueue calls after boundary completion = %d, want 0", enqueueCalls)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got != "" {
		t.Fatalf("stalled latch after boundary completion = %q, want cleared", got)
	}
	if got := f.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events = %d, want the observation exactly once", got)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges = %d, want the bounded %d", got, idleClaimNudgeMaxAttempts)
	}
}

func TestExecutionStalledDrainGuardRejectsNewSessionIncarnationAtActionBoundary(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	info, err := sessionFrontDoor(f.store).Get(f.session.ID)
	if err != nil {
		t.Fatalf("loading the original session incarnation: %v", err)
	}
	target := executionTarget(executionClaim{
		BeadID:     f.work.ID,
		RootID:     strings.TrimSpace(f.work.Metadata[beadmeta.RootBeadIDMetadataKey]),
		Assignee:   f.sessName,
		Revision:   f.work.Revision,
		ClaimFence: f.work.ClaimFence,
		Store:      f.store,
	}, info)
	guard := executionStalledDrainActionGuard("", f.store, sessionFrontDoor(f.store), info, target)

	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		"generation":       "2",
		"instance_token":   "instance-2",
		"awake_started_at": f.now.Add(time.Minute).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("advancing the session incarnation before the action: %v", err)
	}
	actionCalls := 0
	resolution, err := guard(func(_ sessionpkg.Info) error {
		actionCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if resolution != backstopResolutionClear {
		t.Fatalf("resolution = %v, want clear for a superseded incarnation", resolution)
	}
	if actionCalls != 0 {
		t.Fatalf("action calls = %d, want 0 for a superseded incarnation", actionCalls)
	}
}

func TestExecutionStalledDrainGuardUsesLiveSessionIncarnationBehindCache(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	cache := beads.NewCachingStoreForTest(f.store, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("priming session cache: %v", err)
	}
	sessFront := sessionFrontDoor(cache)
	expected, err := sessFront.Get(f.session.ID)
	if err != nil {
		t.Fatalf("loading cached original incarnation: %v", err)
	}
	target := executionTarget(executionClaim{
		BeadID:     f.work.ID,
		RootID:     strings.TrimSpace(f.work.Metadata[beadmeta.RootBeadIDMetadataKey]),
		Assignee:   f.sessName,
		Revision:   f.work.Revision,
		ClaimFence: f.work.ClaimFence,
		Store:      cache,
	}, expected)

	newAwake := f.now.Add(time.Minute).UTC().Format(time.RFC3339)
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		"generation":       "2",
		"instance_token":   "instance-2",
		"awake_started_at": newAwake,
	}); err != nil {
		t.Fatalf("advancing the backing session incarnation: %v", err)
	}
	stale, err := sessFront.Get(f.session.ID)
	if err != nil {
		t.Fatalf("loading cached session: %v", err)
	}
	if stale.Generation != expected.Generation || stale.InstanceToken != expected.InstanceToken {
		t.Fatalf("test cache unexpectedly refreshed: got generation/token %q/%q, want stale %q/%q",
			stale.Generation, stale.InstanceToken, expected.Generation, expected.InstanceToken)
	}
	live, err := sessFront.GetLive(f.session.ID)
	if err != nil {
		t.Fatalf("loading live session: %v", err)
	}
	if live.Generation != "2" || live.InstanceToken != "instance-2" || live.AwakeStartedAt != newAwake {
		t.Fatalf("live incarnation = generation/token/awake %q/%q/%q, want 2/instance-2/%q",
			live.Generation, live.InstanceToken, live.AwakeStartedAt, newAwake)
	}

	actionCalls := 0
	resolution, err := executionStalledDrainActionGuard("", f.store, sessFront, expected, target)(func(_ sessionpkg.Info) error {
		actionCalls++
		return nil
	})
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if resolution != backstopResolutionClear || actionCalls != 0 {
		t.Fatalf("stale-cache guard = resolution %v action calls %d, want clear/0", resolution, actionCalls)
	}
}

// A backend that cannot supply either Revision or ClaimFence cannot prove an
// ownership incarnation. It must hold without even starting a grace marker;
// legacy external bd stays safe until its revision-capable/native alignment is
// available.
func TestExecutionBackstopFailsClosedWithoutClaimAuthority(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(*executionBackstopFixture) (beads.Bead, beads.Store)
	}{
		{
			name: "missing revision and claim fence",
			prepare: func(f *executionBackstopFixture) (beads.Bead, beads.Store) {
				unfenced := f.work
				unfenced.Revision = 0
				unfenced.ClaimFence = 0
				return unfenced, f.store
			},
		},
		{
			name: "missing authoritative store handle",
			prepare: func(f *executionBackstopFixture) (beads.Bead, beads.Store) {
				return f.work, nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.idleFor(t, 10*time.Minute)
			sessions, err := loadSessionBeads(f.store)
			if err != nil {
				t.Fatalf("loading sessions: %v", err)
			}
			work, workStore := tt.prepare(f)
			drainCalls := 0
			nudgeStalledPoolExecution(
				f.sp, f.cfg, f.store, sessions,
				[]beads.Bead{work}, []beads.Store{workStore}, []string{""}, false, "",
				f.now, f.rec,
				func(beads.Bead, backstopTarget) (backstopResolution, error) {
					drainCalls++
					return backstopResolutionOutstanding, nil
				},
				&f.stdout,
			)
			if drainCalls != 0 || f.nudgeCount() != 0 {
				t.Fatalf("unproven claim produced drain=%d nudge=%d, want neither", drainCalls, f.nudgeCount())
			}
			if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
				t.Fatalf("unproven claim marker = %q, want no authority write", got)
			}
		})
	}
}

func TestExecutionBackstopFailsClosedOnMisalignedWorkAuthoritySnapshot(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	sessions, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatalf("loading sessions: %v", err)
	}

	nudgeStalledPoolExecution(
		f.sp, f.cfg, f.store, sessions,
		[]beads.Bead{f.work}, []beads.Store{f.store}, nil, false, "",
		f.now, f.rec,
		func(beads.Bead, backstopTarget) (backstopResolution, error) {
			t.Fatal("misaligned work authority snapshot requested a drain")
			return backstopResolutionOutstanding, nil
		},
		&f.stdout,
	)

	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("misaligned authority snapshot wrote work marker %q", got)
	}
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("misaligned authority snapshot delivered %d nudges", got)
	}
}

func TestRevalidateExecutionClaimFailsClosedWhenAuthorityIsIncomplete(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	valid := backstopTarget{
		ID:             f.work.ID,
		RootID:         strings.TrimSpace(f.work.Metadata[beadmeta.RootBeadIDMetadataKey]),
		Assignee:       f.sessName,
		WorkRevision:   f.work.Revision,
		WorkClaimFence: f.work.ClaimFence,
		Store:          f.store,
	}
	for _, tt := range []struct {
		name   string
		mutate func(*backstopTarget)
	}{
		{"missing work id", func(target *backstopTarget) { target.ID = "" }},
		{"missing assignee", func(target *backstopTarget) { target.Assignee = "" }},
		{"missing store", func(target *backstopTarget) { target.Store = nil }},
		{"missing revision and claim fence", func(target *backstopTarget) {
			target.WorkRevision = 0
			target.WorkClaimFence = 0
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := valid
			tt.mutate(&target)
			if got := revalidateExecutionClaim(target); got != backstopResolutionHold {
				t.Fatalf("revalidate incomplete authority = %v, want hold", got)
			}
		})
	}
}

func TestRequestExecutionStalledDrainFailsClosedWithoutSessionStore(t *testing.T) {
	cr := &CityRuntime{
		cfg:           &config.City{},
		sessionDrains: newDrainTracker(),
	}
	resolution, err := cr.requestExecutionStalledDrain(
		beads.Bead{ID: "session-missing-store"},
		backstopTarget{
			ID:             "work-1",
			Assignee:       "worker-1",
			WorkRevision:   1,
			WorkClaimFence: 1,
			Store:          beads.NewMemStore(),
		},
	)
	if err == nil {
		t.Fatal("missing session store returned no error; want unavailable authority")
	}
	if resolution != backstopResolutionHold {
		t.Fatalf("resolution = %v, want hold for unavailable session authority", resolution)
	}
	if got := cr.sessionDrains.get("session-missing-store"); got != nil {
		t.Fatalf("missing-store request enqueued a drain: %+v", got)
	}
}

// TestExecutionBackstopEscalatesOnceWhenAttemptsAreExhausted: after the bounded
// attempts, the stall becomes an observable typed fact and the session is handed
// to the drain path that already converges (recycle -> dead-assignee reopen).
// Both happen exactly once, however many ticks follow.
func TestExecutionBackstopEscalatesOnceWhenAttemptsAreExhausted(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.runToExhaustion(t, func(sessionBead beads.Bead, _ backstopTarget) (backstopResolution, error) {
		if len(f.drained) == 0 {
			f.drained = append(f.drained, strings.TrimSpace(sessionBead.Metadata["session_name"]))
		}
		return backstopResolutionOutstanding, nil
	})
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

// The escalation marker is the one-shot notification latch, not a promise that
// the in-memory drain exists. A transient request failure must be retried on the
// next matching-claim tick without replaying either nudges or the typed event.
func TestExecutionBackstopRetriesDrainAfterTransientRequestFailure(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	requestCalls := 0
	drainTracked := false
	requestDrain := func(beads.Bead, backstopTarget) (backstopResolution, error) {
		requestCalls++
		if requestCalls == 1 {
			return backstopResolutionHold, errors.New("transient session-store read")
		}
		drainTracked = true
		return backstopResolutionOutstanding, nil
	}

	f.runToExhaustion(t, requestDrain)
	if requestCalls != 1 || drainTracked {
		t.Fatalf("first drain attempt = calls %d tracked %v, want one failed request", requestCalls, drainTracked)
	}
	if got := f.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events after failure = %d, want exactly 1", got)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges after failure = %d, want capped at %d", got, idleClaimNudgeMaxAttempts)
	}

	// Reattachment may make the provider look freshly active, and another claim
	// may appear before the next tick. Neither changes the exact latched claim
	// whose convergence must be reconstructed.
	second, err := f.store.Create(beads.Bead{Title: "second claim", Type: "task"})
	if err != nil {
		t.Fatalf("creating a concurrent claim: %v", err)
	}
	inProgress := "in_progress"
	if err := f.store.Update(second.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("assigning a concurrent claim: %v", err)
	}
	f.now = f.now.Add(time.Second)
	f.sp.SetActivity(f.sessName, f.now)
	f.tickWithDrain(t, requestDrain)
	if requestCalls != 2 || !drainTracked {
		t.Fatalf("retry = calls %d tracked %v, want the next tick to reconstruct the drain", requestCalls, drainTracked)
	}
	if got := f.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events after retry = %d, want still 1", got)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges after retry = %d, want still %d", got, idleClaimNudgeMaxAttempts)
	}
}

// A latched session is re-driven from the assigned-work snapshot, but the live
// owning store is authoritative at the drain boundary. If the claim completed
// after the snapshot, reconstruction clears the stale marker and spares the seat.
func TestExecutionBackstopRevalidatesClaimBeforeReconstructingDrain(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.runToExhaustion(t, func(beads.Bead, backstopTarget) (backstopResolution, error) {
		return backstopResolutionOutstanding, nil
	})

	staleClaim, err := f.store.Get(f.work.ID)
	if err != nil {
		t.Fatalf("reading the stale claim snapshot: %v", err)
	}
	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("completing the claim after the snapshot: %v", err)
	}
	sessions, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	drainCalls := 0
	f.now = f.now.Add(time.Second)
	f.idleFor(t, 10*time.Minute)
	nudgeStalledPoolExecution(f.sp, f.cfg, f.store, sessions,
		[]beads.Bead{staleClaim}, []beads.Store{f.store}, []string{""}, false, "",
		f.now, f.rec, func(beads.Bead, backstopTarget) (backstopResolution, error) {
			drainCalls++
			return backstopResolutionOutstanding, nil
		}, &f.stdout)

	if drainCalls != 0 {
		t.Fatalf("drain requests after authoritative claim completion = %d, want 0", drainCalls)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got != "" {
		t.Fatalf("stalled latch after authoritative completion = %q, want cleared", got)
	}
	if got := f.stalledEventCount(); got != 1 {
		t.Fatalf("execution.step_stalled events after completion = %d, want still 1", got)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges after completion = %d, want still %d", got, idleClaimNudgeMaxAttempts)
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
