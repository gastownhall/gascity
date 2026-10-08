package main

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The simulator's invariant checks (D1a; CONTRACT v5 §10) beyond I8 and I10,
// against ground truth: I1, I7, I15 and I23, which merged code can break, each
// with a seeded mutant; and I2, I4, I5, I9, I14, I16, I17 and I24, whose
// writers have not merged, each shown to report by TestSimChecksBite until its
// PR adds a mutant.

func init() {
	simStepChecks = append(simStepChecks, (*sim).checkCaps, (*sim).checkStarts, (*sim).checkDestructive, (*sim).checkTokens, (*sim).checkDead)
	simWriteChecks = append(simWriteChecks, (*sim).checkWrite)
	simEffectChecks = append(simEffectChecks, (*sim).checkFinalized)
	simQuietChecks = append(simQuietChecks, (*sim).checkDwell)
	simMutants = append(simMutants, []struct {
		name, inv string
		opts      simOpts
	}{
		{"admit ignores the effects in flight", "I1 ", simOpts{inflight: func(m *inflightMap) plannerInflight { return hidingInflight{m} }}},
		{"a heal writes a state legacy does not know", "I7 ", simOpts{arms: mutateHeal(func(r *rowFacts) (intent, bool) {
			it, ok := armTimerHeals(r)
			if it.Kind != "" {
				it.Patch["state"] = "healing"
			}
			return it, ok
		})}},
		{"a heal misreads its timer and clears an operator hold", "I15 ", simOpts{arms: mutateHeal(func(r *rowFacts) (intent, bool) {
			patch, _ := timerHealPatch(r.row.Info, r.w.Now.Add(time.Hour))
			if len(patch) == 0 {
				return intent{}, false
			}
			return intent{Kind: intentRowHeal, Reason: decideTimerHeal, Basis: rowBasis{Incarnation: r.row.Incarnation, InstanceToken: r.row.InstanceToken}, Patch: patch}, true
		})}},
	}...)
}

// hidingInflight is an admission that ignores the effects in flight: the pass
// sees an empty map.
type hidingInflight struct{ *inflightMap }

func (hidingInflight) view() inflightView { return inflightView{} }

// mutateHeal replaces A6's timer heal with heal.
func mutateHeal(heal func(r *rowFacts) (intent, bool)) func([]rowArm) []rowArm {
	return func(arms []rowArm) []rowArm {
		for i := range arms {
			if arms[i].name == "A6" {
				arms[i].decide = heal
			}
		}
		return arms
	}
}

// checkCaps is I1 (I-cap, P4): the effects in flight never exceed their
// class's cap, counted on the planner's own map, whatever admission saw.
func (s *sim) checkCaps() {
	probe := s.cfg.Daemon.ProbeConcurrencyOrDefault()
	caps := map[capClass]int{capStarts: s.cfg.Daemon.MaxWakesPerTickOrDefault(), capCreates: createsInFlightCap, capProbing: probe, capRowWrites: probe}
	count := make(map[capClass]int)
	for _, e := range s.inflight.view().Entries {
		if spec, ok := intentKinds[e.Kind]; ok && !e.Ambiguous {
			count[spec.class]++
		}
	}
	for c, n := range count {
		if n > caps[c] {
			s.failf("I1 I-cap", "%d effects of cap class %d in flight, cap %d", n, c, caps[c])
		}
	}
}

// checkStarts is I2 (I-start: at most one runtime v2 creates per row and
// intent token) and I24 (I-fresh-start: every v2 Start is FreshOnly).
func (s *sim) checkStarts() {
	created := make(map[[2]string]int)
	for _, st := range s.sp.starts {
		if st.External {
			continue
		}
		if !st.FreshOnly {
			s.failf("I24 I-fresh-start", "v2 started %s without FreshOnly", st.Name)
		}
		if k := [2]string{st.ID, st.Token}; st.Created {
			if created[k]++; created[k] > 1 {
				s.failf("I2 I-start", "v2 created a second runtime for row %s token %s", st.ID, st.Token)
			}
		}
	}
}

// checkDestructive is I4 (I-fence) and I5 (I-attached) over v2's provider
// kills: each hits its row's own runtime (O2 Current, or the own-runtime
// exception's StaleSelf with epoch = generation) or the dead object the
// corpse and start-recycle exceptions name, and never an attached one. The
// own-runtime exception to I5 is D1b's to model with the effects it covers.
func (s *sim) checkDestructive() {
	for _, k := range s.sp.kills {
		if k.External || k.Hit == nil {
			continue
		}
		if k.Hit.attached {
			s.failf("I5 I-attached", "v2 %s on %s while attached", k.Method, k.Name)
		}
		row := s.rowNamed(k.Name)
		switch {
		case k.Method == "KillCorpseObject" && k.Hit.corpse, k.Method == "KillZombieObject" && k.Hit.zombie:
		case row.ID == "" || k.Hit.id != row.ID:
			s.failf("I4 I-fence", "v2 %s on %s hit row %q's runtime, not its own", k.Method, k.Name, k.Hit.id)
		case k.Hit.token != row.Metadata["instance_token"] && k.Hit.epoch != row.Metadata["generation"]:
			s.failf("I4 I-fence", "v2 %s on %s hit token %s, not row %s's", k.Method, k.Name, k.Hit.token, row.ID)
		}
	}
}

// rowNamed is the open row that carried name before this step.
func (s *sim) rowNamed(name string) beads.Bead {
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		if b := s.prev[k]; b.Status != "closed" && b.Metadata["session_name"] == name {
			return b
		}
	}
	return beads.Bead{}
}

// checkTokens is I9 (I-tokens): in any window, admitted starts ≤ capacity
// plus the refill over the window, with no refund.
func (s *sim) checkTokens() {
	capacity := s.cfg.Daemon.MaxWakesPerTickOrDefault()
	period := simPatrol / time.Duration(capacity)
	for i := range s.admitted {
		for j := i; j < len(s.admitted); j++ {
			if n := j - i + 1; n > capacity+int(s.admitted[j].Sub(s.admitted[i])/period) {
				s.failf("I9 I-tokens", "%d starts admitted within %s, capacity %d", n, s.admitted[j].Sub(s.admitted[i]), capacity)
			}
		}
	}
}

// stopKeys are the stop request's five keys (v5 D1, R1).
var stopKeys = []string{drainIntentReasonKey, drainIntentAtKey, drainIntentIncarnationKey, session.DrainAckIncarnationKey, session.DrainAckAtKey}

// checkWrite checks one row change v2 made:
//   - I7 (I-legacy): it writes no state outside knownSessionStates, but the
//     signaled projection (draining + stop-pending);
//   - I14 (I-STOP-1/2): a stop request names the row's current incarnation;
//   - I15 (I-STOP-3/4): no request survives a v2 PreWake, and no write
//     rewrites what an operator holds dormant (dormantChange);
//   - I4 (I-fence): no terminal write lands while the row's own runtime has a
//     live pane, which C8.8 would have read.
func (s *sim) checkWrite(w simWrite) {
	b, a := w.Before.Metadata, w.After.Metadata
	id := w.Leg + "/" + w.After.ID
	if st := a["state"]; st != b["state"] && !knownSessionStates[st] && (st != string(session.StateDraining) || a["state_reason"] != session.DrainAckStopPendingReason) {
		s.failf("I7 I-legacy", "v2 wrote state %q on %s", st, id)
	}
	if inc := a[drainIntentIncarnationKey]; inc != "" && inc != b[drainIntentIncarnationKey] && inc != a["generation"] {
		s.failf("I14 I-STOP-1/2", "v2 bound %s's stop request to incarnation %s at generation %s", id, inc, a["generation"])
	}
	if w.Before.ID == "" {
		return
	}
	if a["generation"] != b["generation"] {
		for _, k := range stopKeys {
			if a[k] != "" {
				s.failf("I15 I-STOP-3/4", "%s survived v2's PreWake of %s", k, id)
			}
		}
	}
	if k := dormantChange(w.Before, a, w.At); k != "" {
		s.failf("I15 I-STOP-3/4", "v2 rewrote %s %q -> %q on %s, which an operator holds dormant at %s", k, b[k], a[k], id, w.At.Format(time.RFC3339))
	}
	terminal := (w.After.Status == "closed" && w.Before.Status != "closed") ||
		(a["state"] != b["state"] && slices.Contains([]string{"asleep", "drained", "stopped", "closed", "orphaned", string(session.StateFailedCreate)}, a["state"]))
	if terminal && s.livePane(w.Before) {
		s.failf("I4 I-fence", "v2 wrote %s terminal (state %q) while its runtime has a live pane", id, a["state"])
	}
}

// dormantChange returns a key a v2 write changed on b that an operator owns
// at t, or "": an honored kill fence owns the row's lifecycle and its sleep
// reason, a suspend the row's state, and an unexpired hold or quarantine its
// timer and sleep reason; no dormant row is woken or given a stop request,
// though one it holds may be cleared.
func dormantChange(b beads.Bead, a map[string]string, t time.Time) string {
	m := b.Metadata
	until := func(k string) bool { at, err := time.Parse(time.RFC3339, m[k]); return err == nil && at.After(t) }
	reason := session.SleepReason(m["sleep_reason"])
	var owned []string
	if session.KillPendingMetadata(m["state"], m["state_reason"], m["sleep_reason"], m["slept_at"], t) {
		owned = append(owned, "state", "state_reason", "sleep_reason", "slept_at")
	}
	if m["state"] == string(session.StateSuspended) {
		owned = append(owned, "state", "suspended_at")
	}
	if until("held_until") {
		owned = append(owned, "held_until")
		if reason == session.SleepReasonUserHold {
			owned = append(owned, "sleep_reason")
		}
	}
	if until("quarantined_until") {
		owned = append(owned, "quarantined_until")
		if slices.Contains([]session.SleepReason{session.SleepReasonQuarantine, session.SleepReasonContextChurn, session.SleepReasonRateLimit}, reason) {
			owned = append(owned, "sleep_reason")
		}
	}
	if len(owned) == 0 {
		return ""
	}
	for _, k := range owned {
		if a[k] != m[k] {
			return k
		}
	}
	for _, k := range stopKeys { // the residue void clears them (v5 D1, R1 as amended at C6a)
		if a[k] != m[k] && a[k] != "" {
			return k
		}
	}
	if a["state"] != m["state"] && slices.Contains([]string{"awake", "active", "creating", string(session.StateStartPending)}, a["state"]) {
		return "state"
	}
	return ""
}

// livePane reports whether b's own runtime (its ID and token) has a live
// pane: what a C8.8 confirmation reads as not stopped.
func (s *sim) livePane(b beads.Bead) bool {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	rt := s.sp.rts[b.Metadata["session_name"]]
	return rt != nil && !rt.corpse && rt.id == b.ID && rt.token == b.Metadata["instance_token"]
}

func signaledRow(b beads.Bead) bool {
	return b.Status != "closed" && b.Metadata["state"] == string(session.StateDraining) && b.Metadata["state_reason"] == session.DrainAckStopPendingReason
}

// checkFinalized is I16 (I-STOP-5): a stop effect that ran while its row's
// runtime had no live pane leaves the row's request finalized.
func (s *sim) checkFinalized(e *simEffect) func(settlement) {
	l := s.legOf(e.it.Key.Leg)
	if e.it.Kind != intentStop || l == nil {
		return func(settlement) {}
	}
	before, _ := l.backing.Get(e.it.Key.ID)
	paneBefore := s.livePane(before)
	return func(res settlement) {
		b, err := l.backing.Get(e.it.Key.ID)
		if !paneBefore && res.Cause != "name-busy" && err == nil && signaledRow(b) && !s.livePane(b) {
			s.failf("I16 I-STOP-5", "the stop effect on %v confirmed no live pane and left the request signaled", e.it.Key)
		}
	}
}

// legOf is the leg whose cache the census reads as ref.
func (s *sim) legOf(ref string) *simLeg {
	legs, err := sessionCensusStoreCandidates(s.env.CityPath, s.cfg, s.legs[0].cache, s.env.RigStores(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, c := range legs {
		for _, l := range s.legs {
			if c.ref == ref && c.store == beads.Store(l.cache) {
				return l
			}
		}
	}
	return nil
}

// checkDwell is I17 (I-STOP-7), at quiescence: past the bound, no row is
// still signaled whose runtime would pass the fence (its own, live,
// unattached).
func (s *sim) checkDwell() {
	for _, k := range slices.Sorted(maps.Keys(s.prev)) {
		if b := s.prev[k]; signaledRow(b) && s.livePane(b) && !s.sp.IsAttached(b.Metadata["session_name"]) {
			s.failf("I17 I-STOP-7", "%s is still signaled past the bound with a stoppable runtime", k)
		}
	}
}

// observed is the census a pass would read now and C2d's classes for it.
func (s *sim) observed() (*sessionCensus, map[rowKey]rowObservation) {
	legs, err := sessionCensusStoreCandidates(s.env.CityPath, s.cfg, s.legs[0].cache, s.env.RigStores(), nil)
	if err != nil {
		s.t.Fatal(err)
	}
	c, err := readSessionCensus(s.clk.Now(), legs)
	if err != nil {
		return nil, nil
	}
	return c, observeCensus(s.lane.cache.Snapshot(), c, s.clk.Now(), 2*simPatrol)
}

// checkDead is I23 (I-dead): where the latest fresh inventory pass listed a
// row's runtime after its last change (by the provider's version), the census and a fresh read agree on
// a corpse or a zombie: the census reads it dead (or unknown, its identity
// unread), never alive, gone or occupied; and reads nothing else dead, an
// agent whose probe errored included.
func (s *sim) checkDead() {
	pass := s.lane.cache.Snapshot().Inventory
	if pass.FinishedAt.IsZero() || s.clk.Now().Sub(pass.FinishedAt) > 2*simPatrol {
		return
	}
	c, obs := s.observed()
	if c == nil {
		return
	}
	listed := pass.listedOn()
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	for _, row := range c.Canonical() {
		name := row.Info.SessionName
		rt := s.sp.rts[name]
		if _, ok := listed[name]; !ok || rt == nil || rt.id != row.Key.ID || s.sp.changed[name] > s.listed {
			continue
		}
		fresh := s.sp.livenessLocked(name)
		dead := fresh.Present() && !fresh.Alive
		switch got := obs[row.Key].Liveness; {
		case dead && !rt.probeErr && got != livenessDead && got != livenessUnknown: // an erroring probe proves nothing (O3)
			s.failf("I23 I-dead", "the census reads %s %s; a fresh read finds it dead (corpse %t)", name, got, fresh.Corpse)
		case !dead && got == livenessDead:
			s.failf("I23 I-dead", "the census reads %s dead; a fresh read finds it alive", name)
		}
	}
}

// KillCorpseObject and KillZombieObject are LL2's exact-object kills: each
// kills only the object a read observed, still in the shape it requires, as
// the if-shell re-check does (v5 F2).
func (p *simProvider) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	return p.killObject("KillCorpseObject", name, objectID, created, func(rt *simRuntime) bool { return rt.corpse })
}

func (p *simProvider) KillZombieObject(name, objectID, created, _ string) (runtime.SessionObjectKillResult, error) {
	return p.killObject("KillZombieObject", name, objectID, created, func(rt *simRuntime) bool { return !rt.corpse && rt.zombie })
}

func (p *simProvider) killObject(method, name, objectID, created string, shape func(*simRuntime) bool) (runtime.SessionObjectKillResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch rt := p.rts[name]; {
	case rt == nil || fmt.Sprintf("$%d", rt.object) != objectID || strconv.FormatInt(rt.created, 10) != created:
		return runtime.SessionObjectGone, nil
	case !shape(rt):
		return runtime.SessionObjectLive, nil
	}
	p.killLocked(method, name)
	return runtime.SessionObjectKilled, nil
}

// Kills a check that cannot fail: each invariant whose writers have not
// merged reports a violation fed straight to it.
func TestSimChecksBite(t *testing.T) {
	row := func(id string, kv ...string) beads.Bead {
		b := sessionRow(id, append([]string{"session_name", "s-" + id, "generation", "2", "instance_token", "tok-2"}, kv...)...)
		b.Status = "open"
		return b
	}
	signaled := []string{"state", string(session.StateDraining), "state_reason", session.DrainAckStopPendingReason}
	for _, c := range []struct {
		inv     string
		arrange func(s *sim)
	}{
		{"I2 ", func(s *sim) {
			for range 2 {
				s.sp.starts = append(s.sp.starts, simStart{Name: "s-x", ID: "x", Token: "tok-2", FreshOnly: true, Created: true})
			}
		}},
		{"I24 ", func(s *sim) { s.sp.starts = append(s.sp.starts, simStart{Name: "s-x", ID: "x", Token: "tok-2"}) }},
		{"I4 ", func(s *sim) { // a kill of another incarnation's runtime
			s.prev = map[string]beads.Bead{"city/x": row("x")}
			s.sp.kills = append(s.sp.kills, simKill{Method: "Stop", Name: "s-x", Hit: &simRuntime{id: "x", epoch: "1", token: "tok-1"}})
		}},
		{"I4 ", func(s *sim) { // a terminal write over a live pane
			s.sp.put("s-x", "x", "2", "tok-2")
			s.checkWrite(simWrite{Leg: "city", Before: row("x", "state", "active"), After: row("x", "state", "asleep")})
		}},
		{"I5 ", func(s *sim) {
			s.sp.kills = append(s.sp.kills, simKill{Method: "KillCorpseObject", Name: "s-x", Hit: &simRuntime{corpse: true, attached: true}})
		}},
		{"I9 ", func(s *sim) {
			for range s.cfg.Daemon.MaxWakesPerTickOrDefault() + 1 {
				s.admitted = append(s.admitted, s.clk.Now())
			}
		}},
		{"I14 ", func(s *sim) {
			s.checkWrite(simWrite{Leg: "city", Before: row("x"), After: row("x", drainIntentIncarnationKey, "1")})
		}},
		{"I15 ", func(s *sim) { // a request that survives a v2 PreWake
			s.checkWrite(simWrite{Leg: "city", Before: row("x", "generation", "1"), After: row("x", session.DrainAckIncarnationKey, "1")})
		}},
		{"I16 ", func(s *sim) {
			s.setMeta(s.legs[0], "gc-1", signaled...)
			s.sp.drop("s-gc-1")
			s.checkFinalized(&simEffect{it: intent{Kind: intentStop, Key: rowKey{Leg: rowLeg, ID: "gc-1"}}})(settlement{Outcome: settledLanded})
		}},
		{"I17 ", func(s *sim) {
			s.prev = map[string]beads.Bead{"city/x": row("x", signaled...)}
			s.sp.put("s-x", "x", "2", "tok-2")
			s.checkDwell()
		}},
		{"I23 ", func(s *sim) { // the census still reads alive what is now a corpse
			s.sp.rts["s-gc-1"].corpse = true // its version stays the one listed
		}},
	} {
		t.Run(c.inv, func(t *testing.T) {
			s := newSim(t, 3, simOpts{rows: func(s *sim) ([]beads.Bead, []beads.Bead) {
				s.sp.put("s-gc-1", "gc-1", "1", "tok-gc-1-1")
				return []beads.Bead{poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-gc-1-1")}, nil
			}})
			s.advance(time.Second)
			s.inventory()
			s.advance(time.Second)
			c.arrange(s)
			s.check()
			if !slices.ContainsFunc(s.failures, func(f string) bool { return strings.HasPrefix(f, c.inv) }) {
				t.Errorf("the check reported %q, want an %sviolation", s.failures, c.inv)
			}
		})
	}
}
