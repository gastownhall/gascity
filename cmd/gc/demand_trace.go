package main

import (
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Read points of the demand pass, in tick order. Each is one place where the
// pass reads its stores; a snapshot never spans two of them because demand
// writes land in between (see readyDemandCache).
const (
	demandReadPointFingerprint      = "fingerprint"
	demandReadPointSessionCensus    = "session_census"
	demandReadPointAssigned         = "assigned"
	demandReadPointUnassignedRouted = "unassigned_routed"
	demandReadPointDemand           = "demand"
)

// Handle tiers a demand read asks for. "cached" is the logical cached handle,
// which a store without a cache answers from its backing.
const (
	demandReadTierCached = "cached"
	demandReadTierLive   = "live"
)

// demandTraceErrorMax bounds the error text a store-read record carries: a
// backend error can embed a whole subprocess stderr.
const demandTraceErrorMax = 256

// demandStoreRead names one store read on the demand path: the read point
// that issued it, the canonical label of the store that served it, the
// operation, and the handle tier.
type demandStoreRead struct {
	point, leg, op, tier string
}

// recordDemandStoreRead emits one operation record for one store read of the
// demand pass, under the same site as the sub-phase records. The sub-phases
// say which pass was slow; these say which store and which read inside it, so
// a slow binding or rig store is attributable from production traces. A read
// record overlaps the sub-phase that issued it and must not be summed with it.
// rows < 0 omits the row count for reads that do not return a row set. A nil
// trace costs one branch.
func recordDemandStoreRead(trace *sessionReconcilerTraceCycle, read demandStoreRead, start time.Time, rows int, err error) {
	if trace == nil {
		return
	}
	fields := map[string]any{
		"point": read.point,
		"leg":   read.leg,
		"op":    read.op,
		"tier":  read.tier,
	}
	if rows >= 0 {
		fields["rows"] = rows
	}
	outcome := TraceOutcomeComplete
	if err != nil {
		outcome = TraceOutcomeFailed
		if beads.IsPartialResult(err) {
			outcome = TraceOutcomePartial
		}
		msg := err.Error()
		if len(msg) > demandTraceErrorMax {
			msg = msg[:demandTraceErrorMax]
		}
		fields["error"] = msg
	}
	trace.RecordControllerOperation(TraceSiteDemandSnapshot, TraceReasonRetained, outcome, "demand_snapshot.store_read", trace.demandSince(start), fields)
}

// demandPassTrace is one demand pass's trace sink plus the labels naming the
// physical stores its reads hit.
//
// Each arm of the pass spells its legs in its own vocabulary (the assigned arm
// names the city work store "", the session arm "city:<ws>"), and some reads
// are keyed by ROLE rather than leg: the closed named-session index reads "the
// city store" and the demand groups are keyed "city", which on a split city is
// the sessions binding, not the work ledger. A label taken from those keys
// would give two stores one name and one store several. So every census leg the
// pass resolves is recorded here under its canonical label, and a role-keyed
// read is labeled by looking up the store it actually reads.
//
// A nil *demandPassTrace (no trace) records nothing.
type demandPassTrace struct {
	cycle  *sessionReconcilerTraceCycle
	cfg    *config.City
	mu     sync.Mutex
	labels map[beads.Store]string
}

func newDemandPassTrace(cycle *sessionReconcilerTraceCycle, cfg *config.City) *demandPassTrace {
	if cycle == nil {
		return nil
	}
	return &demandPassTrace{cycle: cycle, cfg: cfg, labels: make(map[beads.Store]string)}
}

// now is the start instant for a store read of the pass.
func (p *demandPassTrace) now() time.Time {
	if p == nil {
		return time.Now()
	}
	return p.cycle.demandNow()
}

// leg returns the canonical label of one census leg and remembers the store
// it names. The first label a store is seen under wins; every arm resolves the
// same topology, so a store has one canonical label.
func (p *demandPassTrace) leg(store beads.Store, ref string) string {
	if p == nil {
		return ""
	}
	label := demandLegLabel(p.cfg, ref)
	if store == nil {
		return label
	}
	key := demandLabelKey(store)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.labels[key]; !ok {
		p.labels[key] = label
	}
	return label
}

// storeLabel labels a role-keyed read by the store it reads. A store no census
// leg of the pass named is labeled "unresolved:<role>", which can never
// collide with a leg label.
func (p *demandPassTrace) storeLabel(store beads.Store, role string) string {
	if p == nil {
		return ""
	}
	if store != nil {
		p.mu.Lock()
		label, ok := p.labels[demandLabelKey(store)]
		p.mu.Unlock()
		if ok {
			return label
		}
	}
	return "unresolved:" + role
}

func (p *demandPassTrace) read(read demandStoreRead, start time.Time, rows int, err error) {
	if p == nil {
		return
	}
	recordDemandStoreRead(p.cycle, read, start, rows, err)
}

// demandLabelKey is the identity a store is labeled under: the bead-policy
// wrapper is a front door over the same physical store, so it is looked
// through.
func demandLabelKey(store beads.Store) beads.Store {
	inner, _, _ := unwrapBeadPolicyStore(store)
	return inner
}

// demandLegLabel spells a census leg ref of either vocabulary in the scoped
// one (censusRefScoped): "" is the city work store, a bare name is a rig, and
// scoped and class refs are already canonical.
func demandLegLabel(cfg *config.City, ref string) string {
	switch {
	case ref == "":
		return "city:" + censusCityName(cfg)
	case storeref.IsClassRef(ref), strings.HasPrefix(ref, "city:"), strings.HasPrefix(ref, "rig:"):
		return ref
	default:
		return "rig:" + ref
	}
}

// demandNow is the start instant for a demand sub-phase or store read.
func (c *SessionReconcilerTraceCycle) demandNow() time.Time {
	if c == nil || c.durationClock == nil {
		return time.Now()
	}
	return c.durationClock.Now()
}

func (c *SessionReconcilerTraceCycle) demandSince(start time.Time) time.Duration {
	return c.demandNow().Sub(start)
}
