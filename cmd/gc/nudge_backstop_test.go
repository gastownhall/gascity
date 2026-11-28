package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// immediateTestBackstop is noticeTestBackstop resolving its target immediate
// (backstopResolutionImmediate).
type immediateTestBackstop struct {
	noticeTestBackstop
}

func (p immediateTestBackstop) resolve(beads.Bead, map[string]beads.Bead, string) (backstopTarget, backstopResolution) {
	if p.done {
		return backstopTarget{}, backstopResolutionClear
	}
	return p.target, backstopResolutionImmediate
}

// unobservedImmediateBackstop is immediateTestBackstop whose observe marker
// never lands: every read of its pacing finds a target it has not seen.
type unobservedImmediateBackstop struct {
	immediateTestBackstop
}

func (unobservedImmediateBackstop) state(beads.Bead, backstopTarget) (bool, int, time.Time) {
	return false, 0, time.Time{}
}

// An immediate verdict skips only the observe-first grace: with no attempt
// spent it nudges at once, and every later step is decideBackstopAction's.
// An outstanding verdict is decideBackstopAction's throughout.
func TestDecideBackstopActionFor(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		attempts int
		last     time.Time
		immed    backstopAction
	}{
		{name: "first sighting inside the grace", attempts: 0, last: now, immed: backstopActionNudge},
		{name: "past the grace", attempts: 0, last: now.Add(-idleClaimNudgeGrace), immed: backstopActionNudge},
		{name: "attempt 1 inside its backoff", attempts: 1, last: now.Add(-time.Minute), immed: backstopActionWait},
		{name: "attempt 1 past its backoff", attempts: 1, last: now.Add(-idleClaimNudgeBackoff), immed: backstopActionNudge},
		{name: "last attempt inside its backoff", attempts: idleClaimNudgeMaxAttempts, last: now.Add(-time.Minute), immed: backstopActionWait},
		{name: "last attempt past its backoff", attempts: idleClaimNudgeMaxAttempts, last: now.Add(-idleClaimNudgeBackoff), immed: backstopActionExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideBackstopActionFor(backstopResolutionImmediate, tc.attempts, tc.last, now); got != tc.immed {
				t.Errorf("immediate = %v, want %v", got, tc.immed)
			}
			want := decideBackstopAction(tc.attempts, tc.last, now)
			if got := decideBackstopActionFor(backstopResolutionOutstanding, tc.attempts, tc.last, now); got != want {
				t.Errorf("outstanding = %v, want decideBackstopAction's %v", got, want)
			}
		})
	}
}

// An immediate verdict whose observe marker did not land spends nothing on
// that tick: it waits for a tick that reads its own marker back.
func TestRunNudgeBackstopImmediateWaitsWhenTheObserveWriteFails(t *testing.T) {
	f := newLaneFixture(t)
	pred := unobservedImmediateBackstop{immediateTestBackstop{noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}}}
	var out bytes.Buffer

	runNudgeBackstop(f.runtime(), f.store, []beads.Bead{f.row(t)}, []beads.Bead{{ID: "wb-1", Status: "open"}}, f.clk.Now(), &out, "idle-claim-nudge", pred)

	if n := len(f.nudges()); n != 0 {
		t.Fatalf("nudges = %d, want 0 while the observe marker has not landed; journal %q", n, out.String())
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "" {
		t.Fatalf("reserved attempts = %q, want none", got)
	}
}

// Under a lane host that executes messages, the immediate attempt is the
// backstop's notice attempt 1, proposed on the tick that first observes the
// target, not typed directly.
func TestRunNudgeBackstopProposesAnImmediateAttemptAsNoticeOne(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	pred := immediateTestBackstop{noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}}
	var out bytes.Buffer

	observed := f.clk.Now() // inside the grace
	runNudgeBackstop(f.runtime(), f.store, backstopRows(t, f, 0, observed), []beads.Bead{{ID: "wb-1", Status: "open"}}, f.clk.Now(), &out, "idle-claim-nudge", pred)

	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
	op := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(observed), 1))
	if op.Payload != "claim wb-1" {
		t.Fatalf("op = %+v, want the claim notice", op)
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want 1", got)
	}
}
