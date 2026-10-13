package main

import (
	"fmt"
	"slices"

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
