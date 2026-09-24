package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestBackstopSeatAwaitsHumanInput(t *testing.T) {
	for _, tt := range []struct {
		name        string
		newProvider func() runtime.Provider
		want        backstopSeatWait
		wantEpisode string
	}{
		{
			name:        "idle seat is nudgeable",
			newProvider: func() runtime.Provider { return runtime.NewFake() },
			want:        backstopSeatReady,
		},
		{
			name: "seat at an approval prompt is refused",
			newProvider: func() runtime.Provider {
				f := runtime.NewFake()
				f.SetPendingInteraction(backstopSession, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval", Prompt: "approve?"})
				return f
			},
			want: backstopSeatPrompted,
			// The episode is what lets the caller report one notice per prompt
			// instead of one per reconcile tick.
			wantEpisode: "r1",
		},
		{
			name:        "probe failure is refused, not assumed idle",
			newProvider: func() runtime.Provider { return runtime.NewFailFake() },
			want:        backstopSeatProbeFailed,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, episode, err := backstopSeatAwaitsHumanInput(tt.newProvider(), backstopSession)
			if got != tt.want {
				t.Fatalf("backstopSeatAwaitsHumanInput = %v, want %v", got, tt.want)
			}
			if episode != tt.wantEpisode {
				t.Fatalf("episode = %q, want %q", episode, tt.wantEpisode)
			}
			// The two refusals must stay distinguishable at the type level:
			// collapsing them to one bool is what made the probe-error case
			// impossible to pace differently from a human.
			if (tt.want == backstopSeatProbeFailed) != (err != nil) {
				t.Fatalf("err = %v, want non-nil only for a probe failure (wait=%v)", err, got)
			}
		})
	}
}

// probeFailProvider is a live seat whose interaction probe is persistently
// broken: IsRunning still reports it, so the backstop cannot shed it the way it
// sheds a stopped seat. NewFailFake cannot model this — it breaks IsRunning
// too, so the engine skips the seat before it ever probes.
type probeFailProvider struct {
	*runtime.Fake
	err error
}

// The nil interaction is the whole point of the double: this models a probe
// that cannot answer, which is exactly the case that must not be mistaken for
// "no prompt" and must not be paced like a human.
func (p probeFailProvider) Pending(string) (*runtime.PendingInteraction, error) { //nolint:unparam // the nil result is the modeled condition
	return nil, p.err
}

// backstopSession is the one seat every case in this file wires. Sharing it
// keeps the harness, the fake, and the assertions talking about the same seat
// without a parameter that can only ever carry this value.
const backstopSession = "worker-1"

// stubBackstopPredicate is a backstopPredicate double that governs one seat with
// permanently outstanding work and records what the engine did to its pacing
// state, so a test can assert on reservations and the terminal action rather
// than on a real lane's persistence.
type stubBackstopPredicate struct {
	target    backstopTarget
	text      string
	attempts  int
	last      time.Time
	reserveOK bool

	// notGoverned and resolutionOverride drive the early exits the per-seat
	// body takes before it ever probes. Those are the exits where the prompt
	// notice used to leak, so a test has to be able to reach them.
	notGoverned        bool
	resolutionOverride *backstopResolution

	reserved       []int
	exhaustedCalls int
	clearedCalls   int
	observedCalls  int
}

func (p *stubBackstopPredicate) governs(beads.Bead) bool { return !p.notGoverned }

func (p *stubBackstopPredicate) resolve(beads.Bead, string) (backstopTarget, backstopResolution) {
	if p.resolutionOverride != nil {
		return p.target, *p.resolutionOverride
	}
	return p.target, backstopResolutionOutstanding
}

func (p *stubBackstopPredicate) state(beads.Bead, backstopTarget) (bool, int, time.Time) {
	return true, p.attempts, p.last
}

func (p *stubBackstopPredicate) content(beads.Bead) string { return p.text }

func (p *stubBackstopPredicate) revalidate(backstopTarget) backstopResolution {
	return backstopResolutionOutstanding
}

func (p *stubBackstopPredicate) observe(beads.Store, *beads.Bead, backstopTarget, time.Time, io.Writer) {
	p.observedCalls++
}

func (p *stubBackstopPredicate) reserve(_ beads.Store, _ *beads.Bead, _ backstopTarget, attempts int, now time.Time, _ io.Writer) bool {
	p.reserved = append(p.reserved, attempts)
	if p.reserveOK {
		// Mirror what a real predicate persists, so a multi-tick test walks the
		// same ladder the engine would read back on the next reconcile.
		p.attempts = attempts
		p.last = now
	}
	return p.reserveOK
}

func (p *stubBackstopPredicate) exhausted(beads.Store, *beads.Bead, io.Writer) { p.exhaustedCalls++ }

func (p *stubBackstopPredicate) clear(beads.Store, *beads.Bead, io.Writer) { p.clearedCalls++ }

// backstopHarness wires one governed seat to the shared engine.
func backstopHarness(t *testing.T) ([]beads.Bead, beads.Store, *stubBackstopPredicate) {
	t.Helper()
	store := beads.NewMemStore()
	sessionBeads := []beads.Bead{{
		ID:       "gc-session-1",
		Metadata: map[string]string{"session_name": backstopSession},
	}}
	pred := &stubBackstopPredicate{
		target:    backstopTarget{ID: "gc-work-1", Store: store},
		text:      "please continue",
		reserveOK: true,
	}
	return sessionBeads, store, pred
}

func nudgeCalls(sp *runtime.Fake) int {
	return sp.CountCalls("Nudge", backstopSession) + sp.CountCalls("NudgeNow", backstopSession)
}

// The commit's stated invariant is that a refusal is taken BEFORE pred.reserve,
// so waiting on a human never consumes a bounded attempt. Nothing exercised the
// engine, so moving the check below reserve kept every test green.
func TestRunNudgeBackstopRefusesSeatAwaitingHumanInput(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(t.Context(), backstopSession, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction(backstopSession, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval", Prompt: "approve?"})

	sessionBeads, store, pred := backstopHarness(t)
	now := time.Now()
	pred.last = now.Add(-2 * idleClaimNudgeGrace) // past the grace window: the engine wants to nudge

	var out bytes.Buffer
	runNudgeBackstop(sp, store, sessionBeads, now, &out, t.Name(), pred)

	if got := nudgeCalls(sp); got != 0 {
		t.Fatalf("nudged a seat awaiting human input %d time(s): %#v", got, sp.SnapshotCalls())
	}
	if len(pred.reserved) != 0 {
		t.Fatalf("refusal consumed attempt(s) %v; a human must cost no bounded attempt", pred.reserved)
	}
	if pred.exhaustedCalls != 0 {
		t.Fatalf("refusal advanced the seat toward the terminal action (%d exhausted calls)", pred.exhaustedCalls)
	}
	if !strings.Contains(out.String(), "awaiting human input") {
		t.Fatalf("refusal must stay observable; out = %q", out.String())
	}
}

// A human may take as long as they take: the prompted skip must stay unbounded
// and must not march the seat to the terminal drain. This is the control for
// the probe-error test below — it pins that bounding the probe-error branch did
// not also start charging attempts to an operator.
func TestRunNudgeBackstopPromptedSeatNeverExhausts(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(t.Context(), backstopSession, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp.SetPendingInteraction(backstopSession, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval", Prompt: "approve?"})

	sessionBeads, store, pred := backstopHarness(t)
	now := time.Now()
	pred.last = now.Add(-2 * idleClaimNudgeGrace)

	var out bytes.Buffer
	for tick := 0; tick < idleClaimNudgeMaxAttempts+3; tick++ {
		runNudgeBackstop(sp, store, sessionBeads, now, &out, t.Name(), pred)
		now = now.Add(idleClaimNudgeBackoff)
	}

	if len(pred.reserved) != 0 {
		t.Fatalf("prompted seat consumed attempt(s) %v across ticks", pred.reserved)
	}
	if pred.exhaustedCalls != 0 {
		t.Fatalf("prompted seat reached the terminal action %d time(s) while a human was being waited on", pred.exhaustedCalls)
	}
	if got := nudgeCalls(sp); got != 0 {
		t.Fatalf("nudged a prompted seat %d time(s)", got)
	}
	// One notice per prompt, not one per reconcile tick: the tick rate belongs
	// to the reconciler, not to the operator being waited on.
	if got := strings.Count(out.String(), "awaiting human input"); got != 1 {
		t.Fatalf("awaiting-human notices = %d, want exactly 1 per prompt; out = %q", got, out.String())
	}
}

// A broken probe is not a human and must not be paid for like one. An unbounded
// pre-reserve skip parks the state machine forever: no attempt is reserved, the
// cap is never reached, and the execution backstop's terminal drain — the only
// thing that releases the claim — never runs.
func TestRunNudgeBackstopProbeFailureReachesTerminalAction(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(t.Context(), backstopSession, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp := probeFailProvider{Fake: fake, err: fmt.Errorf("session unavailable")}

	sessionBeads, store, pred := backstopHarness(t)
	now := time.Now()
	pred.last = now.Add(-2 * idleClaimNudgeGrace)

	var out bytes.Buffer
	for tick := 0; tick < idleClaimNudgeMaxAttempts+1; tick++ {
		runNudgeBackstop(sp, store, sessionBeads, now, &out, t.Name(), pred)
		now = now.Add(idleClaimNudgeBackoff)
	}

	if len(pred.reserved) == 0 {
		t.Fatalf("a persistently broken probe reserved no attempt: the ladder never advances and the drain is withheld forever; out = %q", out.String())
	}
	if pred.exhaustedCalls == 0 {
		t.Fatalf("a persistently broken probe never reached the terminal action after %d ticks; reserved = %v", idleClaimNudgeMaxAttempts+1, pred.reserved)
	}
	// Refusing the delivery is still correct — only the pacing changes.
	if got := nudgeCalls(fake); got != 0 {
		t.Fatalf("nudged a seat whose state could not be read %d time(s)", got)
	}
	if !strings.Contains(out.String(), "probing pending interaction") {
		t.Fatalf("probe failure must stay observable; out = %q", out.String())
	}
}

// promptsDuringDeliveryProvider answers the backstop's pre-probe and is
// prompted by the time the guarded delivery re-probes, which is the only way to
// reach the "prompted during delivery" arm: every other refusal in this lane is
// taken before pred.reserve runs.
type promptsDuringDeliveryProvider struct {
	*runtime.Fake
	probes int
}

func (p *promptsDuringDeliveryProvider) Pending(name string) (*runtime.PendingInteraction, error) {
	p.probes++
	if p.probes == 1 {
		return p.Fake.Pending(name)
	}
	return &runtime.PendingInteraction{RequestID: "r1", Kind: "approval", Prompt: "approve?"}, nil
}

// The one refusal in this lane that spends an attempt, and the only one whose
// three properties nothing else observes: the reservation is NOT refunded (the
// write-ahead is what stops a crash from replaying an unbounded nudge), the
// tick reports on its own line, and it returns true so the seat's notice
// survives to the next tick. Folding this arm into the generic failed: branch
// — returning false, or refunding the attempt — keeps the whole suite green
// while reopening either the crash-replay hole or the per-tick chatter.
func TestRunNudgeBackstopPromptedDuringDeliveryKeepsTheReservationAndTheNotice(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(t.Context(), backstopSession, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sp := &promptsDuringDeliveryProvider{Fake: fake}

	sessionBeads, store, pred := backstopHarness(t)
	now := time.Now()
	pred.last = now.Add(-2 * idleClaimNudgeGrace) // past the grace window: the engine wants to nudge

	// Seed the notice this lane would have recorded for an earlier prompted
	// skip. Surviving the tick is the assertion; an entry that was never there
	// cannot show that the arm declined to reclaim it.
	label := t.Name()
	backstopPromptNoticeIsNew(label, backstopSession, "r1")
	t.Cleanup(func() { backstopPromptNoticeForget(label, backstopSession) })

	var out bytes.Buffer
	runNudgeBackstop(sp, store, sessionBeads, now, &out, label, pred)

	if sp.probes < 2 {
		t.Fatalf("probes = %d, want >= 2: the pre-probe must have passed before the delivery re-probed", sp.probes)
	}
	if got := nudgeCalls(fake); got != 0 {
		t.Fatalf("typed into a seat that raised a prompt mid-delivery %d time(s): %#v", got, fake.SnapshotCalls())
	}
	// Reserved and kept. Refunding it would let a crash between the write and
	// the delivery replay the nudge without bound.
	if want := []int{1}; len(pred.reserved) != 1 || pred.reserved[0] != want[0] {
		t.Fatalf("reserved = %v, want %v: the write-ahead attempt is not given back", pred.reserved, want)
	}
	if pred.attempts != 1 {
		t.Fatalf("persisted attempts = %d, want 1", pred.attempts)
	}
	if !strings.Contains(out.String(), "prompted during delivery") {
		t.Fatalf("the mid-delivery refusal must report on its own line; out = %q", out.String())
	}
	if strings.Contains(out.String(), "failed:") {
		t.Fatalf("a prompted seat must not be reported as a delivery failure; out = %q", out.String())
	}
	// The notice survives only because the arm returns true. A false there
	// reclaims it and the lane re-announces "awaiting human input" next tick.
	if _, held := backstopPromptNoticeSeen.Load(backstopPromptNoticeKey(label, backstopSession)); !held {
		t.Fatal("the prompted-during-delivery tick reclaimed the seat's notice; the lane will re-announce on the next tick")
	}
}

// The push poller is where refused wait-idle nudges land: the mail and sling
// lanes queue what they could not deliver, so a guard that stops at the
// wait-idle paths only defers the mistype by one quiescence window.
func TestPollerSeatAwaitsHumanInput(t *testing.T) {
	for _, tt := range []struct {
		name        string
		newProvider func() runtime.Provider
		target      nudgeTarget
		want        bool
	}{
		{
			name:        "idle seat may receive queued nudges",
			newProvider: func() runtime.Provider { return runtime.NewFake() },
			target:      nudgeTarget{sessionName: backstopSession},
			want:        false,
		},
		{
			name: "seat at a prompt is refused",
			newProvider: func() runtime.Provider {
				f := runtime.NewFake()
				f.SetPendingInteraction(backstopSession, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval"})
				return f
			},
			target: nudgeTarget{sessionName: backstopSession},
			want:   true,
		},
		{
			name:        "probe failure fails closed",
			newProvider: func() runtime.Provider { return runtime.NewFailFake() },
			target:      nudgeTarget{sessionName: backstopSession},
			want:        true,
		},
		{
			name:        "no session name is not a refusal",
			newProvider: func() runtime.Provider { return runtime.NewFake() },
			target:      nudgeTarget{},
			want:        false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := pollerSeatAwaitsHumanInput(tt.target, tt.newProvider()); got != tt.want {
				t.Fatalf("pollerSeatAwaitsHumanInput = %v, want %v", got, tt.want)
			}
		})
	}
}

// backstopPromptNoticeSeen is process-global and only the delivery path used to
// clear it, so a seat that was prompted on one tick and filtered out earlier on
// the next left its entry behind for the life of the controller. Session names
// churn per epoch, so that grows without bound — and the stale entry also
// suppresses the notice if the seat ever comes back still prompted, which is
// what this asserts: the third tick must announce the prompt again.
func TestRunNudgeBackstopReclaimsPromptNoticeOnPreProbeExits(t *testing.T) {
	hold := backstopResolutionHold
	for _, tt := range []struct {
		name string
		skip func(*stubBackstopPredicate)
	}{
		{
			name: "no longer governed",
			skip: func(p *stubBackstopPredicate) { p.notGoverned = true },
		},
		{
			name: "target resolves away",
			skip: func(p *stubBackstopPredicate) { p.resolutionOverride = &hold },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := runtime.NewFake()
			if err := sp.Start(t.Context(), backstopSession, runtime.Config{}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			sp.SetPendingInteraction(backstopSession, &runtime.PendingInteraction{RequestID: "r1", Kind: "approval", Prompt: "approve?"})

			sessionBeads, store, pred := backstopHarness(t)
			now := time.Now()
			pred.last = now.Add(-2 * idleClaimNudgeGrace)
			label := t.Name()

			var out bytes.Buffer
			runNudgeBackstop(sp, store, sessionBeads, now, &out, label, pred)
			if got := strings.Count(out.String(), "awaiting human input"); got != 1 {
				t.Fatalf("first tick notices = %d, want 1; out = %q", got, out.String())
			}

			// The seat is filtered out before any probe runs. Nothing about the
			// prompt is observed on this tick, so nothing about it may be
			// remembered either.
			tt.skip(pred)
			now = now.Add(idleClaimNudgeBackoff)
			runNudgeBackstop(sp, store, sessionBeads, now, &out, label, pred)

			pred.notGoverned = false
			pred.resolutionOverride = nil
			now = now.Add(idleClaimNudgeBackoff)
			runNudgeBackstop(sp, store, sessionBeads, now, &out, label, pred)

			if got := strings.Count(out.String(), "awaiting human input"); got != 2 {
				t.Fatalf("notices = %d, want 2: the entry survived a tick that never observed the prompt; out = %q", got, out.String())
			}
			if _, leaked := backstopPromptNoticeSeen.Load(backstopPromptNoticeKey(label, backstopSession)); !leaked {
				t.Fatal("precondition: the final prompted tick should have recorded an entry")
			}
			t.Cleanup(func() { backstopPromptNoticeForget(label, backstopSession) })
		})
	}
}
