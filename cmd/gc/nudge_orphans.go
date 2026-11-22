package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// An orphaned nudge queue key is one that no open session's queue keys match
// under the claim's own rule (nudgeTarget.matchesQueueAgent), so no claim will
// ever take its items. Enqueue keys an unaliased pool worker's items by its
// session bead ID (or its bead-scoped runtime name) and fences them to that
// session; a respawn is a new bead that lists neither, so the seat's items
// strand until the TTL sweep dead-letters them. A dispatch pass resolves each
// orphaned key, read-only, through its items' session fence to the predecessor
// session's pool seat: when exactly one open session holds that seat, the pass
// hands it the key for that pass, and its claim's Leg B (with its own live
// census) decides whether the items move. The claim re-fences what it takes to
// the successor but leaves it under the predecessor's key, so an item whose
// delivery does not succeed returns to pending fenced to the successor; a later
// pass resolves that fence to the successor itself and hands it the key again,
// for its claim's Leg A. Every other orphaned key is counted and named with a
// nudgeOrphanResolution, and gc nudge status --orphaned lists its items.

// nudgeOrphanResolution says whether an orphaned key's items resolve to a
// successor session holding their predecessor's pool seat, or why not.
type nudgeOrphanResolution string

const (
	// nudgeOrphanSuccessor: exactly one open session takes the items: the one
	// holding the predecessor's pool seat, or the open session an item is
	// already fenced to (a claim that adopted it re-fenced it there).
	nudgeOrphanSuccessor nudgeOrphanResolution = "successor"
	// nudgeOrphanSeatVacant: no open session holds the predecessor's seat.
	nudgeOrphanSeatVacant nudgeOrphanResolution = "seat-vacant"
	// nudgeOrphanSeatAmbiguous: several open sessions hold the seat, or the
	// key's items resolve to different successors.
	nudgeOrphanSeatAmbiguous nudgeOrphanResolution = "seat-ambiguous"
	// nudgeOrphanNotPoolSeat: the predecessor is not a pool-managed session
	// with an agent name, so it names no seat.
	nudgeOrphanNotPoolSeat nudgeOrphanResolution = "not-pool-seat"
	// nudgeOrphanFencedSessionLive: the fenced session still holds its fence,
	// but the open sessions read for the pass do not list it (it is newer than
	// that read), so the pass has no target to hand the key to.
	nudgeOrphanFencedSessionLive nudgeOrphanResolution = "fenced-session-live"
	// nudgeOrphanFencedSessionUnknown: no session record exists for the fence.
	nudgeOrphanFencedSessionUnknown nudgeOrphanResolution = "fenced-session-unknown"
	// nudgeOrphanUnfenced: the key's items carry no session fence.
	nudgeOrphanUnfenced nudgeOrphanResolution = "unfenced"
	// nudgeOrphanStartupTurn: only launch-bound startup turns are due under
	// the key, or it carries an unfenced startup turn a successor's claim
	// would judge as its own.
	nudgeOrphanStartupTurn nudgeOrphanResolution = "startup-turn"
	// nudgeOrphanLookupError: reading the fenced session failed.
	nudgeOrphanLookupError nudgeOrphanResolution = "lookup-error"
	// nudgeOrphanLookupDeferred: the key was past this pass's lookup budget.
	nudgeOrphanLookupDeferred nudgeOrphanResolution = "lookup-deferred"
)

// nudgeOrphanPredecessorLookupsPerPass bounds the session-store reads one
// dispatch pass spends on fenced sessions its snapshot does not list (closed
// rows). Lookups go to the newest due items first; the rest wait a pass. A
// read that settles a fence is carried to later passes
// (nudgeOrphanPredecessorMemo), so the next pass's budget reaches the fences
// no pass has read yet.
const nudgeOrphanPredecessorLookupsPerPass = 32

// unmatchedNudgeQueueKeys returns the keys no target matches under the
// claim's trimmed rule (nudgeTarget.matchesQueueAgent), in keys' order.
func unmatchedNudgeQueueKeys(keys []string, targets []nudgeTarget) []string {
	var orphaned []string
	for _, key := range keys {
		matched := false
		for _, target := range targets {
			if target.matchesQueueAgent(key) {
				matched = true
				break
			}
		}
		if !matched {
			orphaned = append(orphaned, key)
		}
	}
	return orphaned
}

// nudgeOrphanSeat is how one queued item resolves against its fenced
// (predecessor) session's pool seat.
type nudgeOrphanSeat struct {
	Resolution nudgeOrphanResolution
	// Successor is the one open session holding the seat, set when
	// Resolution is nudgeOrphanSuccessor.
	Successor sessionpkg.Info
}

// nudgeOrphanPredecessor is one fenced-session read: the record when found,
// or the resolution that stands for it (unknown, error, deferred).
type nudgeOrphanPredecessor struct {
	info       sessionpkg.Info
	resolution nudgeOrphanResolution
	err        error
	// open reports that info is one of the resolver's open sessions.
	open bool
	// settled reports a store read a later pass may reuse: a closed row,
	// whose pool seat does not change (were it reopened, the open sessions
	// would list it first), or no record at all.
	settled bool
}

// nudgeOrphanSeatResolver resolves orphaned items against the open sessions
// of one read, reading a fenced session the open list does not hold through
// lookup at most budget times (a negative budget is unlimited), once per ID.
// A fence carried answers is not read: an earlier pass settled it.
type nudgeOrphanSeatResolver struct {
	open         []sessionpkg.Info
	lookup       func(id string) (sessionpkg.Info, error)
	budget       int
	carried      map[string]nudgeOrphanPredecessor
	predecessors map[string]nudgeOrphanPredecessor
}

// newNudgeOrphanSeatResolver resolves against open, reading other sessions
// through lookup. A nil lookup reads nothing, so a fenced session open does
// not list resolves fenced-session-unknown.
func newNudgeOrphanSeatResolver(open []sessionpkg.Info, lookup func(id string) (sessionpkg.Info, error), budget int) *nudgeOrphanSeatResolver {
	return &nudgeOrphanSeatResolver{open: open, lookup: lookup, budget: budget, predecessors: make(map[string]nudgeOrphanPredecessor)}
}

// predecessor reads the fenced session id: from the open list, else from an
// earlier pass's settled read, else (the open list deliberately omits closed
// rows) through lookup, within budget.
func (r *nudgeOrphanSeatResolver) predecessor(id string) nudgeOrphanPredecessor {
	if p, ok := r.predecessors[id]; ok {
		return p
	}
	p, known := r.known(id)
	if !known {
		if r.lookup != nil && r.budget == 0 {
			// Not cached: a resolver with no budget left defers every new ID.
			return nudgeOrphanPredecessor{resolution: nudgeOrphanLookupDeferred}
		}
		p = r.read(id)
	}
	r.predecessors[id] = p
	return p
}

// known answers id without a store read: one of the open sessions, or an
// earlier pass's settled read.
func (r *nudgeOrphanSeatResolver) known(id string) (nudgeOrphanPredecessor, bool) {
	for _, info := range r.open {
		if info.ID == id {
			return nudgeOrphanPredecessor{info: info, open: true}, true
		}
	}
	p, ok := r.carried[id]
	return p, ok
}

// read reads id through lookup, spending one read of a positive budget. A nil
// lookup reads nothing, so id resolves fenced-session-unknown, unsettled.
func (r *nudgeOrphanSeatResolver) read(id string) nudgeOrphanPredecessor {
	if r.lookup == nil {
		return nudgeOrphanPredecessor{resolution: nudgeOrphanFencedSessionUnknown}
	}
	if r.budget > 0 {
		r.budget--
	}
	info, err := r.lookup(id)
	switch {
	case err == nil:
		return nudgeOrphanPredecessor{info: info, settled: info.Closed}
	case errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessionpkg.ErrSessionNotFound):
		return nudgeOrphanPredecessor{resolution: nudgeOrphanFencedSessionUnknown, settled: true}
	default:
		return nudgeOrphanPredecessor{
			resolution: nudgeOrphanLookupError,
			err:        fmt.Errorf("reading fenced session %q of an orphaned nudge: %w", id, err),
		}
	}
}

// resolve resolves item through its session fence to the predecessor's pool
// seat. An unfenced item and a startup turn (bound to its own launch, never
// moved) resolve without a read. An item fenced to an open session that still
// holds its fence resolves to that session: a claim that adopted the item
// under an inherited key re-fenced it there, and that session's claim takes an
// item fenced to it as its own (Leg A).
func (r *nudgeOrphanSeatResolver) resolve(item queuedNudge) nudgeOrphanSeat {
	fence := strings.TrimSpace(item.SessionID)
	if fence == "" {
		return nudgeOrphanSeat{Resolution: nudgeOrphanUnfenced}
	}
	if nudgequeue.IsStartupTurn(item) {
		return nudgeOrphanSeat{Resolution: nudgeOrphanStartupTurn}
	}
	p := r.predecessor(fence)
	if p.resolution != "" {
		return nudgeOrphanSeat{Resolution: p.resolution}
	}
	pred := p.info
	if sessionHoldsNudgeFence(pred) {
		if p.open {
			return nudgeOrphanSeat{Resolution: nudgeOrphanSuccessor, Successor: pred}
		}
		return nudgeOrphanSeat{Resolution: nudgeOrphanFencedSessionLive}
	}
	if !isPoolManagedSessionInfo(pred) || strings.TrimSpace(pred.AgentName) == "" {
		return nudgeOrphanSeat{Resolution: nudgeOrphanNotPoolSeat}
	}
	var holders []sessionpkg.Info
	for _, info := range r.open {
		if info.ID != pred.ID && sessionHoldsNudgeFence(info) && isPoolManagedSessionInfo(info) && sameNudgePoolSeat(pred, info) {
			holders = append(holders, info)
		}
	}
	switch len(holders) {
	case 0:
		return nudgeOrphanSeat{Resolution: nudgeOrphanSeatVacant}
	case 1:
		return nudgeOrphanSeat{Resolution: nudgeOrphanSuccessor, Successor: holders[0]}
	default:
		return nudgeOrphanSeat{Resolution: nudgeOrphanSeatAmbiguous}
	}
}

// sameNudgePoolSeat reports whether a and b hold the same pool seat: the same
// template, agent name and pool slot. The slot keeps a seat whose agent name
// fell back to the template from matching every slot of the pool.
func sameNudgePoolSeat(a, b sessionpkg.Info) bool {
	return strings.TrimSpace(a.AgentName) == strings.TrimSpace(b.AgentName) &&
		strings.TrimSpace(a.Template) == strings.TrimSpace(b.Template) &&
		strings.TrimSpace(a.PoolSlot) == strings.TrimSpace(b.PoolSlot)
}

// nudgeOrphanKey is one orphaned key's decision for a dispatch pass.
type nudgeOrphanKey struct {
	Key string
	// Items is the number of due items under Key.
	Items int
	// Fence is the session fence of Key's newest fenced due item, if any.
	Fence      string
	Resolution nudgeOrphanResolution
	// Successor is the session the pass hands Key to, set when Resolution
	// is nudgeOrphanSuccessor.
	Successor sessionpkg.Info
}

// dueNudgeOrphanItems returns state's items under keys that DueKeys counts
// as due at now (a due pending item not held behind a startup turn, or an
// in-flight item whose lease lapsed), grouped by key.
func dueNudgeOrphanItems(state nudgequeue.State, keys []string, now time.Time) map[string][]queuedNudge {
	want := make(map[string]bool, len(keys))
	for _, key := range keys {
		want[key] = true
	}
	var barrier nudgequeue.StartupBarrier
	for _, item := range state.Pending {
		barrier.Add(item, false, now)
	}
	for _, item := range state.InFlight {
		barrier.Add(item, true, now)
	}
	due := make(map[string][]queuedNudge)
	for _, item := range state.Pending {
		if !want[item.Agent] || (!item.DeliverAfter.IsZero() && item.DeliverAfter.After(now)) || barrier.Holds(item) {
			continue
		}
		due[item.Agent] = append(due[item.Agent], item)
	}
	for _, item := range state.InFlight {
		if !want[item.Agent] || item.LeaseUntil.IsZero() || !item.LeaseUntil.Before(now) || barrier.Holds(item) {
			continue
		}
		due[item.Agent] = append(due[item.Agent], item)
	}
	return due
}

// decideNudgeOrphanKeys decides each orphaned key from its due items. Items
// resolve newest first, so a lookup budget goes to the newest. A key is handed
// to a successor only when at least one fenced, non-startup item under it
// resolves, every such item resolves to the same successor, and the key
// carries no unfenced startup turn. The returned error joins the lookup
// failures.
func decideNudgeOrphanKeys(r *nudgeOrphanSeatResolver, keys []string, due map[string][]queuedNudge) ([]nudgeOrphanKey, error) {
	var all []queuedNudge
	for _, key := range keys {
		all = append(all, due[key]...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	seats := make(map[string]nudgeOrphanSeat, len(all))
	for _, item := range all {
		if strings.TrimSpace(item.SessionID) != "" && !nudgequeue.IsStartupTurn(item) {
			seats[item.ID] = r.resolve(item)
		}
	}

	var errs []error
	reported := make(map[string]bool)
	decided := make([]nudgeOrphanKey, 0, len(keys))
	for _, key := range keys {
		items := append([]queuedNudge(nil), due[key]...)
		sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
		k := nudgeOrphanKey{Key: key, Items: len(items)}
		var fenced []nudgeOrphanSeat
		startup, unfencedStartup := false, false
		for _, item := range items {
			fence := strings.TrimSpace(item.SessionID)
			if nudgequeue.IsStartupTurn(item) {
				startup = true
				unfencedStartup = unfencedStartup || fence == ""
				continue
			}
			if fence == "" {
				continue
			}
			if k.Fence == "" {
				k.Fence = fence
			}
			fenced = append(fenced, seats[item.ID])
			if p := r.predecessors[fence]; p.err != nil && !reported[fence] {
				reported[fence] = true
				errs = append(errs, p.err)
			}
		}
		switch {
		case unfencedStartup:
			k.Resolution = nudgeOrphanStartupTurn
		case len(fenced) == 0 && startup:
			k.Resolution = nudgeOrphanStartupTurn
		case len(fenced) == 0:
			k.Resolution = nudgeOrphanUnfenced
		default:
			k.Resolution, k.Successor = nudgeOrphanKeySeat(fenced)
		}
		decided = append(decided, k)
	}
	return decided, errors.Join(errs...)
}

// nudgeOrphanKeySeat combines a key's fenced item seats, newest first: the
// first that does not resolve to a successor names the key's resolution, and
// successors that differ make it ambiguous.
func nudgeOrphanKeySeat(seats []nudgeOrphanSeat) (nudgeOrphanResolution, sessionpkg.Info) {
	for _, seat := range seats {
		if seat.Resolution != nudgeOrphanSuccessor {
			return seat.Resolution, sessionpkg.Info{}
		}
	}
	successor := seats[0].Successor
	for _, seat := range seats[1:] {
		if seat.Successor.ID != successor.ID {
			return nudgeOrphanSeatAmbiguous, sessionpkg.Info{}
		}
	}
	return nudgeOrphanSuccessor, successor
}

// nudgeOrphanPredecessorMemo carries a dispatcher's settled fenced-session
// reads (nudgeOrphanPredecessor.settled) from one pass to the next, so a fence
// that stays due is read once rather than once per pass, and each pass's lookup
// budget goes to fences no pass has read yet instead of the same newest ones.
// A pass keeps only the entries its due orphaned items named; a pass with no
// orphaned key forgets them all. The zero value is empty and ready to use; a
// nil memo carries nothing.
type nudgeOrphanPredecessorMemo struct {
	mu   sync.Mutex
	rows map[string]nudgeOrphanPredecessor
}

// carried returns a copy of the memo's entries, by fence ID.
func (m *nudgeOrphanPredecessorMemo) carried() map[string]nudgeOrphanPredecessor {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.rows)
}

// keep replaces the memo's entries with the settled reads among read, a
// pass's reads by fence ID.
func (m *nudgeOrphanPredecessorMemo) keep(read map[string]nudgeOrphanPredecessor) {
	if m == nil {
		return
	}
	rows := make(map[string]nudgeOrphanPredecessor, len(read))
	for id, p := range read {
		if p.settled {
			rows[id] = p
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = rows
}

// forget drops every entry.
func (m *nudgeOrphanPredecessorMemo) forget() {
	m.keep(nil)
}

// resolveNudgeOrphanKeysForPass decides a dispatch pass's orphaned keys: one
// whole-queue Snapshot read, then the seat resolution of each key's due items
// against the pass's open sessions, within the pass's lookup budget, starting
// from memo's settled reads and leaving it the settled reads of this pass. A
// failed Snapshot leaves every key orphaned as a lookup error and memo as it
// was, and is returned.
func resolveNudgeOrphanKeysForPass(queue nudgeQueueHandle, keys []string, open []sessionpkg.Info, lookup func(id string) (sessionpkg.Info, error), memo *nudgeOrphanPredecessorMemo, now time.Time) ([]nudgeOrphanKey, error) {
	state, err := queue.Queue.Snapshot()
	if err != nil {
		decided := make([]nudgeOrphanKey, len(keys))
		for i, key := range keys {
			decided[i] = nudgeOrphanKey{Key: key, Resolution: nudgeOrphanLookupError}
		}
		return decided, fmt.Errorf("reading the nudge queue to resolve orphaned keys: %w", err)
	}
	r := newNudgeOrphanSeatResolver(open, lookup, nudgeOrphanPredecessorLookupsPerPass)
	r.carried = memo.carried()
	decided, err := decideNudgeOrphanKeys(r, keys, dueNudgeOrphanItems(state, keys, now))
	memo.keep(r.predecessors)
	return decided, err
}

// adoptNudgeOrphanKeys decides the due keys of a dispatch pass that none of
// targets matches, so no branch of the pass would reach them. One whose fenced
// items resolve to a single open session is handed to that session's target
// for this pass, whose claim's own live census still decides whether they
// move; every other one is counted orphaned in skipCounts, once per pass.
// lookup reads a fenced session the open sessions do not list; nil reads
// none. A pass whose every due key is matched reads nothing more and forgets
// memo. It returns the decision's read error.
func adoptNudgeOrphanKeys(queue nudgeQueueHandle, dueKeys []string, open []sessionpkg.Info, targets []nudgeTarget, lookup func(id string) (sessionpkg.Info, error), memo *nudgeOrphanPredecessorMemo, now time.Time, skipCounts map[string]int64, debugOut io.Writer) error {
	orphanKeys := unmatchedNudgeQueueKeys(dueKeys, targets)
	if len(orphanKeys) == 0 {
		memo.forget()
		return nil
	}
	decided, err := resolveNudgeOrphanKeysForPass(queue, orphanKeys, open, lookup, memo, now)
	for _, k := range decided {
		if k.Resolution == nudgeOrphanSuccessor {
			if successor, ok := inheritNudgeOrphanKey(targets, k); ok {
				logRoute(debugOut, "nudge-dispatch-tick", "adopt", "orphaned-seat", "agent", k.Key, "session", successor)
				continue
			}
		}
		skipCounts["orphaned"]++
		logNudgeDispatchSkip(debugOut, "orphaned", k.Key, k.Fence, fmt.Sprintf("resolution=%s items=%d", k.Resolution, k.Items))
	}
	return err
}

// inheritNudgeOrphanKey hands k's key to the target of k's successor for the
// pass and returns that target's session name. It reports false when that
// session has no deliverable target (the pass skips it as no-target), which
// leaves the key orphaned.
func inheritNudgeOrphanKey(targets []nudgeTarget, k nudgeOrphanKey) (string, bool) {
	for i := range targets {
		if targets[i].sessionID == k.Successor.ID && targets[i].sessionName != "" {
			targets[i].inheritedKeys = append(targets[i].inheritedKeys, k.Key)
			return targets[i].sessionName, true
		}
	}
	return "", false
}
