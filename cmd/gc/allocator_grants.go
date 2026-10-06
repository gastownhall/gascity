package main

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/poolplan"
)

// The allocator's admission (P3 spec §4.4 steps 15-17, §4.7; CONTRACT
// C5.4-C5.10 as amended 2026-10-04): ledger housekeeping, the bucket refill,
// grants for existing rows, create admission and the diff. It is planning,
// not a fence: every grant and create re-checks at its effect (C0.6). It only
// emits ledger ops, which P3-7 applies, each a CAS. Grants are sticky; new
// ones go in LRU order while a token, a city slot and the endpoint's gate
// admit. Creates follow, named first, then pool and dependency-floor plans in
// fair share, each only if its row's first grant could be admitted now, and
// never more than the tokens the grants left (C5.8, C5.9).

// Admission causes (C2.2, C5.10), beyond the plan gates.
const (
	gateBackoff          = "backoff"
	gateDepsUnsatisfied  = "deps-unsatisfied"
	reasonAwaitingBudget = "awaiting-budget"
	releaseTTL           = "ttl"
	releaseDeselected    = "deselected"
	releaseStaleConfig   = "stale-config-rev"
)

type ledgerOpKind uint8

const (
	opClear ledgerOpKind = iota + 1
	opRelease
	opReserve
)

// ledgerOp is one ledger move, applied in order by P3-7: a clear is
// Transition(ID, From, cleared) after recording Backoff, a release
// Release(ID), a reserve Reserve(Entry), whose grant debit (already out of
// allocDecision.Bucket) it credits back if Reserve refuses.
type ledgerOp struct {
	Kind   ledgerOpKind
	ID     string
	From   ledgerState
	Clear  ledgerClear
	Reason string
	Entry  ledgerEntry
	// Backoff: an ambiguous create proved unwritten records a stalling
	// create backoff (C5.4(3)); never fence, which advances a slot (F3).
	Backoff *backoffOp
	// Alert is the stderr alert of a hard-bound clear (C5.4(5), C5.15).
	Alert string
}

// backoffOp is a backoffTable.Refuse the apply step makes.
type backoffOp struct {
	Key         string
	Until       time.Time
	Cause       string
	Fingerprint string
}

// admission is steps 15-17's working state.
type admission struct {
	p        *decidePass
	d        *allocDecision
	lc       ledgerCensus
	capacity int
	interval time.Duration
	bucket   bucketState
	// live is the ledger after this pass's clears and before its releases:
	// what the pass counts in flight (P3-4 obligation: a release can lose to
	// an issue, so it still counts).
	live        []ledgerEntry
	inFlight    int
	outstanding map[endpointKey]int
	// parked are the pending rows inFlight counts from the census, and
	// parkedOn their number per endpoint: behind a probe gate they hold
	// the probe against new creates (F5).
	parked   map[rowKey]bool
	parkedOn map[endpointKey]int
	gates    map[endpointKey]endpointGate
	res      map[string]planReservation
	// released are the keys whose grant this pass releases: no new grant
	// for them until the release is applied.
	released map[rowKey]bool
	starved  bool
}

// admit is steps 15-17.
func (p *decidePass) admit(d *allocDecision) {
	a := &admission{
		p: p, d: d, lc: p.in.Census.Ledger(p.cfg),
		capacity: p.cfg.Daemon.MaxWakesPerTickOrDefault(), interval: p.cfg.Daemon.PatrolIntervalDuration(),
		gates:    map[endpointKey]endpointGate{"": gateClosed},
		res:      make(map[string]planReservation),
		released: make(map[rowKey]bool),
	}
	for k, v := range p.in.Endpoints {
		a.gates[k] = v.Gate
	}
	d.FairSeed, d.EntrySeq = p.in.FairSeed, p.in.EntrySeq
	a.housekeep()
	a.bucket = p.in.Bucket.refill(p.in.Now, a.capacity, a.interval)
	a.inFlight, a.parked = cityInFlight(a.live, a.lc, a.gates)
	a.outstanding, a.parkedOn = endpointOutstanding(a.live), make(map[endpointKey]int)
	for k := range a.parked {
		a.parkedOn[a.lc.Rows[k].Endpoint]++
	}
	a.grants()
	a.creates()
	d.Bucket, d.Trace = a.bucket, p.trace
	for _, id := range slices.Sorted(maps.Keys(a.res)) {
		d.Reservations = append(d.Reservations, a.res[id])
	}
	d.Enqueue = p.diff()
	d.NextWake = a.nextWake()
}

// housekeep is step 15's clearing (C5.4(1-3,5)): each entry's verdict
// against this pass's census. A cleared create drops its reservation in the
// same step; a reservation whose entry the ledger no longer holds goes too.
func (a *admission) housekeep() {
	p := a.p
	for _, r := range p.in.Reservations {
		a.res[r.EntryID] = r
	}
	held := make(map[string]bool)
	for _, e := range p.in.Ledger {
		v := e.clearVerdict(a.lc, p.in.Now)
		if v == clearKeep {
			a.live = append(a.live, e)
			held[e.ID] = true
			continue
		}
		op := ledgerOp{Kind: opClear, ID: e.ID, From: e.State, Clear: v}
		if r, ok := a.res[e.ID]; ok && v == clearUnwritten && e.Ambiguous {
			op.Backoff = &backoffOp{
				Key: createBackoffKey(reservationIdentity(r)), Until: p.in.Now.Add(3 * a.interval),
				Cause: createStageWrite, Fingerprint: e.ConfigRev,
			}
		}
		if v == clearHardBound {
			op.Alert = fmt.Sprintf("allocator: ledger entry %s (key %s/%s, leg %s) cleared as unwritten: its marker missed the census for %s",
				e.ID, e.Key.Leg, e.Key.ID, e.Key.Leg, ledgerHardBound)
			p.trace = append(p.trace, allocTraceRecord{Template: e.Template, Key: e.Key, Reason: "ledger-hard-bound:" + e.ID})
		}
		a.d.LedgerOps = append(a.d.LedgerOps, op)
	}
	for id := range a.res {
		if !held[id] {
			delete(a.res, id)
		}
	}
}

// reservationIdentity is a create's identity from its reservation, the key
// its create backoff is kept under (allocPlan.identity).
func reservationIdentity(r planReservation) string {
	if r.NamedIdentity != "" {
		return createIdentity{Named: true, QualifiedInstance: r.NamedIdentity}.key()
	}
	return createIdentity{Template: r.Template, QualifiedInstance: r.QualifiedInstance}.key()
}

// grants is steps 16-17's grant half: eligibility, rank, sticky republish and
// release (C5.4(4)), and new grants.
func (a *admission) grants() {
	p := a.p
	keys := slices.Collect(maps.Keys(p.snap.Entries))
	sortRowKeys(keys)
	var ranked []*selectionEntry
	for _, k := range keys {
		e := p.snap.Entries[k]
		if e.Desired != desireWake || e.Liveness.alive() {
			continue
		}
		if cause := a.ineligible(e); cause != "" {
			e.Reason = "ineligible:" + cause
			continue
		}
		ranked = append(ranked, e)
	}
	// LRU by legacy's wakeFairnessTime (START-009), stable on the key
	// order: managed rows are all on the sessions leg, so ties go by bead ID.
	sort.SliceStable(ranked, func(i, j int) bool {
		return wakeFairnessTime(startCandidate{info: p.in.Census.Rows[ranked[i].Key].Info}).
			Before(wakeFairnessTime(startCandidate{info: p.in.Census.Rows[ranked[j].Key].Info}))
	})
	rotateProbeSlots(ranked, func(e *selectionEntry) endpointKey { return e.Endpoint },
		func(k endpointKey, e *selectionEntry) int { return p.in.Endpoints[k].Refusals[e.Key.ID] })
	for i, e := range ranked {
		e.Start = &startView{Rank: i}
	}
	for _, g := range a.live {
		e := p.snap.Entries[g.Key]
		switch {
		case g.Kind == kindCreate:
			switch {
			case g.reserveExpired(p.in.Now):
				a.release(g, releaseTTL)
			case g.State == ledgerReserved && g.ConfigRev != p.in.ConfigRev:
				a.release(g, releaseStaleConfig)
			}
			continue
		case g.State == ledgerIssued && e != nil && e.Start != nil && e.Start.Grant == "":
			e.Start.Grant = g.ID
			continue
		case g.State != ledgerReserved:
			continue
		case e == nil || e.Desired != desireWake || e.Liveness.alive() || (e.Start != nil && e.Start.Grant != ""):
			a.release(g, releaseDeselected)
		case e.Start == nil:
			a.release(g, e.Reason)
		case g.reserveExpired(p.in.Now):
			a.release(g, releaseTTL)
		case g.ConfigRev != p.in.ConfigRev:
			a.release(g, releaseStaleConfig)
		default:
			e.Start.Grant = g.ID
		}
	}
	for _, e := range ranked {
		if e.Start.Grant != "" || a.released[e.Key] {
			continue
		}
		// A parked row already holds its city slot: its grant continues
		// that effect and is counted once (C5.9, C5.13).
		held := a.parked[e.Key]
		if !a.admits(e.Endpoint, a.bucket.Tokens, held, 0) {
			e.Reason, a.starved = reasonAwaitingBudget, true
			continue
		}
		a.bucket.Tokens--
		e.Start.Grant = a.reserve(ledgerEntry{Kind: kindGrant, Key: e.Key, Template: e.Template, Endpoint: e.Endpoint}, held)
	}
}

// ineligible is C5.10's cause for a Wake entry whose runtime is not alive,
// or "". The step 2 classes are None and never reach here; a row's own
// reserved or issued grant does not make it ineligible (r3 B1).
func (a *admission) ineligible(e *selectionEntry) string {
	p := a.p
	row := p.in.Census.Rows[e.Key]
	agent := findAgentByTemplate(p.cfg, e.Template)
	switch {
	case !e.Liveness.startCandidate():
		return gateLivenessUnknown
	case a.gates[e.Endpoint] == gateShut:
		return gateEndpointShut
	case agent != nil && p.bp != nil && p.providerRed(agent):
		return gateProviderRed
	case p.quarantined(startupHealthEpisodeKey(row.Info, row.Info.SessionNameMetadata)):
		return gateQuarantine
	case p.in.Backoff[rowBackoffKey(e.Key)].live(p.in.Now):
		return gateBackoff
	case a.busy(e.Key, row.InstanceToken):
		return gateInFlight
	case agent != nil && !a.depsAlive(agent.DependsOn):
		return gateDepsUnsatisfied
	}
	return ""
}

// busy reports an uncleared entry for k other than its own reserved or
// issued grant (C5.5). A create matches its row by key or by instance
// token: one the census shows before it settles has no key yet, and a
// reopen never writes its token (C5.13).
func (a *admission) busy(k rowKey, token string) bool {
	for _, e := range a.live {
		switch {
		case e.Kind == kindGrant && e.Key == k && e.State != ledgerReserved && e.State != ledgerIssued:
			return true
		case e.Kind == kindCreate && (e.Key == k || (token != "" && e.Marker.InstanceToken == token)):
			return true
		}
	}
	return false
}

// depsAlive is C2.12 (START-006): every configured dependency template has
// an InDesired member whose runtime is alive and whose start is not in
// flight. A suspended dependency has none, so it blocks, as in legacy.
// Unknown liveness is not alive. No waves: a dependent waits, it is not
// ordered (C8).
func (a *admission) depsAlive(deps []string) bool {
	p := a.p
	for _, dep := range deps {
		agent := findAgentByTemplate(p.cfg, dep)
		if agent == nil {
			continue
		}
		alive := false
		for k, e := range p.snap.Entries {
			alive = alive || (e.Template == agent.QualifiedName() && e.InDesired && e.Liveness.alive() && !a.starting(k))
		}
		if !alive {
			return false
		}
	}
	return true
}

// starting is legacy's dependencySessionStartInFlight over the pass's view:
// k's census row holds a start lease, or an uncleared entry names k, by key
// or by create token. Unlike busy, the row's own grant counts: its start
// has not resolved.
func (a *admission) starting(k rowKey) bool {
	r := a.lc.Rows[k]
	if r.StartLease {
		return true
	}
	for _, e := range a.live {
		if e.Key == k || (e.Kind == kindCreate && r.InstanceToken != "" && e.Marker.InstanceToken == r.InstanceToken) {
			return true
		}
	}
	return false
}

// admits is admitStart over the pass's counts: tokens left, the city cap
// less the slot a parked row already holds, and k's gate with its
// outstanding grants and creates plus extra.
func (a *admission) admits(k endpointKey, tokens int, held bool, extra int) bool {
	inFlight := a.inFlight
	if held {
		inFlight--
	}
	return admitStart(bucketState{Tokens: tokens}, inFlight, a.capacity, a.gates[k], a.outstanding[k]+extra)
}

// creates is step 17's create half (C5.8, C5.9): named plans, then pool and
// dependency-floor plans in fair-share order within the tokens the grants
// left. A create debits nothing, but holds the token its row's first grant
// needs, a city slot and its endpoint's probe. The seed advances only when
// a fair-share create was admitted, so passes without tokens do not decide
// the rotation.
func (a *admission) creates() {
	p := a.p
	left := a.bucket.Tokens
	var pool []allocPlan
	for _, ap := range a.d.Plans {
		if ap.Kind != createNamed {
			pool = append(pool, ap)
		} else {
			a.create(ap, &left, nil)
		}
	}
	if len(pool) == 0 {
		return
	}
	var demands []poolplan.Demand
	for _, ap := range pool {
		i := slices.IndexFunc(demands, func(d poolplan.Demand) bool { return d.Template == ap.Template })
		if i < 0 {
			i, demands = len(demands), append(demands, poolplan.Demand{Template: ap.Template})
		}
		demands[i].FreshCreates++
		demands[i].HasFloor = demands[i].HasFloor || ap.Request.FloorGuarantee || ap.Kind == createDependency
	}
	budget := poolplan.NewCreateBudget(left)
	budget.ConfigureFairShare(demands, p.in.FairSeed)
	admitted := false
	for _, ap := range pool {
		admitted = a.create(ap, &left, budget) || admitted
	}
	if admitted {
		a.d.FairSeed++
	}
}

// create admits one plan if a grant for its row could be admitted now and
// budget (nil for the named tier) grants its template a share. Behind a
// probe gate the endpoint's parked rows count as outstanding: they wait for
// the probe, and a create would only park another (F5).
func (a *admission) create(ap allocPlan, left *int, budget *poolplan.CreateBudget) bool {
	p := a.p
	if p.sessionsLeg == "" || !a.admits(ap.Endpoint, *left, false, a.parkedOn[ap.Endpoint]) ||
		(budget != nil && !budget.TryClaim(ap.Template)) {
		a.starved = true
		p.trace = append(p.trace, allocTraceRecord{Template: ap.Template, Instance: ap.identity(), Reason: reasonAwaitingBudget})
		return false
	}
	*left--
	id := a.reserve(ledgerEntry{Kind: kindCreate, Key: rowKey{Leg: p.sessionsLeg}, Template: ap.Template, Endpoint: ap.Endpoint}, false)
	r := planReservation{EntryID: id, Template: ap.Template, ReservedAt: p.in.Now}
	if ap.Named != nil {
		r.NamedIdentity, r.SessionName = ap.Named.Identity, ap.Named.SessionName
		a.d.Creates = append(a.d.Creates, createPlan{EntryID: id, Named: ap.Named})
	} else {
		r.QualifiedInstance, r.Slot, r.WorkBeadID = ap.Plan.qualifiedInstance, ap.Plan.slot, ap.Request.WorkBeadID
		a.d.Creates = append(a.d.Creates, createPlanOf(id, ap.Template, ap.Plan))
	}
	a.res[id] = r
	return true
}

// reserve records e's reserve op under the next entry ID, counting it
// outstanding on its endpoint and in flight unless held, a parked row's slot
// cityInFlight already counts. A create gets its instance token here: the
// epoch's prefix and the sequence, unique within the process.
func (a *admission) reserve(e ledgerEntry, held bool) string {
	p := a.p
	a.d.EntrySeq++
	e.ID = fmt.Sprintf("grant:%s:%d", p.in.Epoch, a.d.EntrySeq)
	if e.Kind == kindCreate {
		e.ID = fmt.Sprintf("create:%s:%d", p.in.Epoch, a.d.EntrySeq)
		e.Marker.InstanceToken = fmt.Sprintf("%.16s%016x", p.in.Epoch, a.d.EntrySeq)
	}
	e.ConfigRev, e.Epoch, e.SelGen, e.ReservedAt = p.in.ConfigRev, p.in.Epoch, p.in.SelGen, p.in.Now
	if held {
		delete(a.parked, e.Key)
	} else {
		a.inFlight++
	}
	a.outstanding[e.Endpoint]++
	a.d.LedgerOps = append(a.d.LedgerOps, ledgerOp{Kind: opReserve, ID: e.ID, Entry: e})
	return e.ID
}

// release cancels a reserved entry (C5.4(4)). The pass keeps counting it,
// and a released create keeps its reservation: the release may lose to an
// issue. Housekeeping drops the reservation once the ledger no longer holds
// the entry.
func (a *admission) release(e ledgerEntry, why string) {
	a.d.LedgerOps = append(a.d.LedgerOps, ledgerOp{Kind: opRelease, ID: e.ID, From: ledgerReserved, Reason: why})
	if e.Kind == kindGrant {
		a.released[e.Key] = true
	}
}

// nextWake is when the allocator must pass again: the bucket refilling a
// token for a starved candidate, a reserved entry's TTL (a released one only
// wakes it early), or a backoff record expiring; 0 is the patrol backstop.
func (a *admission) nextWake() time.Duration {
	p := a.p
	var wake time.Duration
	consider := func(d time.Duration) {
		if d > 0 && (wake == 0 || d < wake) {
			wake = d
		}
	}
	if a.starved {
		consider(a.bucket.untilTokens(1, a.capacity, a.interval))
	}
	for _, op := range a.d.LedgerOps {
		if op.Kind == opReserve {
			consider(ledgerReserveTTL)
		}
	}
	for _, e := range a.live {
		if e.State == ledgerReserved {
			consider(e.ReservedAt.Add(ledgerReserveTTL).Sub(p.in.Now))
		}
	}
	for _, r := range p.in.Backoff {
		consider(r.Until.Sub(p.in.Now))
	}
	return wake
}

// diff is the C2.10 enqueue list: every key whose entry was added, removed
// or changed in what its session key acts on, or whose template or leg
// partial state changed.
func (p *decidePass) diff() []rowKey {
	prev := p.in.Prev
	var out []rowKey
	if prev == nil {
		prev = &selectionSnapshot{}
	}
	for k, e := range p.snap.Entries {
		if pe := prev.Entries[k]; pe == nil || entryDigest(pe) != entryDigest(e) || partialChanged(prev, p.snap, e) {
			out = append(out, k)
		}
	}
	for k := range prev.Entries {
		if p.snap.Entries[k] == nil {
			out = append(out, k)
		}
	}
	sortRowKeys(out)
	return out
}

func entryDigest(e *selectionEntry) string {
	var grant, binding string
	if e.Start != nil {
		grant = e.Start.Grant
	}
	if e.Binding != nil {
		binding = e.Binding.ID
	}
	return fmt.Sprintf("%v|%s|%s|%s|%v|%s|%v|%v", e.Desired, e.Reason, grant, binding, e.Floor, e.DrainReason, e.Normalize, e.Identity)
}

func partialChanged(prev, cur *selectionSnapshot, e *selectionEntry) bool {
	pt, ct := prev.Partial.Templates[e.Template], cur.Partial.Templates[e.Template]
	return pt.Retain != ct.Retain || !slices.Equal(pt.Causes, ct.Causes)
}
