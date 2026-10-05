package main

import (
	"sort"
	"sync"
	"time"
)

// The allocator's intent ledger (CONTRACT §5, I8, as amended by AM1 and on
// 2026-10-04 per SIMPLIFICATION-CHECKPOINT C2, C3 and C6): the
// in-memory record of effects the allocator admitted that the census may not
// show yet. A pass counts an effect through its entry until the effect's
// marker (the row a create wrote, or the incarnation a start's PreWake wrote)
// is in the census that pass counts; then it clears the entry and counts the
// row instead. So no pass counts an effect twice or not at all (I-ledger),
// and clearing needs only the effect's own row, never a store-wide watermark.
// The ledger is planning bookkeeping, not a correctness source: any read may
// be stale, clearing errs toward fewer starts, and every effect re-checks at
// its own boundary (C0.6).
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
	// ledgerHardBound is how long a landed effect's marker may stay out of
	// the census before the allocator clears the entry as unwritten and
	// alerts (C5.4(5), C5.15 as amended 2026-10-04).
	ledgerHardBound = 10 * time.Minute
)

type ledgerKind uint8

const (
	kindCreate ledgerKind = iota + 1
	kindGrant
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
// terminal: the entry leaves the ledger.
//
//	reserved ──allocator release (TTL, deselected, ineligible, stale ConfigRev)──► released (refund: grant 1, create 0)
//	reserved ──session key (grant) / create executor (create)──────────────────► issued
//	issued   ──effect landed (marker set)──────────────────────────────────────► committed
//	issued   ──effect failed or not dispatched (deferred finalizer, C5.6)──────► failed (no refund)
//	committed | failed ──allocator at pass start (C5.4)────────────────────────► cleared
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
	Key      rowKey // grant: the row. create: the sessions leg, then the row the effect reports
	Template string
	Endpoint endpointKey
	// ConfigRev, Epoch and SelGen are the pass that reserved the entry.
	ConfigRev  string
	Epoch      string
	SelGen     uint64
	ReservedAt time.Time
	State      ledgerState
	// Marker is the census-visible trace of the effect's write. A create
	// carries its pre-minted InstanceToken from reserve; Reserve refuses one
	// without.
	Marker ledgerMarker
	// WroteRow is set when the effect wrote a row: PreWake landed, or the
	// create committed.
	WroteRow bool
	// Ambiguous marks a create whose write call failed after it may have
	// landed: it resolves against a read that started after SettledAt
	// (C5.4(3)).
	Ambiguous bool
	// SettledAt is when the entry was committed or failed.
	SettledAt time.Time
}

type ledgerMarker struct {
	RowID         string // create: the new bead ID; grant: the row
	InstanceToken string // create: the token the effect minted; grant: the PreWake token
	Incarnation   int64  // grant: the generation PreWake wrote (row generation + 1)
}

// ledgerCensus is what the ledger reads of the census one pass counts. Rows
// holds every open session row on every leg. Legs names the legs the census
// holds rows for this pass (read, or served from last good); only there does
// a missing row prove that the row closed. ReadStarted is when the read
// behind each complete non-exact leg's rows started; an exact leg has none,
// since its cache installs nothing for a write whose call failed.
type ledgerCensus struct {
	Rows        map[rowKey]ledgerRow
	Legs        map[string]bool
	ReadStarted map[string]time.Time
}

// ledgerRow is one census row as the ledger and the in-flight count read it.
type ledgerRow struct {
	Incarnation   int64 // the row's generation
	InstanceToken string
	Endpoint      endpointKey // config-only key (endpointKeyForAgent)
	// StartLease: the row holds its pending-create claim or is creating,
	// and last_woke_at is within the start-in-flight lease (START-043,
	// pendingCreateStartInFlightInfo).
	StartLease bool
	// PendingCreate: a never-started pending create within its lease
	// (POOL-028).
	PendingCreate bool
}

// createIdentity is the identity a create plan materializes. Create
// backoff records key on it (AM-N8): a row key does not exist until the
// create lands.
type createIdentity struct {
	Template          string
	QualifiedInstance string
	// Slot is the plan's pool slot. It is not part of the key; agentIn reads
	// it to re-derive the identity from config.
	Slot int
	// Named marks a configured named session's create: QualifiedInstance is
	// its identity, and Template its backing template.
	Named bool
}

func (c createIdentity) key() string {
	if c.Named {
		return "named:" + c.QualifiedInstance
	}
	return c.Template + "/" + c.QualifiedInstance
}

// intentLedger is shared by the allocator and the session keys. It holds
// tens of entries, bounded by the in-flight cap.
type intentLedger struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*ledgerEntry
}

func newIntentLedger(now func() time.Time) *intentLedger {
	return &intentLedger{now: now, entries: make(map[string]*ledgerEntry)}
}

// Reserve records a create or grant the allocator admitted, debited by the
// caller. It refuses another kind, an empty ID, an ID already present, or a
// create without its instance token or its sessions leg (the marker an
// ambiguous create clears by, and the leg whose read decides it).
func (l *intentLedger) Reserve(e ledgerEntry) bool {
	if e.ID == "" || (e.Kind != kindCreate && e.Kind != kindGrant) ||
		(e.Kind == kindCreate && (e.Marker.InstanceToken == "" || e.Key.Leg == "")) {
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
// grant on k. From here its token is spent whatever the outcome (C5.7). If
// it fails, the key abandons the ticket and re-decides.
func (l *intentLedger) Issue(grantID string, k rowKey) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.transitionLocked(grantID, ledgerReserved, ledgerIssued, func(e *ledgerEntry) bool {
		return e.Kind == kindGrant && e.Key == k
	}, nil)
}

// IssueCreate is the create executor's commitment point: reserved → issued
// for create entry id. It returns the entry's pre-minted instance token,
// which the effect writes on the row, and the ConfigRev it was reserved
// under, which fingerprints the effect's create backoff. If it fails (the
// allocator released the entry first, or id is not a create), the effect
// performs nothing.
func (l *intentLedger) IssueCreate(id string) (token, configRev string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok = l.transitionLocked(id, ledgerReserved, ledgerIssued, isCreateEntry, func(e *ledgerEntry) {
		token, configRev = e.Marker.InstanceToken, e.ConfigRev
	})
	return token, configRev, ok
}

func isCreateEntry(e *ledgerEntry) bool { return e.Kind == kindCreate }

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

// CommitCreate records that create id may have written its row: issued →
// committed, with the row and token as its marker. ambiguous says the write
// call failed, so the row may not exist (C5.4(3)).
func (l *intentLedger) CommitCreate(id string, ambiguous bool, m ledgerMarker) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.settleLocked(id, ledgerCommitted, true, m, isCreateEntry) {
		return false
	}
	l.entries[id].Ambiguous = ambiguous
	return true
}

// Retarget records, before the write, that issued create id reopens the
// closed row rowID rather than writing a new one
// (AM-N2). From then on the entry's key and marker name the row, so a
// census that shows the reopened row before the entry settles counts the
// two as one effect (C5.13) and C5.10's in-flight cause blocks a grant for
// it; the reopen never writes the token the entry was reserved with. It
// refuses an entry that is not an issued create.
func (l *intentLedger) Retarget(id, rowID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[id]
	if e == nil || e.Kind != kindCreate || e.State != ledgerIssued || rowID == "" {
		return false
	}
	e.Key.ID, e.Marker.RowID = rowID, rowID
	return true
}

func (l *intentLedger) settle(id string, to ledgerState, wroteRow bool, m ledgerMarker) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.settleLocked(id, to, wroteRow, m, nil)
}

func (l *intentLedger) settleLocked(id string, to ledgerState, wroteRow bool, m ledgerMarker, allow func(*ledgerEntry) bool) bool {
	now := l.now()
	return l.transitionLocked(id, ledgerIssued, to, allow, func(e *ledgerEntry) {
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

// Release is the allocator's cancel of a reserved entry: reserved →
// released. It returns the refund, 1 for a grant and 0 for a create (C5.7),
// which the caller credits only when ok: a lost release means the key issued
// first and the token is spent.
func (l *intentLedger) Release(id string) (refund int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.entries[id]; e != nil && e.Kind == kindGrant {
		refund = 1
	}
	if !l.transitionLocked(id, ledgerReserved, ledgerReleased, nil, nil) {
		return 0, false
	}
	return refund, true
}

// View returns a copy of every entry, ordered by ID.
func (l *intentLedger) View() []ledgerEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.viewLocked()
}

func (l *intentLedger) viewLocked() []ledgerEntry {
	out := make([]ledgerEntry, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// landed reports a committed or failed effect that wrote a row: it clears
// only by marker, by a read started after it settled, or at the hard bound.
func (e ledgerEntry) landed() bool {
	return (e.State == ledgerCommitted || e.State == ledgerFailed) && e.WroteRow
}

// ledgerClear is the allocator's verdict on one entry at a pass start. A
// clear refunds nothing: an issued grant's token is spent (C5.7), and a
// create debited none.
type ledgerClear uint8

const (
	clearKeep    ledgerClear = iota
	clearWritten             // C5.4(1): the marker is in the census
	// clearUnwritten: C5.4(2), a failure that wrote nothing; or C5.4(3), an
	// ambiguous create whose sessions-leg read started after it settled and
	// lacks its marker, which then records a backoff for its identity.
	clearUnwritten
	// clearHardBound: C5.4(5), cleared as unwritten; the allocator alerts,
	// naming the entry, its key and its leg, and records a trace (C5.15).
	clearHardBound
)

// clearVerdict is the clear the allocator applies to e at the start of a
// pass at now that counts census c. The marker wins over any absence.
func (e ledgerEntry) clearVerdict(c ledgerCensus, now time.Time) ledgerClear {
	switch {
	case e.State == ledgerFailed && !e.WroteRow:
		return clearUnwritten
	case !e.landed():
		return clearKeep
	case e.markerVisible(c):
		return clearWritten
	case e.Ambiguous && c.ReadStarted[e.Key.Leg].After(e.SettledAt):
		return clearUnwritten
	case now.Sub(e.SettledAt) >= ledgerHardBound:
		return clearHardBound
	}
	return clearKeep
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

// reserveExpired reports a grant or create reserved for ledgerReserveTTL
// that no key or executor issued; the allocator releases it, refunding a
// grant's token and freeing its slot, and a later pass may reserve a new one.
func (e ledgerEntry) reserveExpired(now time.Time) bool {
	return e.State == ledgerReserved && !now.Before(e.ReservedAt.Add(ledgerReserveTTL))
}
