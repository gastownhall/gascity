package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestV2SessionResetVisibleWithoutRescan (ga-45119r AC-2, #5843): `gc session
// reset` writes its markers through the CLI's one-shot store, straight to
// the backing around the controller's primed session cache, and the keyed
// poke it used to send only enqueues a wake — so the woken pass decided on
// the stale cached row and the reset sat invisible for minutes. The
// session-sync barrier closes the window: it installs the row into that
// cache and enqueues the session's wake before replying, so the woken pass
// decides the A13 restart stop with the reset's ledger event still held (no
// re-scan, no event delivery), the stop's handoff consumes the request, and
// the wanted session wakes fresh exactly once — nothing stops the
// replacement.
func TestV2SessionResetVisibleWithoutRescan(t *testing.T) {
	// The row is mid-work, its work recorded on the row: the current-bead
	// heal arm (SESS-613's backstop) would otherwise claim the first pass
	// and delay the restart stop to a later one.
	s := scripted(t, []string{"gc-1"}, poolRow("gc-1", "worker", 1, "active",
		"instance_token", "tok-1", "session_key", "k-1", "last_woke_at", rowAt(-2*time.Minute)), gc1Work())
	// Settle the row first, as the reporter's long-lived session already
	// was: a pass parks at most one effect per row, so heal the fixture's
	// identity and work-stamp gaps, pass by pass, until a pass parks
	// nothing — the pass below is then free to decide on the reset alone.
	s.advance(simPatrol)
	for i := 0; i < 8; i++ {
		s.inventory()
		s.pass()
		if len(s.parked) == 0 {
			break
		}
		for len(s.parked) > 0 {
			s.release(0)
		}
		s.audit("v2")
	}

	// The CLI's reset write, as an outside writer: both markers straight to
	// the backing, its ledger event queued but never delivered — the feed
	// stays off, exactly the #5843 window.
	row, err := s.legs[0].backing.Get("gc-1")
	if err != nil {
		t.Fatalf("loading gc-1: %v", err)
	}
	s.outside("gc session reset", s.legs[0], row, func(l *simLeg, b beads.Bead) {
		s.setMeta(l, b.ID, "restart_requested", "true", "continuation_reset_pending", "true")
	})
	if len(s.legs[0].events) == 0 {
		t.Fatalf("the reset queued no ledger event; the fixture does not hold the feed")
	}
	if cached, err := s.legs[0].cache.Get("gc-1"); err != nil || cached.Metadata["restart_requested"] == "true" {
		t.Fatalf("the controller cache already sees the reset (err %v); the fixture does not reproduce the stale row", err)
	}

	// The barrier, over the controller's own socket against this city's
	// registered runtime: session-sync installs the row and enqueues the
	// session's wake before replying.
	pokeCh := make(chan struct{}, 1)
	wake := newLegacyWake(pokeCh, nil)
	registerResidencyRoutes(s.env.CityPath, nil, func() beads.Store { return s.legs[0].cache })
	defer unregisterResidencyRoutes(s.env.CityPath, nil)
	reply := askSessionSync(t, s.env.CityPath, wake, "gc-1")
	if reply.Outcome != "ok" || reply.Generation != "1" {
		t.Fatalf("session-sync reply = %+v, want ok at generation 1", reply)
	}
	select {
	case <-pokeCh:
	default:
		t.Fatalf("session-sync enqueued no wake for gc-1")
	}

	// The woken pass decides the A13 restart stop from the cache the
	// refresh installed into — no re-scan, no delivered event.
	s.inventory()
	s.pass()
	if len(s.parked) != 1 || s.parked[0].it.Kind != intentStop || s.parked[0].it.Reason != decideRestartRequested {
		got := make([]string, 0, len(s.parked))
		for _, e := range s.parked {
			got = append(got, fmt.Sprintf("%s/%s on %s", e.it.Kind, e.it.Reason, e.it.Key.ID))
		}
		t.Fatalf("parked %d effects (%v), want gc-1's restart stop alone", len(s.parked), got)
	}
	if s.parked[0].it.Key.ID != "gc-1" {
		t.Fatalf("parked effect on %s, want gc-1", s.parked[0].it.Key.ID)
	}
	s.release(0)
	s.audit("v2")
	if kills := s.killsOf("s-gc-1"); len(kills) != 1 {
		t.Fatalf("kills of s-gc-1 = %d, want the one stop", len(kills))
	}

	// The handoff consumed the request and marked the row asleep, its
	// continuation reset committed for the fresh conversation.
	m := s.gc1()
	if m["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q after the stop, want it consumed by the handoff", m["restart_requested"])
	}
	if m["state"] != string(session.StateAsleep) || m["session_key"] != "" {
		t.Fatalf("row after the handoff: state %q session_key %q, want asleep with the key cleared", m["state"], m["session_key"])
	}

	// The wanted session wakes fresh exactly once, and a later pass leaves
	// the replacement running: no second wave.
	s.advance(simPatrol)
	s.settle()
	s.advance(simPatrol)
	s.settle()
	if kills := s.killsOf("s-gc-1"); len(kills) != 1 {
		t.Fatalf("kills of s-gc-1 = %d after the fresh wake, want still the one stop", len(kills))
	}
	if starts := s.startsOf("s-gc-1"); len(starts) != 1 {
		t.Fatalf("starts of s-gc-1 = %d, want the one fresh wake", len(starts))
	}
	m = s.gc1()
	if m["state"] != "awake" || m["generation"] != "2" || m["restart_requested"] != "" {
		t.Fatalf("row after the fresh wake: %v, want awake at generation 2 with no restart request left", m)
	}
	s.noViolations(t)
}

// killsOf is the provider's kills of the named runtime.
func (s *sim) killsOf(name string) []simKill {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	var out []simKill
	for _, k := range s.sp.kills {
		if k.Name == name {
			out = append(out, k)
		}
	}
	return out
}

// startsOf is the provider's starts of the named runtime.
func (s *sim) startsOf(name string) []simStart {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	var out []simStart
	for _, st := range s.sp.starts {
		if st.Name == name {
			out = append(out, st)
		}
	}
	return out
}
