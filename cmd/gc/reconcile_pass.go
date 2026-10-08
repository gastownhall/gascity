package main

import (
	"cmp"
	"fmt"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The planner's pass (architecture §1.3; CONTRACT v5 P1-P5): gather the
// World, allocate, decide each row, add the allocation's creates, admit, and
// publish. In this slice it is trace-only: it records what admission let
// through and submits nothing. Unwired: C2c2 runs it behind
// session_reconciler=v2, and C9 adds the submit.

// passOutputs are the values a pass publishes for other goroutines. Each is
// replaced after every pass and never mutated once published.
type passOutputs struct {
	record   atomic.Pointer[passTrace]
	relevant atomic.Pointer[relevantSet] // beadEventRelevant's recent set
	summary  atomic.Pointer[allocSummary]
}

// passTrace is one pass's record (R6): admission's intents, the deferred with
// their causes, the rows whose (reason, outcome) changed, and the allocation's
// operator alerts. Err ended the pass early.
type passTrace struct {
	At                 time.Time
	Err                string
	Admitted, Deferred []intent
	Rows               []rowTrace
	Alerts             []string
}

// rowTrace is one row's decision when it changed: decideRow's reason and
// admission's outcome for its intent, with the row's template and session
// name, which the trace sink files it under.
type rowTrace struct {
	Key                   rowKey
	Template, SessionName string
	Reason, Outcome       string
}

// Row trace outcomes; a deferred intent's outcome is its cause after
// outcomeDeferred.
const (
	outcomeNone     = "none" // a hold, None or no action
	outcomeAdmitted = "admitted"
	outcomeDeferred = "deferred/"
	decidePanic     = "panic"
)

// allocSummary is what C8's lane steps read of the last allocation (S-14):
// the assigned work with its stores, refs and readiness (the continuation
// candidates' inputs), the ready routed work, and the census's open rows. It
// shares the pass's demand rows, which each pass gathers afresh and no one
// edits. Partial is legacy's snapshotQueryPartial: the assigned-work read or
// a census leg failed.
type allocSummary struct {
	At                time.Time
	Partial           bool
	OpenSessions      []session.Info
	AssignedWork      []beads.Bead
	AssignedStores    []beads.Store
	AssignedStoreRefs []string
	ReadyAssigned     map[storeScopedBeadKey]bool
	ReadyRouted       []beads.Bead
	ReadyRoutedRefs   []string
}

// tracePass runs one trace-only pass over e at now. C2c2 hands the planner
// func(now time.Time) passResult { return p.tracePass(e, now) }.
func (p *planner) tracePass(e gatherEnv, now time.Time) passResult {
	rec := &passTrace{At: now}
	defer p.out.record.Store(rec)
	w, err := gather(e, p, now)
	if err != nil {
		rec.Err = err.Error()
		return passResult{}
	}
	p.clearAmbiguous(&w, now)
	cfg := w.Env.Cfg
	a, err := decideAllocation(allocInputs{
		Now: now, Cfg: cfg, ConfigRev: w.Env.ConfigRev, EnvGen: w.Env.Gen, CityPath: e.CityPath, CityName: e.CityName,
		CitySuspended: w.CitySuspended, SuspendedRigPaths: w.SuspendedRigPaths, Census: w.Census, Demand: w.Demand,
		ScaleCheck: w.ScaleCheck, Obs: w.Obs, ObsMaxAge: w.ObsMaxAge, Endpoints: w.Gates, ProviderHealth: w.ProviderHealth,
		Episodes: w.Episodes, SleepPolicies: w.SleepPolicies, TransportRefused: w.TransportRefused, ReadyWaits: w.ReadyWaits,
		InFlight: w.InFlight, Backoff: w.Backoff,
	})
	if err != nil {
		rec.Err = err.Error()
		return passResult{}
	}
	rec.Alerts = a.Alerts

	running := make(map[rowKey]bool)
	for _, f := range w.InFlight.Entries {
		if f.Kind != inflightCreate {
			running[f.Key] = true
		}
	}
	var intents []intent
	var next time.Time
	reasons := make(map[rowKey]string)
	for _, row := range w.Census.Canonical() {
		if running[row.Key] {
			continue // the only enforcement of one intent per row (R5)
		}
		it, due := p.decideRowSafe(&w, &a, row.Key)
		next = earliest(next, due)
		reasons[row.Key] = it.Reason
		if it.Kind != "" {
			intents = append(intents, it)
		}
	}
	for _, ap := range a.Plans {
		intents = append(intents, createIntent(cfg, w.Env.ConfigRev, ap))
	}
	var unregistered []intent
	if p.effects != nil {
		intents, unregistered = splitRegistered(intents, p.creates != nil)
	}
	res := admit(admitInput{
		Now: now, Cfg: cfg, Bucket: p.bucket, FairSeed: p.fairSeed, InFlight: w.InFlight, BringUp: w.Census.BringUp(cfg),
		Endpoints: w.Gates, Backoff: w.Backoff, Paused: w.Paused,
		BootOpen: w.Boot.open(), // P2
	}, intents)
	res.Deferred = append(res.Deferred, unregistered...)
	// The planner state admission leaves is stored before any submit. A
	// trace-only pass submits nothing, so it keeps no token debit, and no
	// refill is a reason for a pass.
	p.fairSeed = res.FairSeed
	if p.effects != nil {
		p.bucket, next = res.Bucket, earliest(next, res.NextToken)
		p.submit(&w, &a, res.Admitted)
	}
	rec.Admitted, rec.Deferred = res.Admitted, res.Deferred
	rec.Rows = p.traceRows(w.Census, reasons, res)

	p.out.relevant.Store(newRelevantSet(w.Demand))
	p.out.summary.Store(newAllocSummary(now, &w, &a))
	counts := passCountsOf(res, w.InFlight)
	p.observeWorld(&w, a.Alerts, &counts)
	return passResult{Next: next, Counts: counts}
}

// newAllocSummary is the summary a pass at now publishes for C8's steps. Any
// census leg error makes it partial, a hard one on a rig leg included
// (Census.Partial counts only partial reads), so the release fails closed.
func newAllocSummary(now time.Time, w *World, a *allocDecision) *allocSummary {
	v := w.Demand
	s := &allocSummary{
		At: now, Partial: v.StorePartial,
		AssignedWork: v.AssignedWork, AssignedStores: v.AssignedStores, AssignedStoreRefs: v.AssignedStoreRefs,
		ReadyAssigned: v.ReadyAssigned, ReadyRouted: a.ReadyRouted, ReadyRoutedRefs: a.ReadyRoutedRefs,
	}
	for _, l := range w.Census.Legs {
		s.Partial = s.Partial || l.Err != nil
	}
	for _, row := range w.Census.Canonical() {
		s.OpenSessions = append(s.OpenSessions, row.Info)
	}
	return s
}

// splitRegistered splits off the intents whose kind has no registered
// effect yet, and the creates when no create runner is wired, deferred with
// cause no-effect before admission, so they take no cap, no token and no
// backoff: the row is traced and the arm stays visible until its effect
// lands.
func splitRegistered(intents []intent, creates bool) (registered, unregistered []intent) {
	for _, it := range intents {
		if effectRegistry[it.Kind] == nil || (it.Kind == intentCreate && !creates) {
			it.Cause = causeNoEffect
			unregistered = append(unregistered, it)
			continue
		}
		registered = append(registered, it)
	}
	return registered, unregistered
}

// submit hands each admitted intent's effect to the executor under a new
// in-flight entry, recorded first so the next pass counts it; the creates'
// inputs are built on the first admitted create. A create gets
// its instance token minted here, into its plan and its entry, which is
// keyed by it (S-8, P5). A submit the executor refuses (busy, or stopped)
// runs and posts nothing, so its entry is settled here at once.
func (p *planner) submit(w *World, a *allocDecision, admitted []intent) {
	if len(admitted) == 0 {
		return
	}
	pass := newEffectPass(w, a)
	pass.creates = p.creates
	pass.Stderr = p.stderr
	for _, it := range admitted {
		e := inflightEntry{Kind: it.Kind, Key: it.Key, Endpoint: it.Endpoint}
		if it.Kind == intentCreate {
			if pass.create == nil {
				pass.create = newCreatePass(w)
			}
			it.CreatePlan.Token = session.NewInstanceToken()
			e = createInflightEntry(it, w.SessionsLeg)
		}
		seq := p.inflight.add(e)
		if seq == 0 {
			continue
		}
		it.CreatePlan.Seq = seq
		if err := p.effects.submitIntent(pass, it, seq); err != nil {
			p.inflight.settle(settlement{Key: it.Key, Kind: it.Kind, Seq: seq, Token: e.Token})
		}
	}
}

// createInflightEntry is an admitted create's in-flight entry: its token, identity
// and endpoint, and the planning stand-in it reserves until the census shows
// its row (inFlightCreates).
func createInflightEntry(it intent, leg string) inflightEntry {
	c := it.CreatePlan
	e := inflightEntry{
		Kind: it.Kind, Endpoint: it.Endpoint, Token: c.Token, Identity: c.identity().key(), Leg: leg,
		Template: c.Template, QualifiedInstance: c.QualifiedInstance, Slot: c.Slot, WorkBeadID: c.WorkBeadID,
	}
	if c.Named != nil {
		e.Template, e.SessionName = c.Named.Template, c.Named.SessionName
	}
	return e
}

// clearAmbiguous clears the ambiguous creates w's census shows, by token or,
// for a named create, by an open canonical row of its identity, or whose
// hard bound passed, alerting on each hard-bound clear (P5), and refreshes
// w's in-flight view.
func (p *planner) clearAmbiguous(w *World, now time.Time) {
	c := inflightCensus{Tokens: make(map[string]bool), Named: make(map[string]bool)}
	for _, row := range w.Census.Rows {
		if row.InstanceToken != "" {
			c.Tokens[row.InstanceToken] = true
		}
	}
	for _, row := range w.Census.Canonical() {
		if id := strings.TrimSpace(row.Info.ConfiguredNamedIdentity); id != "" && !row.Info.Closed {
			c.Named[createIdentity{QualifiedInstance: id, Named: true}.key()] = true
		}
	}
	p.alertCleared(p.inflight.clearVisible(c, now))
	w.InFlight = p.inflight.view()
}

// decideRowSafe is decideRow with P6's panic isolation: a row that panics
// holds, and is traced with reason panic.
func (p *planner) decideRowSafe(w *World, a *allocDecision, k rowKey) (it intent, next time.Time) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(p.stderr, "v2 planner: row %s/%s panicked: %v\n%s\n", k.Leg, k.ID, r, debug.Stack()) //nolint:errcheck // best-effort stderr
			it, next = intent{Key: k, Reason: decidePanic}, time.Time{}
		}
	}()
	return decideRow(w, a, k)
}

// createIntent is the create intent for one of the allocation's plans, as
// allocPlan documents it, on its row-to-be's endpoint.
func createIntent(cfg *config.City, rev string, ap allocPlan) intent {
	plan := createPlan{ID: ap.identity(), Named: ap.Named}
	if ap.Named == nil {
		plan = createPlanOf(ap.identity(), ap.Template, ap.Plan)
		plan.WorkBeadID = ap.Request.WorkBeadID
	}
	plan.ConfigRev = rev
	return intent{
		Kind: intentCreate, Reason: "create-" + ap.Kind.String(), CreatePlan: plan, Floor: ap.Request.FloorGuarantee,
		Endpoint: rowEndpoint(cfg, session.Info{Template: ap.Template}),
	}
}

// traceRows returns the decided rows whose (reason, outcome) changed since
// they were last traced, in census order (R6), and forgets rows the census
// no longer holds. A row skipped for its effect in flight keeps its last.
func (p *planner) traceRows(c *sessionCensus, reasons map[rowKey]string, res admitResult) []rowTrace {
	outcomes := make(map[rowKey]string)
	for _, it := range res.Admitted {
		outcomes[it.Key] = outcomeAdmitted
	}
	for _, it := range res.Deferred {
		outcomes[it.Key] = outcomeDeferred + it.Cause
	}
	var out []rowTrace
	for _, row := range c.Canonical() {
		reason, decided := reasons[row.Key]
		t := rowTrace{Key: row.Key, Template: row.Info.Template, SessionName: row.Info.SessionNameMetadata, Reason: reason, Outcome: cmp.Or(outcomes[row.Key], outcomeNone)}
		if line := t.Reason + "/" + t.Outcome; decided && p.rowTrace[row.Key] != line {
			p.rowTrace[row.Key] = line
			out = append(out, t)
		}
	}
	for k := range p.rowTrace {
		if _, ok := c.Rows[k]; !ok {
			delete(p.rowTrace, k)
		}
	}
	return out
}

// passCountsOf is the metrics' view of one pass: admitted and deferred
// intents by kind, and the in-flight depth by kind.
func passCountsOf(res admitResult, v inflightView) passCounts {
	c := passCounts{Admitted: make(map[string]int), Deferred: make(map[deferral]int), InFlight: make(map[string]int)}
	for _, it := range res.Admitted {
		c.Admitted[it.Kind]++
	}
	for _, it := range res.Deferred {
		c.Deferred[deferral{Kind: it.Kind, Cause: it.Cause}]++
	}
	for _, f := range v.Entries {
		c.InFlight[f.Kind]++
	}
	return c
}

// earliest is the earlier of two deadlines, where zero is none.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
