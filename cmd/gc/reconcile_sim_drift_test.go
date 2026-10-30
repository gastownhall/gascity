package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The simulator's config drifts (mc-i7efm, A14): an operator edits the
// worker template and the controller reloads it, so the next gather reads a
// new environment generation whose template hashes differ from what the
// rows started on. Each drift is one of A14's kinds: a box-affecting change
// (a pre_start command: the drain, or a named row's restart in place), a launch-only
// one (the start command: a relaunch where the provider can), a live one
// (session_live: its RunLive), or the seed's config restored (the drift
// resolved). An effect already running keeps the environment its pass
// gathered, as a reload does.
var driftOps = []struct {
	name string
	fn   func(s *sim, l *simLeg, b beads.Bead)
}{
	{"config-drift", func(s *sim, _ *simLeg, _ beads.Bead) {
		kind := []string{"box", "launch", "live", "revert"}[s.rng.IntN(4)]
		s.reloadConfig(kind, s.rng.IntN(3))
		s.logf("config-drift: %s", kind)
	}},
}

// reloadConfig makes the next gather read a new generation of the city's
// config: the worker template drifted by kind (to variant n), or the seed's
// config restored.
func (s *sim) reloadConfig(kind string, n int) {
	if s.baseCfg == nil {
		s.baseCfg = cloneSimCity(s.cfg)
	}
	next := cloneSimCity(s.cfg)
	if kind == "revert" {
		next = cloneSimCity(s.baseCfg)
	}
	for i := range next.Agents {
		a := &next.Agents[i]
		if a.Name != "worker" {
			continue
		}
		switch kind {
		case "box": // env vars mostly sit outside the fingerprint's allow list
			a.PreStart = []string{fmt.Sprintf("true --sim-box=%d", n)}
		case "launch":
			a.StartCommand = fmt.Sprintf("true --sim-drift=%d", n)
		case "live":
			a.SessionLive = []string{fmt.Sprintf("true --sim-live=%d", n)}
		}
	}
	s.cfg = next
	s.envNow = &reconcileEnv{Gen: s.envNow.Gen + 1, Cfg: next, SP: s.envNow.SP}
}

// cloneSimCity is c with its own agents and named sessions, the parts a
// drift edits or the seed appends to.
func cloneSimCity(c *config.City) *config.City {
	out := *c
	out.Agents = slices.Clone(c.Agents)
	out.NamedSessions = slices.Clone(c.NamedSessions)
	return &out
}

// Kills I10's v5.10m exemptions widened past their cases: A14's attached
// re-stamp excuses only a landed decideDriftAttached heal moving the
// stamp's two keys on a row still attached under the same drift, at most
// once per refresh interval. (The attached start's pacing is
// TestSimPendingPacedRetries'.)
func TestI10V510mExemptions(t *testing.T) {
	at := func(d time.Duration) string { return plannerT0.Add(d).UTC().Format(time.RFC3339) }
	stamped := func(when time.Duration, key string, kv ...string) beads.Bead {
		return poolRow("gc-1", "worker", 1, "awake", append([]string{
			"instance_token", "tok-1",
			sessionAttachedConfigDriftDeferredAtMetadata, at(when), sessionAttachedConfigDriftDeferredKeyMetadata, key,
		}, kv...)...)
	}
	heal := simResult{it: intent{Kind: intentRowHeal, Reason: decideDriftAttached, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}, res: settlement{Outcome: settledLanded}}
	for _, c := range []struct {
		name    string
		after   beads.Bead
		round   []simResult
		detach  bool
		excused bool
	}{
		{"a re-stamp after the interval", stamped(sessionAttachedConfigDriftRefreshInterval, "k"), []simResult{heal}, false, true},
		{"a re-stamp inside the interval", stamped(time.Minute, "k"), []simResult{heal}, false, false},
		{"another drift's key", stamped(sessionAttachedConfigDriftRefreshInterval, "k2"), []simResult{heal}, false, false},
		{"another key moved too", stamped(sessionAttachedConfigDriftRefreshInterval, "k", "sleep_reason", "x"), []simResult{heal}, false, false},
		{"a detached row", stamped(sessionAttachedConfigDriftRefreshInterval, "k"), []simResult{heal}, true, false},
		{"another effect in the round", stamped(sessionAttachedConfigDriftRefreshInterval, "k"), []simResult{heal, {it: intent{Kind: intentStart}, res: settlement{Outcome: settledLanded}}}, false, false},
		{"a landed re-stamp that moved nothing", stamped(0, "k"), []simResult{heal}, false, false},
		{"a refused re-stamp", stamped(sessionAttachedConfigDriftRefreshInterval, "k"), []simResult{{it: heal.it, res: settlement{Outcome: settledRefused, Cause: causePremise}}}, false, false},
		{"a re-stamp of another row than the heal's", stamped(sessionAttachedConfigDriftRefreshInterval, "k"), []simResult{{it: intent{Kind: intentRowHeal, Reason: decideDriftAttached, Key: rowKey{Leg: rowLeg, ID: "gc-2"}}, res: settlement{Outcome: settledLanded}}}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := stamped(0, "k")
			s := scripted(t, []string{"gc-1"}, before)
			if !c.detach {
				s.sp.rts[before.Metadata["session_name"]].attached = true
			}
			s.round = c.round
			id := simCityLeg + "/gc-1"
			if got := s.attachedDriftRestamps(map[string]beads.Bead{id: before}, map[string]beads.Bead{id: c.after}); got != c.excused {
				t.Fatalf("attachedDriftRestamps = %t, want %t", got, c.excused)
			}
		})
	}
}
