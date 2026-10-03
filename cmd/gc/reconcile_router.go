package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
)

// The keyed reconciler's router: it maps every trigger the controller sees
// (bead events, cache-reconcile replays, inventory passes, socket and API
// keys) to the v2 keys that must reconcile — the city-wide allocator and the
// session rows it concerns. It is not runtime.Router, which routes a session
// name to a provider backend.
//
// Two reverse indexes make the mapping possible: identity -> session rows
// (every name a work bead can be assigned under) and work bead -> its last
// seen assignee. Bead events maintain them incrementally; the resync lane
// rebuilds them from cached census reads.
//
// The router has no write path. Everything it does is an enqueue, which is
// why cache-reconcile replays can be routed like any other event without
// re-creating the ga-yoix1 self-echo loop (INC-006, API-002).

// rowKey is the contract's SessionKey (CONTRACT §0): the canonical store leg
// of the session row plus its bead ID. In P2 Leg is always the sessions-class
// store's census label: legacy reconciles only rows read from that store
// (loadSessionBeadSnapshot over the sessions store), and [storage] is
// boot-latched, so the label is stable for the process.
type rowKey struct{ Leg, ID string }

// routeReason says why the router woke a key. It is field-for-field the
// workqueue Reason, so the v2 runtime's sink converts it directly. Urgent
// reasons bypass the per-key backoff gate (amendment A2).
type routeReason struct {
	Kind   string
	Detail string
	Urgent bool
}

// routerSink is function values, not an interface: the v2 runtime is its only
// real implementation (tests pass recorders). Session keys are always added on
// the hot lane; the resync lane is fed by the resync pass, never the router.
type routerSink struct {
	addSession    func(k rowKey, r routeReason)
	wakeAllocator func(r routeReason)
	requestResync func(reason string)
}

// Reason kinds the router emits itself. Enqueue passes its caller's reason
// through.
const (
	routeReasonEvent           = "event"
	routeReasonReplay          = "replay"
	routeReasonUndecodable     = "undecodable"
	routeReasonControlDispatch = "control-dispatch"
	routeReasonInventory       = "inventory"
	routeReasonSupervisor      = "supervisor-reload"
)

// routerUrgentReasons are the operator intents and the supervisor reload:
// the reasons amendment A2 lets bypass the backoff gate.
var routerUrgentReasons = map[string]bool{
	"api":                 true,
	"socket":              true,
	routeReasonSupervisor: true,
}

// routerMaxPending bounds the index ops logged while a rebuild reads the
// census. Reads are cached, so a build normally logs a handful; a build that
// overflows is abandoned (the live maps stay current) and retried.
const routerMaxPending = 1 << 14

// routerMaxTombstones bounds the identities kept for rows closed since the
// last complete rebuild. Past it a close asks for a resync, which clears them.
const routerMaxTombstones = 1 << 12

var errRouterPendingOverflow = errors.New("reconcile router: events overflowed the rebuild log")

// routerIndex is the pair of reverse indexes. A rebuild constructs a fresh
// one and swaps it in.
type routerIndex struct {
	identities map[string]map[rowKey]struct{} // identity -> session rows (multi-map: duplicates and alias reuse)
	keyIdents  map[rowKey][]string            // reverse, for updates and removal
	assignee   map[string]string              // open work bead ID -> last seen assignee
	// tombstones holds the identities of rows closed since the last complete
	// rebuild. A name that resolves only here (the runtime of a row just
	// retired reports gone) wakes the closed row, a cheap no-op, instead of
	// forcing a resync.
	tombstones map[string]map[rowKey]struct{}
	tombCount  int  // entries in tombstones
	tombFull   bool // a close found tombstones at routerMaxTombstones
}

func newRouterIndex() routerIndex {
	return routerIndex{
		identities: make(map[string]map[rowKey]struct{}),
		keyIdents:  make(map[rowKey][]string),
		assignee:   make(map[string]string),
		tombstones: make(map[string]map[rowKey]struct{}),
	}
}

// routerOp is one idempotent index update, logged while a rebuild is in
// flight and replayed onto the rebuilt index.
type routerOp func(*routerIndex)

func upsertSessionOp(k rowKey, identities []string) routerOp {
	return func(x *routerIndex) {
		x.dropSession(k)
		for _, id := range identities {
			rows := x.identities[id]
			if rows == nil {
				rows = make(map[rowKey]struct{})
				x.identities[id] = rows
			}
			rows[k] = struct{}{}
		}
		x.keyIdents[k] = identities
	}
}

// closeSessionOp drops a closed row and keeps identities, its names when it
// closed, as tombstones. They are captured from the live index, so a replay
// onto a rebuilt index that never held the row still tombstones them.
func closeSessionOp(k rowKey, identities []string) routerOp {
	return func(x *routerIndex) {
		x.dropSession(k)
		for _, id := range identities {
			if _, ok := x.tombstones[id][k]; ok {
				continue
			}
			if x.tombCount >= routerMaxTombstones {
				x.tombFull = true
				return
			}
			if x.tombstones[id] == nil {
				x.tombstones[id] = make(map[rowKey]struct{})
			}
			x.tombstones[id][k] = struct{}{}
			x.tombCount++
		}
	}
}

func setAssigneeOp(id, assignee string) routerOp {
	return func(x *routerIndex) { x.assignee[id] = assignee }
}

func dropAssigneeOp(id string) routerOp { return func(x *routerIndex) { delete(x.assignee, id) } }

func (x *routerIndex) dropSession(k rowKey) {
	for _, id := range x.keyIdents[k] {
		delete(x.identities[id], k)
		if len(x.identities[id]) == 0 {
			delete(x.identities, id)
		}
	}
	delete(x.keyIdents, k)
}

// resolve appends every open session row that answers to identity.
func (x *routerIndex) resolve(identity string, out []rowKey) []rowKey {
	for k := range x.identities[strings.TrimSpace(identity)] {
		out = append(out, k)
	}
	return out
}

// resolveName is resolve, falling back to the rows closed under the name.
func (x *routerIndex) resolveName(name string, out []rowKey) []rowKey {
	n := len(out)
	if out = x.resolve(name, out); len(out) > n {
		return out
	}
	for k := range x.tombstones[strings.TrimSpace(name)] {
		out = append(out, k)
	}
	return out
}

// reconcileRouter routes triggers to v2 keys and owns the reverse indexes.
// Index writers are the bead event watcher and the resync pass; readers
// resolve under RLock. Keys are collected under mu and handed to the sink
// after it is released, so the router never holds mu while calling the queue.
type reconcileRouter struct {
	mu          sync.RWMutex
	sessionsLeg string
	idx         routerIndex
	building    bool
	pending     []routerOp // index ops applied while a rebuild is in flight
	overflow    bool       // pending hit routerMaxPending during this build

	rebuildMu sync.Mutex // one rebuild at a time
	sink      routerSink
	stderr    io.Writer

	events, replays, undecodable, keysOut, unresolved, panics atomic.Uint64
}

// routerStats is a point-in-time copy of the router's counters.
type routerStats struct {
	Events, Replays, Undecodable, KeysOut, Unresolved, Panics uint64
}

func newReconcileRouter(sessionsLeg string, sink routerSink, stderr io.Writer) *reconcileRouter {
	if stderr == nil {
		stderr = io.Discard
	}
	return &reconcileRouter{sessionsLeg: sessionsLeg, idx: newRouterIndex(), sink: sink, stderr: stderr}
}

func (r *reconcileRouter) stats() routerStats {
	return routerStats{
		Events:      r.events.Load(),
		Replays:     r.replays.Load(),
		Undecodable: r.undecodable.Load(),
		KeysOut:     r.keysOut.Load(),
		Unresolved:  r.unresolved.Load(),
		Panics:      r.panics.Load(),
	}
}

// recoverMapping keeps a mapping panic from killing the caller's goroutine
// (the bead event watcher has no recover of its own, F7). The dropped trigger
// is repaired by a forced resync.
func (r *reconcileRouter) recoverMapping(site string) {
	p := recover()
	if p == nil {
		return
	}
	r.panics.Add(1)
	fmt.Fprintf(r.stderr, "reconcile router: recovered panic in %s: %v\n", site, p) //nolint:errcheck // best-effort stderr
	r.requestResync("router-panic")
}

func (r *reconcileRouter) requestResync(reason string) {
	if r.sink.requestResync != nil {
		r.sink.requestResync(reason)
	}
}

// apply runs op on the live index and logs it for an in-flight rebuild.
// mu must be held for writing.
func (r *reconcileRouter) apply(op routerOp) {
	op(&r.idx)
	if !r.building {
		return
	}
	if len(r.pending) >= routerMaxPending {
		r.overflow = true
		return
	}
	r.pending = append(r.pending, op)
}

// emit hands deduped session rows to the sink in a stable order.
func (r *reconcileRouter) emit(rows []rowKey, reason routeReason) {
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Leg != rows[j].Leg {
			return rows[i].Leg < rows[j].Leg
		}
		return rows[i].ID < rows[j].ID
	})
	for i, k := range rows {
		if i > 0 && k == rows[i-1] {
			continue
		}
		r.keysOut.Add(1)
		r.sink.addSession(k, reason)
	}
}

func (r *reconcileRouter) wakeAllocator(reason routeReason) {
	r.keysOut.Add(1)
	r.sink.wakeAllocator(reason)
}

// OnBeadEvent routes one bead event after the caches applied it. snapshot is
// a cache-reconcile replay, which routes exactly like a live event but is
// counted apart (F1). appliedToSessions says the event's bead lives in the
// sessions-class store, so a session bead there is a row v2 reconciles.
func (r *reconcileRouter) OnBeadEvent(evt events.Event, snapshot, appliedToSessions bool) {
	defer r.recoverMapping("bead event")
	kind := routeReasonEvent
	if snapshot {
		kind = routeReasonReplay
		r.replays.Add(1)
	} else {
		r.events.Add(1)
	}
	b, ok := beads.DecodeBeadEventPayload(evt.Payload)
	if !ok {
		r.undecodable.Add(1)
		r.wakeAllocator(routeReason{Kind: routeReasonUndecodable, Detail: evt.Type})
		return
	}
	reason := routeReason{Kind: kind, Detail: evt.Type + " " + b.ID}
	rows, tombFull := r.indexBeadEvent(evt.Type, b, appliedToSessions)
	r.emit(rows, reason)
	if tombFull {
		r.requestResync("tombstone-overflow")
	}
	// Every bead event is a census input: a session row's change, a relic on
	// another leg (the create fence), or work demand.
	r.wakeAllocator(reason)
}

// indexBeadEvent updates the indexes for one decoded bead and returns the
// session rows it concerns, and whether a close found the tombstones full.
func (r *reconcileRouter) indexBeadEvent(eventType string, b beads.Bead, appliedToSessions bool) (rows []rowKey, tombFull bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	closedOrDeleted := eventType == events.BeadClosed || eventType == events.BeadDeleted
	gone := closedOrDeleted || b.Status == "closed"
	k := rowKey{Leg: r.sessionsLeg, ID: b.ID}
	_, indexed := r.idx.keyIdents[k]
	// A close or delete payload can carry only the ID; an indexed session row
	// in the sessions store is still that session.
	if session.IsSessionBeadOrRepairable(b) || (closedOrDeleted && appliedToSessions && indexed) {
		if !appliedToSessions {
			return nil, false // a migrated relic (C2.11): census only
		}
		if !gone {
			r.apply(upsertSessionOp(k, sessionBeadAssigneeIdentities(b)))
			return []rowKey{k}, false
		}
		// Logged even when not indexed: a rebuild's census may hold the row.
		r.apply(closeSessionOp(k, r.idx.keyIdents[k]))
		return []rowKey{k}, indexed && r.idx.tombFull
	}
	old, had := r.idx.assignee[b.ID]
	if had {
		rows = r.idx.resolve(old, rows)
	}
	next := strings.TrimSpace(b.Assignee)
	if !gone && next != "" && (b.Status == "open" || b.Status == "in_progress") {
		rows = r.idx.resolve(next, rows)
		if !had || old != next {
			r.apply(setAssigneeOp(b.ID, next))
		}
	} else if had {
		r.apply(dropAssigneeOp(b.ID))
	}
	return rows, false
}

// Enqueue routes externally keyed triggers (API, socket, lanes, pump).
// Allocator and key-less triggers wake the allocator; the control-dispatch key
// is allocator work (MAINT-056). A session ID needs no index; a name resolves
// through the identity index, else to the rows closed under it since the last
// complete rebuild (a retired row's runtime reporting gone), and an unresolved
// name wakes the allocator and asks for a resync. A supervisor reload also
// forces a resync (API-004).
func (r *reconcileRouter) Enqueue(reason string, keys ...reconcilekey.Key) {
	defer r.recoverMapping("enqueue")
	rr := routeReason{Kind: reason, Urgent: routerUrgentReasons[reason]}
	rows, alloc, dispatch, unresolved := r.resolveKeys(keys)
	alloc = alloc || len(keys) == 0 || reason == routeReasonSupervisor

	r.emit(rows, rr)
	if alloc {
		r.wakeAllocator(rr)
	}
	if dispatch {
		r.wakeAllocator(routeReason{Kind: routeReasonControlDispatch, Detail: reason, Urgent: rr.Urgent})
	}
	if unresolved > 0 {
		r.unresolved.Add(uint64(unresolved))
		r.requestResync("unresolved-name")
	}
	if reason == routeReasonSupervisor {
		r.requestResync(routeReasonSupervisor)
	}
}

// resolveKeys maps keys to session rows under RLock. It reports whether an
// allocator key or an unresolved name wants the allocator, whether a
// control-dispatch key was seen, and how many names did not resolve.
func (r *reconcileRouter) resolveKeys(keys []reconcilekey.Key) (rows []rowKey, alloc, dispatch bool, unresolved int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, k := range keys {
		k = k.Normalize()
		switch k.Kind {
		case reconcilekey.KindSession:
			n := len(rows)
			// A StoreRef can only name the sessions leg in P2 (rowKey), so an
			// ID resolves without the index.
			if rows = r.resolveKeyLocked(k, rows); len(rows) == n {
				unresolved++
				alloc = true
			}
		case reconcilekey.KindControlDispatch:
			dispatch = true
		default:
			alloc = true
		}
	}
	return rows, alloc, dispatch, unresolved
}

// OnEventGap reports that the bead event tail broke or regressed: events may
// be missing, so only a rebuild and enqueue-all repairs the indexes.
func (r *reconcileRouter) OnEventGap() {
	r.requestResync("bead-event-gap")
}

// OnInventoryPass routes one inventory pass, given the snapshots before and
// after it. Each name whose facts, incarnation or owner changed, or that the
// cache stopped tracking, wakes its owner's row when the runtime is
// attributed to a session, else the rows its name resolves to. Any such
// change, and a health or primed change, also wakes the allocator once:
// observation flips are selection inputs (CONTRACT §1.1), and an ownerless
// runtime changes singleton occupancy (POOL-052). The cache already applies
// the partial-list rule, so a partial pass yields no absence flip (R14).
func (r *reconcileRouter) OnInventoryPass(prev, next *ObservationSnapshot) {
	defer r.recoverMapping("inventory pass")
	if next == nil {
		return
	}
	if prev == nil {
		prev = &ObservationSnapshot{}
	}
	reason := routeReason{Kind: routeReasonInventory, Detail: fmt.Sprintf("pass %d", next.PassSeq)}
	rows, changed := r.inventoryRows(prev, next)
	alloc := changed || primedChanged(prev.Primed, next.Primed) || healthChanged(prev.Health, next.Health)
	r.emit(rows, reason)
	if alloc {
		r.wakeAllocator(reason)
	}
}

// inventoryRows returns the rows of every name the pass changed, and whether
// any name changed.
func (r *reconcileRouter) inventoryRows(prev, next *ObservationSnapshot) (rows []rowKey, changed bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route := func(name string, obs RuntimeObservation) {
		changed = true
		n := len(rows)
		if obs.OwnerState == OwnerSession {
			rows = r.resolveKeyLocked(obs.Owner, rows)
		}
		if len(rows) == n {
			rows = r.idx.resolveName(name, rows)
		}
	}
	for name, obs := range next.ByName {
		old, had := prev.ByName[name]
		if !observationChanged(old, obs, had) {
			continue
		}
		route(name, obs)
		// A reused name moved to a new owner: the old owner lost its runtime.
		if had && old.OwnerState == OwnerSession && old.Owner != obs.Owner {
			route(name, old)
		}
	}
	for name, old := range prev.ByName {
		if _, ok := next.ByName[name]; !ok {
			route(name, old)
		}
	}
	return rows, changed
}

// resolveKeyLocked appends the rows a session key names. mu must be held.
func (r *reconcileRouter) resolveKeyLocked(k reconcilekey.Key, out []rowKey) []rowKey {
	k = k.Normalize()
	if k.Kind != reconcilekey.KindSession {
		return out
	}
	if k.SessionID != "" {
		return append(out, rowKey{Leg: r.sessionsLeg, ID: k.SessionID})
	}
	return r.idx.resolveName(k.SessionName, out)
}

func healthChanged(prev, next map[string]BackendHealth) bool {
	if len(prev) != len(next) {
		return true
	}
	for label, h := range next {
		if p, ok := prev[label]; !ok || p.State != h.State {
			return true
		}
	}
	return false
}

// routerCensus is what a rebuild reads: the open session rows of the sessions
// store, and the work census legs (censusStoreCandidates). Both must be cached
// reads; a rebuild never reads live.
type routerCensus struct {
	sessions func() ([]session.Info, error)
	legs     func() ([]classStoreCandidate, error)
}

// rebuild replaces the indexes from the census and returns the open session
// rows it indexed. Events that arrive while the census is read update the
// live index and are logged; they are replayed onto the rebuilt index before
// the swap, so nothing is lost. sessErr reports a session index that was not
// rebuilt (the sessions census failed, or the log overflowed and the build was
// abandoned); boot readiness gates on it alone. legErr names the work census
// legs that failed. A partial read never reads as deletion (P-3): a failed
// sessions census keeps the session index whole, and a failed assignee leg
// keeps every previous assignee entry the legs that did read do not
// supersede. Any failure also asks for a resync.
func (r *reconcileRouter) rebuild(c routerCensus) (rows []rowKey, sessErr, legErr error) {
	rows, sessErr, legErr = r.rebuildIndex(c)
	switch {
	case errors.Is(sessErr, errRouterPendingOverflow):
		r.requestResync("rebuild-overflow")
	case sessErr != nil || legErr != nil:
		r.requestResync("census-error")
	}
	return rows, sessErr, legErr
}

func (r *reconcileRouter) rebuildIndex(c routerCensus) (rows []rowKey, sessErr, legErr error) {
	r.rebuildMu.Lock()
	defer r.rebuildMu.Unlock()

	r.mu.Lock()
	r.building, r.pending, r.overflow = true, nil, false
	r.mu.Unlock()
	// Ends the build even when a census read panics.
	defer func() {
		r.mu.Lock()
		r.building, r.pending, r.overflow = false, nil, false
		r.mu.Unlock()
	}()

	infos, sessErr := c.sessions()
	if sessErr != nil {
		sessErr = fmt.Errorf("reconcile router: sessions census: %w", sessErr)
	}
	assignees, legErr := readAssigneeCensus(c.legs)
	return r.swap(infos, sessErr, assignees, legErr)
}

// swap builds the new index under mu, replays the logged ops onto it and
// swaps it in, unless the log overflowed.
func (r *reconcileRouter) swap(infos []session.Info, sessErr error, assignees map[string]string, legErr error) ([]rowKey, error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overflow {
		return nil, errors.Join(errRouterPendingOverflow, sessErr), legErr
	}
	var rows []rowKey
	next := newRouterIndex()
	if sessErr != nil {
		// Tombstones last until a complete rebuild.
		next.identities, next.keyIdents = r.idx.identities, r.idx.keyIdents
		next.tombstones, next.tombCount, next.tombFull = r.idx.tombstones, r.idx.tombCount, r.idx.tombFull
	} else {
		for _, info := range infos {
			if info.Closed {
				continue
			}
			k := rowKey{Leg: r.sessionsLeg, ID: info.ID}
			upsertSessionOp(k, session.AssigneeIdentities(info))(&next)
			rows = append(rows, k)
		}
	}
	if legErr != nil {
		next.assignee = r.idx.assignee
	}
	for id, a := range assignees {
		next.assignee[id] = a
	}
	for _, op := range r.pending {
		op(&next)
	}
	r.idx = next
	return rows, sessErr, legErr
}

// readAssigneeCensus reads open and in-progress work with an assignee over
// every census leg, from the cache. It returns what it read and an error
// naming every leg that failed; rows a failed leg did return are kept.
func readAssigneeCensus(legs func() ([]classStoreCandidate, error)) (map[string]string, error) {
	out := make(map[string]string)
	candidates, err := legs()
	if err != nil {
		return out, fmt.Errorf("reconcile router: census legs: %w", err)
	}
	var errs []error
	for _, leg := range candidates {
		if leg.store == nil {
			continue
		}
		cached := beads.HandlesFor(leg.store).Cached
		for _, status := range []string{"in_progress", "open"} {
			rows, err := cached.List(beads.ListQuery{Status: status})
			if err != nil {
				errs = append(errs, fmt.Errorf("reconcile router: leg %q List(%s): %w", leg.ref, status, err))
			}
			for _, b := range rows {
				if session.IsSessionBeadOrRepairable(b) {
					continue
				}
				if a := strings.TrimSpace(b.Assignee); a != "" {
					out[b.ID] = a
				}
			}
		}
	}
	return out, errors.Join(errs...)
}
