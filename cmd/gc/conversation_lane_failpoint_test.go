package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// laneCrash is what a test's failpoint kill panics with, so the pass ends
// at the failpoint exactly as a SIGKILL would end it: the lane runs no
// deferred write and recovers nothing.
type laneCrash struct{}

// laneFailpointHarness replaces the process's failpoint state and seams for
// one test: the variable reads raw (unset when raw is ""), the stamp reads
// hooks, notices go to notice, and a kill panics laneCrash{} after counting.
type laneFailpointHarness struct {
	notice *bytes.Buffer
	kills  int
	// crash panics laneCrash{} on a kill; false only counts it.
	crash bool
}

func withLaneFailpoint(t *testing.T, raw string, hooks bool) *laneFailpointHarness {
	t.Helper()
	h := &laneFailpointHarness{notice: &bytes.Buffer{}, crash: true}
	oldState, oldLookup, oldHooks, oldNotice, oldKill := laneFailpointState, laneFailpointLookupEnv, laneFailpointHooksEnabled, laneFailpointNotice, laneFailpointKill
	t.Cleanup(func() {
		laneFailpointState, laneFailpointLookupEnv, laneFailpointHooksEnabled, laneFailpointNotice, laneFailpointKill = oldState, oldLookup, oldHooks, oldNotice, oldKill
	})
	laneFailpointState = &laneFailpoints{}
	laneFailpointLookupEnv = func(name string) (string, bool) {
		if name != laneFailpointEnv || raw == "" {
			return "", false
		}
		return raw, true
	}
	laneFailpointHooksEnabled = func() bool { return hooks }
	laneFailpointNotice = h.notice
	laneFailpointKill = func() {
		h.kills++
		if h.crash {
			panic(laneCrash{})
		}
	}
	return h
}

// crashed runs fn and reports whether it ended at a failpoint kill.
func crashed(fn func()) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(laneCrash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

func TestParseLaneFailpoint(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want laneFailpoint
		err  string
	}{
		{raw: "ctl:take", want: laneFailpoint{host: "ctl", stage: laneFailTake, n: 1}},
		{raw: "ctl:effect", want: laneFailpoint{host: "ctl", stage: laneFailEffect, n: 1}},
		{raw: "ctl:done:5", want: laneFailpoint{host: "ctl", stage: laneFailDone, n: 5}},
		{raw: "cli:propose", want: laneFailpoint{host: "cli", stage: laneFailPropose, n: 1}},
		{raw: "cli:take:1", want: laneFailpoint{host: "cli", stage: laneFailTake, n: 1}},
		{raw: "cli:effect:12", want: laneFailpoint{host: "cli", stage: laneFailEffect, n: 12}},
		{raw: "ctl:propose", err: "stage propose is the cli host's only"},
		{raw: "hook:take", err: `unknown host "hook"`},
		{raw: "ctl:read", err: `unknown stage "read"`},
		{raw: "ctl:take:0", err: `hit count "0"`},
		{raw: "ctl:take:-1", err: `hit count "-1"`},
		{raw: "ctl:take:+2", err: `hit count "+2"`},
		{raw: "ctl:take:x", err: `hit count "x"`},
		{raw: " ctl:take", err: `unknown host " ctl"`},
		{raw: "ctl:take ", err: `unknown stage "take "`},
		{raw: "ctl", err: "not <host>:<stage>[:<n>]"},
		{raw: "ctl:take:1:2", err: "not <host>:<stage>[:<n>]"},
		{raw: "", err: "not <host>:<stage>[:<n>]"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseLaneFailpoint(tc.raw)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("parse(%q) = %+v, %v; want an error containing %q", tc.raw, got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parse(%q) = %+v, %v; want %+v", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestLaneFailpointGating(t *testing.T) {
	t.Run("without the stamp it is ignored, once, and never kills", func(t *testing.T) {
		h := withLaneFailpoint(t, "ctl:done", false)
		for range 3 {
			laneFailpointHit("ctl", laneFailDone)
		}
		if h.kills != 0 {
			t.Fatalf("%d kills without the stamp", h.kills)
		}
		want := `gc: WARNING: GC_LANE_FAILPOINT="ctl:done": ignored: this gc binary is not built with test hooks (test-only knob)` + "\n"
		if got := h.notice.String(); got != want {
			t.Fatalf("notice %q, want %q", got, want)
		}
	})
	t.Run("unset says nothing and never kills", func(t *testing.T) {
		h := withLaneFailpoint(t, "", true)
		laneFailpointHit("ctl", laneFailDone)
		if h.kills != 0 || h.notice.Len() != 0 {
			t.Fatalf("kills %d, notice %q", h.kills, h.notice.String())
		}
	})
	t.Run("a malformed value is ignored with why", func(t *testing.T) {
		h := withLaneFailpoint(t, "ctl:propose", true)
		laneFailpointHit("ctl", laneFailPropose)
		if h.kills != 0 || !strings.Contains(h.notice.String(), "ignored: stage propose is the cli host's only") {
			t.Fatalf("kills %d, notice %q", h.kills, h.notice.String())
		}
	})
	t.Run("armed: announced once, fires on the matching host, stage and hit only, once", func(t *testing.T) {
		h := withLaneFailpoint(t, "ctl:done:3", true)
		h.crash = false
		laneFailpointHit("cli", laneFailDone) // another host
		laneFailpointHit("ctl", laneFailTake) // another stage
		laneFailpointHit("ctl", laneFailDone) // hit 1
		laneFailpointHit("ctl", laneFailDone) // hit 2
		if h.kills != 0 {
			t.Fatalf("fired before the third hit (%d kills)", h.kills)
		}
		laneFailpointHit("ctl", laneFailDone) // hit 3
		laneFailpointHit("ctl", laneFailDone) // hit 4
		if h.kills != 1 {
			t.Fatalf("%d kills, want exactly one at the third hit", h.kills)
		}
		want := `gc: WARNING: GC_LANE_FAILPOINT="ctl:done:3": TEST HOOK ACTIVE: this process kills itself (SIGKILL) at done (hit 3) of its ctl lane` + "\n"
		if got := h.notice.String(); got != want {
			t.Fatalf("notice %q, want %q", got, want)
		}
	})
	t.Run("arming a lane of another host or a done stage hooks nothing", func(t *testing.T) {
		withLaneFailpoint(t, "cli:take", true)
		f := newLaneFixture(t)
		st := f.laneStore(t)
		l := f.lane(t, st)
		armLaneFailpoints(l, laneHostName)
		f.propose(t, st, f.msg("k", opjournal.RolePrompt, "go"))
		if crashed(func() { _, _ = f.pass(context.Background(), t, l, st, nil) }) {
			t.Fatal("a cli failpoint fired in a ctl lane")
		}
	})
}

// TestLaneFailpointStages runs a real pass over the live executor and checks
// the store and the runtime at each stage's kill.
func TestLaneFailpointStages(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		status    opjournal.Status
		delivered int
	}{
		{raw: "ctl:take", status: opjournal.StatusExecuting, delivered: 0},
		{raw: "ctl:effect", status: opjournal.StatusExecuting, delivered: 1},
		{raw: "ctl:done", status: opjournal.StatusClosed, delivered: 1},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			withLaneFailpoint(t, tc.raw, true)
			f := newLaneFixture(t)
			st := f.laneStore(t)
			l := f.lane(t, st)
			armLaneFailpoints(l, laneHostName)
			op := f.propose(t, st, f.msg("k", opjournal.RolePrompt, "go"))
			ok := crashed(func() {
				res, _ := f.pass(context.Background(), t, l, st, nil)
				laneFailpointAfterPass(laneHostName, res)
			})
			if !ok {
				t.Fatal("the failpoint did not fire")
			}
			if got := f.get(t, st, op.ID); got.Status != tc.status {
				t.Fatalf("op %s at the kill, want %s", got.Status, tc.status)
			}
			if n := len(f.nudges()); n != tc.delivered {
				t.Fatalf("%d deliveries at the kill, want %d", n, tc.delivered)
			}
		})
	}
	t.Run("cli:propose", func(t *testing.T) {
		withLaneFailpoint(t, "cli:propose", true)
		c := newCLILaneCity(t)
		if !crashed(func() { c.nudge(nudgeDeliveryWaitIdle, "k-propose-1", false, cliLaneTestMessage) }) {
			t.Fatal("the propose failpoint did not fire")
		}
		if op := c.only(t); op.Status != opjournal.StatusSubmitted || op.ClientKey != "k-propose-1" {
			t.Fatalf("op %+v, want it durable and untaken", op)
		}
		if len(c.nudges()) != 0 || len(c.rings) != 0 {
			t.Fatalf("%d deliveries and rings %v at the propose kill", len(c.nudges()), c.rings)
		}
	})
	t.Run("done fires on an accepted Done only", func(t *testing.T) {
		h := withLaneFailpoint(t, "ctl:done", true)
		h.crash = false
		for _, arm := range []opjournal.DoneArm{"", opjournal.DoneReleased, opjournal.DoneRejected, opjournal.DoneSuperseded} {
			laneFailpointAfterPass(laneHostName, opjournal.PassResult{Done: arm})
		}
		if h.kills != 0 {
			t.Fatalf("%d kills on Dones that were not accepted", h.kills)
		}
		laneFailpointAfterPass(laneHostName, opjournal.PassResult{Done: opjournal.DoneAccepted})
		if h.kills != 1 {
			t.Fatalf("%d kills on an accepted Done", h.kills)
		}
	})
}

// The controller's lane host consults the failpoints: take and effect
// through the lane its store opens, done after each pass.
func TestLaneHostRunsItsFailpoints(t *testing.T) {
	for _, raw := range []string{"ctl:take", "ctl:effect", "ctl:done"} {
		t.Run(raw, func(t *testing.T) {
			h := withLaneFailpoint(t, raw, true)
			h.crash = false
			f := newLaneFixture(t)
			hh := newLaneHostHarness(t, f)
			hh.start()
			st := f.laneStore(t)
			f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
			hh.h.Poke(f.id)
			if p := hh.nextPass(t); p.out.err != nil || p.out.res.Done != opjournal.DoneAccepted {
				t.Fatalf("pass = %+v, want accepted", p.out)
			}
			if h.kills != 1 {
				t.Fatalf("%d kills at %s, want one", h.kills, raw)
			}
		})
	}
}
