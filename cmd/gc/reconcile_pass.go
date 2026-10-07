package main

import (
	"cmp"
	"fmt"
	"runtime/debug"
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
	res := admit(admitInput{
		Now: now, Cfg: cfg, Bucket: p.bucket, FairSeed: p.fairSeed, InFlight: w.InFlight, BringUp: w.Census.BringUp(cfg),
		Endpoints: w.Gates, Backoff: w.Backoff, Paused: w.Paused,
		BootOpen: w.Boot.CachePrimed && w.Boot.InventoryComplete && w.Boot.RecordingSeen, // P2
	}, intents)
	// The planner state admission leaves is stored before any submit. A
	// trace-only pass submits nothing, so it keeps no token debit, and no
	// refill is a reason for a pass.
	p.fairSeed = res.FairSeed
	if v2EffectsReal {
		p.bucket, next = res.Bucket, earliest(next, res.NextToken)
	}
	rec.Admitted, rec.Deferred = res.Admitted, res.Deferred
	rec.Rows = p.traceRows(w.Census, reasons, res)

	p.out.relevant.Store(newRelevantSet(w.Demand))
	p.out.summary.Store(newAllocSummary(now, &w, &a))
	return passResult{Next: next, Counts: passCountsOf(res, w.InFlight)}
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
	}
	plan.ConfigRev = rev
	return intent{
		Kind: intentCreate, Reason: "create-" + ap.Kind.String(), Create: plan, Floor: ap.Request.FloorGuarantee,
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
