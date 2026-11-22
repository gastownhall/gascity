package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
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
// census) decides whether the items move. Every other orphaned key is counted
// and named with a nudgeOrphanResolution, and gc nudge status --orphaned lists
// its items.

// nudgeOrphanResolution says whether an orphaned key's items resolve to a
// successor session holding their predecessor's pool seat, or why not.
type nudgeOrphanResolution string

const (
	// nudgeOrphanSuccessor: exactly one open session holds the seat.
	nudgeOrphanSuccessor nudgeOrphanResolution = "successor"
	// nudgeOrphanSeatVacant: no open session holds the predecessor's seat.
	nudgeOrphanSeatVacant nudgeOrphanResolution = "seat-vacant"
	// nudgeOrphanSeatAmbiguous: several open sessions hold the seat, or the
	// key's items resolve to different successors.
	nudgeOrphanSeatAmbiguous nudgeOrphanResolution = "seat-ambiguous"
	// nudgeOrphanNotPoolSeat: the predecessor is not a pool-managed session
	// with an agent name, so it names no seat.
	nudgeOrphanNotPoolSeat nudgeOrphanResolution = "not-pool-seat"
	// nudgeOrphanFencedSessionLive: the fenced session still holds its fence.
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
// rows). Lookups go to the newest due items first; the rest wait a pass.
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
}

// nudgeOrphanSeatResolver resolves orphaned items against the open sessions
// of one read, reading a fenced session the open list does not hold through
// lookup at most budget times (a negative budget is unlimited), once per ID.
type nudgeOrphanSeatResolver struct {
	open         []sessionpkg.Info
	lookup       func(id string) (sessionpkg.Info, error)
	budget       int
	predecessors map[string]nudgeOrphanPredecessor
}

// newNudgeOrphanSeatResolver resolves against open, reading other sessions
// through lookup. A nil lookup reads nothing, so a fenced session open does
// not list resolves fenced-session-unknown.
func newNudgeOrphanSeatResolver(open []sessionpkg.Info, lookup func(id string) (sessionpkg.Info, error), budget int) *nudgeOrphanSeatResolver {
	return &nudgeOrphanSeatResolver{open: open, lookup: lookup, budget: budget, predecessors: make(map[string]nudgeOrphanPredecessor)}
}

// predecessor reads the fenced session id: from the open list, or else (the
// open list deliberately omits closed rows) through lookup, within budget.
func (r *nudgeOrphanSeatResolver) predecessor(id string) nudgeOrphanPredecessor {
	if p, ok := r.predecessors[id]; ok {
		return p
	}
	var p nudgeOrphanPredecessor
	found := false
	for _, info := range r.open {
		if info.ID == id {
			p.info, found = info, true
			break
		}
	}
	switch {
	case found:
	case r.lookup == nil:
		p.resolution = nudgeOrphanFencedSessionUnknown
	case r.budget == 0:
		// Not cached: a resolver with no budget left defers every new ID.
		return nudgeOrphanPredecessor{resolution: nudgeOrphanLookupDeferred}
	default:
		if r.budget > 0 {
			r.budget--
		}
		info, err := r.lookup(id)
		switch {
		case err == nil:
			p.info = info
		case errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessionpkg.ErrSessionNotFound):
			p.resolution = nudgeOrphanFencedSessionUnknown
		default:
			p.resolution = nudgeOrphanLookupError
			p.err = fmt.Errorf("reading fenced session %q of an orphaned nudge: %w", id, err)
		}
	}
	r.predecessors[id] = p
	return p
}

// resolve resolves item through its session fence to the predecessor's pool
// seat. An unfenced item and a startup turn (bound to its own launch, never
// moved) resolve without a read.
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

// resolveNudgeOrphanKeysForPass decides a dispatch pass's orphaned keys: one
// whole-queue Snapshot read, then the seat resolution of each key's due items
// against the pass's open sessions, within the pass's lookup budget. A failed
// Snapshot leaves every key orphaned as a lookup error and is returned.
func resolveNudgeOrphanKeysForPass(queue nudgeQueueHandle, keys []string, open []sessionpkg.Info, lookup func(id string) (sessionpkg.Info, error), now time.Time) ([]nudgeOrphanKey, error) {
	state, err := queue.Queue.Snapshot()
	if err != nil {
		decided := make([]nudgeOrphanKey, len(keys))
		for i, key := range keys {
			decided[i] = nudgeOrphanKey{Key: key, Resolution: nudgeOrphanLookupError}
		}
		return decided, fmt.Errorf("reading the nudge queue to resolve orphaned keys: %w", err)
	}
	r := newNudgeOrphanSeatResolver(open, lookup, nudgeOrphanPredecessorLookupsPerPass)
	return decideNudgeOrphanKeys(r, keys, dueNudgeOrphanItems(state, keys, now))
}
