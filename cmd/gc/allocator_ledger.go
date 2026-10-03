package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// The allocator's intent ledger (CONTRACT §5, I8, as amended by AM1): the
// in-memory record of effects the allocator admitted that the census may not
// show yet. A pass counts an effect through its entry until the effect's
// marker (the row a create wrote, or the incarnation a start's PreWake wrote)
// is in the census that pass counts; then it clears the entry and counts the
// row instead. So no pass counts an effect twice or not at all (I-ledger),
// and clearing needs only the effect's own row, never a store-wide watermark.
//
// One writer per move: only the allocator reserves, releases and clears; a
// session key issues a reserved grant or leaves it alone, then commits or
// fails what it issued; the create executor does the same for creates.
// Nothing is persisted (C5.14): after a restart the rows' durable markers
// stand in for lost entries.
//
// Unwired in this slice: P3-5b reserves, releases and clears from its pure
// decide over View, P3-6 runs creates, and P4.1's session keys issue grants.

const (
	// ledgerReserveTTL is how long a grant or create may stay reserved
	// before the allocator releases it (C5.4(4)), so an entry no key or
	// executor picks up returns its token and slot.
	ledgerReserveTTL = 30 * time.Second
	// ledgerCacheLagBound is how long a landed effect's marker may stay out
	// of the census before lag repair reads the row (C5.15 as amended).
	ledgerCacheLagBound = 60 * time.Second
	// ledgerVetoBase and ledgerVetoMax bound a key's veto backoff (C5.11).
	ledgerVetoBase = 10 * time.Second
	ledgerVetoMax  = 5 * time.Minute
)

type ledgerKind uint8

const (
	kindCreate ledgerKind = iota + 1
	kindGrant
	kindVeto // P4 adds stop, drain and close
)

type ledgerState uint8

const (
	ledgerReserved ledgerState = iota + 1
	ledgerIssued
	ledgerCommitted
	ledgerFailed
	ledgerReleased
	ledgerCleared
)

// ledgerEdges is the state machine (CONTRACT §5.1). Released and cleared are
// terminal: the entry leaves the ledger. A veto is born committed, because it
// records a decision already made, so its only move is the clear.
//
//	reserved ──allocator release (TTL, deselected, ineligible, stale ConfigRev)──► released (refund Cost)
//	reserved ──session key (grant) / create executor (create)──────────────────► issued
//	issued   ──effect landed (marker set)──────────────────────────────────────► committed
//	issued   ──effect failed or not dispatched (deferred finalizer, C5.6)──────► failed
//	committed | failed ──allocator at pass start, marker visible or !WroteRow───► cleared
var ledgerEdges = map[[2]ledgerState]bool{
	{ledgerReserved, ledgerReleased}: true,
	{ledgerReserved, ledgerIssued}:   true,
	{ledgerIssued, ledgerCommitted}:  true,
	{ledgerIssued, ledgerFailed}:     true,
	{ledgerCommitted, ledgerCleared}: true,
	{ledgerFailed, ledgerCleared}:    true,
}

type ledgerEntry struct {
	ID       string
	Kind     ledgerKind
	Key      rowKey // grant and veto: the row. create: Leg only until the effect reports its row
	Template string
	Endpoint endpointKey
	// ConfigRev, Epoch and SelGen are the pass that reserved the entry.
	ConfigRev  string
	Epoch      string
	SelGen     uint64
	ReservedAt time.Time
	State      ledgerState
	// Cost is the tokens debited at reserve: 1 for a create; 0 for the
	// prepaid first grant of a never-PreWaked row (C5.8), else 1.
	Cost int
	// Marker is the census-visible trace of the effect's write. A create
	// carries its pre-minted InstanceToken from reserve; Reserve refuses one
	// without.
	Marker ledgerMarker
	// WroteRow is set when the effect wrote a row: PreWake landed, or the
	// create committed.
	WroteRow bool
	// ProviderCalled is C5.6's start_called, set before provider Start.
	ProviderCalled bool
	// SettledAt is when the entry was committed or failed, or when lag
	// repair last installed its row; lag repair measures
	// ledgerCacheLagBound from it.
	SettledAt time.Time
	// Repair is lag repair's clearing outcome (lagNoMarker, lagClosed or
	// lagNotFound): the next pass clears the entry. Zero until then.
	Repair      lagOutcome
	Until       time.Time // veto expiry
	Consecutive int       // veto count since the key's last issued start (C5.11)
	Reason      string
}

type ledgerMarker struct {
	RowID         string // create: the new bead ID; grant: the row
	InstanceToken string // create: the token the effect minted; grant: the PreWake token
	Incarnation   int64  // grant: the generation PreWake wrote (row generation + 1)
}

// lagOutcome is what lag repair's live read of a lagging effect's row found
// (C5.15 as amended 2026-10-03 per P3-4 review).
type lagOutcome uint8

const (
	// lagInstalled: the row carries the marker and the read installed it in
	// the leg's cache, so the next census clears the entry by marker.
	lagInstalled lagOutcome = iota + 1
	// lagNoMarker: the row is open without the marker (a grant's row below
	// the incarnation PreWake would have written): the write never landed.
	lagNoMarker
	// lagClosed: the row is closed. Its start, if any, is over.
	lagClosed
	// lagNotFound: no row. The write never landed.
	lagNotFound
)

// ledgerCensus is what the ledger reads of the census one pass counts. Rows
// holds every open session row on every leg. Legs names the legs the census
// holds rows for this pass (read, or served from last good); only there does
// a missing row prove that the row closed.
type ledgerCensus struct {
	Rows map[rowKey]ledgerRow
	Legs map[string]bool
}

// ledgerRow is one census row as the ledger and the in-flight count read it.
type ledgerRow struct {
	Incarnation   int64 // the row's generation
	InstanceToken string
	Endpoint      endpointKey // config-only key (endpointKeyForAgent)
	// StartLease: last_woke_at is within the start-in-flight lease
	// (START-043).
	StartLease bool
	// PendingCreate: a never-started pending create within its lease
	// (POOL-028).
	PendingCreate bool
}

// intentLedger is shared by the allocator and the session keys. It holds
// tens of entries, bounded by the in-flight cap plus vetoes.
type intentLedger struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*ledgerEntry
	// vetoes counts each key's consecutive vetoes since its last issued
	// start. It outlives the veto entries, which clear at Until.
	vetoes  map[rowKey]int
	vetoSeq uint64
}

func newIntentLedger(now func() time.Time) *intentLedger {
	return &intentLedger{now: now, entries: make(map[string]*ledgerEntry), vetoes: make(map[rowKey]int)}
}

// Reserve records a create or grant the allocator admitted, debited by the
// caller. It refuses another kind, an empty ID, an ID already present, or a
// create without its instance token (the marker an ambiguous create clears
// by).
func (l *intentLedger) Reserve(e ledgerEntry) bool {
	if e.ID == "" || (e.Kind != kindCreate && e.Kind != kindGrant) ||
		(e.Kind == kindCreate && e.Marker.InstanceToken == "") {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries[e.ID] != nil {
		return false
	}
	e.State = ledgerReserved
	l.entries[e.ID] = &e
	return true
}

// Transition moves id from from to to, a compare-and-swap on State (C5.1),
// and applies mutate under the same lock. It refuses an edge the state
// machine lacks. A lost CAS means the caller re-decides and performs no
// effect.
func (l *intentLedger) Transition(id string, from, to ledgerState, mutate func(*ledgerEntry)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.transitionLocked(id, from, to, nil, mutate)
}

func (l *intentLedger) transitionLocked(id string, from, to ledgerState, allow func(*ledgerEntry) bool, mutate func(*ledgerEntry)) bool {
	e := l.entries[id]
	if e == nil || e.State != from || !ledgerEdges[[2]ledgerState{from, to}] || (allow != nil && !allow(e)) {
		return false
	}
	if mutate != nil {
		mutate(e)
	}
	e.State = to
	if to == ledgerReleased || to == ledgerCleared {
		delete(l.entries, id)
	}
	return true
}

// Issue is the session key's commitment point: reserved → issued for its own
// grant on k. It resets k's veto backoff. If it fails, the key abandons the
// ticket and re-decides.
func (l *intentLedger) Issue(grantID string, k rowKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok := l.transitionLocked(grantID, ledgerReserved, ledgerIssued, func(e *ledgerEntry) bool {
		return e.Kind == kindGrant && e.Key == k
	}, nil)
	if ok {
		delete(l.vetoes, k)
	}
	return ok
}

// MarkProviderCalled records, before provider Start, that the start's
// token is spent whatever the outcome (C5.6, #45).
func (l *intentLedger) MarkProviderCalled(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[id]; e != nil && e.Kind == kindGrant && e.State == ledgerIssued {
		e.ProviderCalled = true
	}
}

// Commit records that id's effect landed: issued → committed, with the
// marker the census will show.
func (l *intentLedger) Commit(id string, m ledgerMarker) bool {
	return l.settle(id, ledgerCommitted, true, m)
}

// Fail records that id's effect failed or was never dispatched: issued →
// failed. wroteRow says whether it wrote a row first (PreWake landed); if so
// the entry clears by marker like a commit.
func (l *intentLedger) Fail(id string, wroteRow bool, m ledgerMarker) bool {
	return l.settle(id, ledgerFailed, wroteRow, m)
}

func (l *intentLedger) settle(id string, to ledgerState, wroteRow bool, m ledgerMarker) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	return l.transitionLocked(id, ledgerIssued, to, nil, func(e *ledgerEntry) {
		e.WroteRow, e.SettledAt = wroteRow, now
		e.Marker.RowID = firstNonEmpty(m.RowID, e.Marker.RowID)
		e.Marker.InstanceToken = firstNonEmpty(m.InstanceToken, e.Marker.InstanceToken)
		if m.Incarnation != 0 {
			e.Marker.Incarnation = m.Incarnation
		}
		if e.Kind == kindCreate && e.Marker.RowID != "" {
			e.Key.ID = e.Marker.RowID
		}
	})
}

// Veto records that k will not issue its grant for a fact the allocator
// cannot see (C5.11). The veto lasts until the later of until and k's
// backoff: 10s doubling per consecutive veto, capped at 5m, reset when k
// issues a start. While k's veto is live, another veto is the same refusal
// seen again: it keeps the count and extends the live veto to the later
// Until. The next pass finds k ineligible and releases its grant with a
// refund.
func (l *intentLedger) Veto(k rowKey, until time.Time, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var live *ledgerEntry
	for id, e := range l.entries {
		switch {
		case e.Kind != kindVeto || e.Key != k:
		case e.Until.After(now):
			live = e
		default:
			delete(l.entries, id) // expired, awaiting its clear
		}
	}
	if live == nil || l.vetoes[k] == 0 {
		l.vetoes[k]++
	}
	n := l.vetoes[k]
	backoff := ledgerVetoMax
	if n <= 6 { // 10s × 2^5 = 320s already exceeds the cap
		backoff = min(ledgerVetoBase<<(n-1), ledgerVetoMax)
	}
	if b := now.Add(backoff); b.After(until) {
		until = b
	}
	if live != nil {
		if until.After(live.Until) {
			live.Until = until
		}
		live.Consecutive, live.Reason = n, reason
		return
	}
	l.vetoSeq++
	id := fmt.Sprintf("veto:%s/%s:%d", k.Leg, k.ID, l.vetoSeq)
	l.entries[id] = &ledgerEntry{
		ID: id, Kind: kindVeto, Key: k, ReservedAt: now, State: ledgerCommitted,
		Until: until, Consecutive: n, Reason: reason,
	}
}

// Release is the allocator's cancel of a reserved entry: reserved →
// released. It returns the refund, which the caller credits only when ok: a
// lost release means the key issued first and the token stays spent until
// the entry clears.
func (l *intentLedger) Release(id string) (refund int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[id]; e != nil {
		refund = e.Cost
	}
	if !l.transitionLocked(id, ledgerReserved, ledgerReleased, nil, nil) {
		return 0, false
	}
	return refund, true
}

// ResolveLag records the outcome of lag repair's live read for a landed
// effect whose marker stayed out of the census (C5.15 as amended 2026-10-03
// per P3-4 review). lagInstalled restarts the bound, so repair reads at most
// once per bound while the census catches up; every other outcome clears
// the entry at the next pass. It refuses an entry that has not landed or
// already holds a clearing outcome.
func (l *intentLedger) ResolveLag(id string, o lagOutcome) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[id]
	if e == nil || !e.landed() || e.Repair != 0 || o < lagInstalled || o > lagNotFound {
		return false
	}
	if o == lagInstalled {
		e.SettledAt = l.now()
	} else {
		e.Repair = o
	}
	return true
}

// ForgetClosed drops the veto backoff of every key whose row c shows closed
// (gone from a leg c holds), so the backoff memory stays bounded by the open
// rows. The allocator calls it with the census each pass counts.
func (l *intentLedger) ForgetClosed(c ledgerCensus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.vetoes {
		if _, open := c.Rows[k]; !open && c.Legs[k.Leg] {
			delete(l.vetoes, k)
		}
	}
}

// View returns a copy of every entry, ordered by ID, for one pass.
func (l *intentLedger) View() []ledgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ledgerEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// landed reports a committed or failed effect that wrote a row: it clears
// only by marker, or by lag repair.
func (e ledgerEntry) landed() bool {
	return (e.State == ledgerCommitted || e.State == ledgerFailed) && e.WroteRow
}

// clearVerdict reports whether the allocator clears e at the start of a pass
// at now that counts census c, and the tokens the clear refunds (AM1):
//
//   - a veto clears at Until;
//   - a failure that wrote nothing clears at once;
//   - a landed effect clears when its marker is in c, or lag repair found
//     its row closed, without the marker, or gone. The marker wins: a write
//     that lands after a read proved it absent still clears as written.
//
// A create refunds only if it wrote nothing (a closed row was written). A
// grant refunds its Cost unless the provider was called (#45, START-013).
func (e ledgerEntry) clearVerdict(c ledgerCensus, now time.Time) (clears bool, refund int) {
	refundFor := func(written bool) int {
		if (e.Kind == kindCreate && written) || (e.Kind == kindGrant && e.ProviderCalled) {
			return 0
		}
		return e.Cost
	}
	switch {
	case e.Kind == kindVeto:
		return !e.Until.After(now), 0
	case e.State == ledgerFailed && !e.WroteRow:
		return true, refundFor(false)
	case !e.landed():
		return false, 0
	case e.markerVisible(c), e.Repair == lagClosed:
		return true, refundFor(true)
	case e.Repair == lagNoMarker, e.Repair == lagNotFound:
		return true, refundFor(false)
	}
	return false, 0
}

// markerVisible reports whether c shows e's write. A create shows as its new
// row ID or its instance token on any leg. A grant shows as its row at the
// incarnation PreWake wrote or later, or as its row gone from a leg c holds;
// a grant with no recorded incarnation shows only as gone.
func (e ledgerEntry) markerVisible(c ledgerCensus) bool {
	switch e.Kind {
	case kindCreate:
		for k, r := range c.Rows {
			if (e.Marker.RowID != "" && k.ID == e.Marker.RowID) ||
				(e.Marker.InstanceToken != "" && r.InstanceToken == e.Marker.InstanceToken) {
				return true
			}
		}
	case kindGrant:
		r, ok := c.Rows[e.Key]
		if !ok {
			return c.Legs[e.Key.Leg]
		}
		return e.Marker.Incarnation > 0 && r.Incarnation >= e.Marker.Incarnation
	}
	return false
}

// lagging reports a landed effect whose marker has stayed out of c for
// ledgerCacheLagBound: lag repair must read its row, and its leg is flagged
// (Keep, BlockCreate) until the read resolves.
func (e ledgerEntry) lagging(c ledgerCensus, now time.Time) bool {
	return e.landed() && e.Repair == 0 && !e.markerVisible(c) && now.Sub(e.SettledAt) >= ledgerCacheLagBound
}

// lagOutcomeOf classifies lag repair's live read of e's row: found says the
// read found row r at k (by row ID, or a create's by instance token), open
// that the row is open. The caller reports lagInstalled only after the read
// installed the row in the leg's cache.
func (e ledgerEntry) lagOutcomeOf(k rowKey, r ledgerRow, found, open bool) lagOutcome {
	switch {
	case !found:
		return lagNotFound
	case !open:
		return lagClosed
	case e.markerVisible(ledgerCensus{Rows: map[rowKey]ledgerRow{k: r}}):
		return lagInstalled
	}
	return lagNoMarker
}

// reserveExpired reports a grant or create reserved for ledgerReserveTTL
// that no key or executor issued; the allocator releases it, refunding its
// token and freeing its slot, and a later pass may reserve a new one.
func (e ledgerEntry) reserveExpired(now time.Time) bool {
	return e.Kind != kindVeto && e.State == ledgerReserved && !now.Before(e.ReservedAt.Add(ledgerReserveTTL))
}
