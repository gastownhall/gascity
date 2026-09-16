package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// poolWakeReadiness answers, per store, the question the SERVE side answers for
// an orphaned assigned row: would the query a freshly woken seat runs actually
// be served this bead?
//
// The wake-known-identity tier mints a seat for assigned work whose claimant
// session is gone. That seat's own reader for an open row is the assigned-ready
// tier — `bd ready --assignee="$id"` (internal/config/workquery.go,
// assignedReadyTierCommand) — so a row bd ready refuses is a row the seat can
// never claim. The controller cannot run that query per row, and Bead.Status
// cannot stand in for it: mapBdStatus collapses bd's blocked, deferred, review
// and testing statuses onto "open", so the status gate in
// computePoolDesiredStatesAt passes every one of them and the row is capacity
// demand forever.
//
// Counting rows the serve path is structurally forbidden to hand out is the
// protocol mismatch config.PoolDemandServeRules was introduced to close for the
// UNASSIGNED arm. This is that same correspondence for the assigned arm, read
// off the Ready() snapshot the demand phase already takes once per store
// (readyDemandCache) rather than a second hand-written opinion about what
// "ready" means.
type poolWakeReadiness struct {
	// ready holds the store-scoped keys the ready frontier returned, keyed by
	// the same census store refs the candidates carry.
	ready map[storeScopedBeadKey]bool
	// verified names the store refs whose ready frontier was actually read.
	// Anything else fails open: a store that told us nothing must not suppress
	// its pool's demand, because a hiccup that reads as "no ready work" is how
	// a live pool gets drained.
	verified map[string]bool
}

// isPoolWakeCandidate reports whether a census row could only ever become pool
// demand through the wake tier, and therefore needs the serve side's verdict.
// Open plus an assignee is exactly the orphan-release pass's catch
// (appendOpenRoutedWorkUnique) and the open assigned molecule roots beside it;
// in-progress rows are excluded because their serve tier is the crash-recovery
// list read, not bd ready.
func isPoolWakeCandidate(b beads.Bead) bool {
	return b.Status == "open" && strings.TrimSpace(b.Assignee) != ""
}

// servesWakeCandidate reports whether the ready frontier returned this row.
// Every uncertain case answers true — a nil receiver (the caller computed no
// verdict), a store that was never read, or one whose read errored — because
// over-counting demand costs one idle seat while under-counting strands claimed
// work and drains the pool holding it.
func (r *poolWakeReadiness) servesWakeCandidate(storeRef, id string) bool {
	if r == nil || !r.verified[storeRef] {
		return true
	}
	return r.ready[storeScopedBeadKey{StoreRef: storeRef, ID: id}]
}

// vetoesWakeCandidate reports whether this verdict positively disqualifies a row
// from the wake tier. Absence from the ready frontier is evidence only where
// the frontier was asked the same question the serving seat will ask, so every
// row the verdict cannot speak for answers false and keeps the demand it had
// before this gate existed:
//
//   - a live claimant's row belongs to the RESUME tier, whose job is to keep
//     that session alive whether or not the work is ready this instant;
//   - a non-open row is served by the crash-recovery list read, not bd ready;
//   - a row whose type or labels Ready() structurally EXCLUDES was never a
//     candidate, so its absence means "not eligible for this query", not
//     "blocked". An assigned molecule/wisp root is exactly that case — the
//     census admits it as genuine demand on purpose because "an assigned
//     root-only wisp is the executable turn"
//     (appendOpenAssignedMoleculeWorkUnique), and the legacy assigned-ready
//     probe serves it through `bd query` with a selector that excludes epics
//     and nothing else (config.ephemeralAssignedReadyProbeScript);
//   - a target template with its own work_query has replaced the default
//     assigned-ready tier verbatim (config.Agent.effectiveQuery), so the
//     predicate this gate mirrors is not the one that seat will run.
//
// The one thing it does NOT read off the frontier is a deferral, because there
// the frontier is wrong: see the comment on that branch below.
func (r *poolWakeReadiness) vetoesWakeCandidate(b beads.Bead, agentCfg *config.Agent, claimantLive bool, storeRef string) bool {
	// No verdict, or none for this row's own store: the gate is not armed here
	// and every row keeps the demand it had.
	if r == nil || !r.verified[storeRef] {
		return false
	}
	if claimantLive || !isPoolWakeCandidate(b) {
		return false
	}
	if agentCfg != nil && strings.TrimSpace(agentCfg.WorkQuery) != "" {
		return false
	}
	// A deferral is POSITIVE evidence, not an absence, so it is read off the
	// bead rather than off the frontier. It has to be, because the frontier
	// disagrees with bd here: mapBdStatus has already spelled a deferred row
	// "open" by the time it reaches this function, IsDeferred goes false the
	// moment defer_until elapses, and both the cached tier and
	// NativeDoltStore.Ready then hand the row back as ready — while `bd ready
	// --assignee=<id>`, the query the woken seat actually runs, refuses it at
	// any timestamp (beads.CarriesDeferral records that measurement). This is
	// the state gastownhall/gascity#6207 reported.
	if beads.CarriesDeferral(b) {
		return true
	}
	if beads.IsReadyExcludedBead(b) {
		return false
	}
	return !r.servesWakeCandidate(storeRef, b.ID)
}

// newPoolWakeReadiness reads the ready frontier once per store that carries a
// wake candidate, through the per-pass ready cache the demand probes already
// share (readyDemandCache). A leg the scale-check probes already read is served
// from that cache for free; a rig or class leg they never touched costs exactly
// one Ready read, once per pass. A nil cache reads directly, matching every
// other optional-cache consumer here.
//
// The verdict is a SNAPSHOT taken in the demand phase, so a blocker that closes
// after it is taken is seen on the next tick, not this one. That is the same
// one-tick latency every other demand read on this path carries, and it errs
// toward one late spawn rather than one wrong drain.
//
// work, stores and storeRefs are the index-aligned census snapshot. A snapshot
// that is not aligned carries no usable provenance, so it yields no verdict
// rather than a guess.
func newPoolWakeReadiness(cache *readyDemandCache, work []beads.Bead, stores []beads.Store, storeRefs []string, stderr io.Writer) *poolWakeReadiness {
	if len(work) == 0 || len(stores) != len(work) || len(storeRefs) != len(work) {
		return nil
	}
	r := &poolWakeReadiness{
		ready:    make(map[storeScopedBeadKey]bool),
		verified: make(map[string]bool),
	}
	seen := make(map[string]bool, len(storeRefs))
	for i, wb := range work {
		if !isPoolWakeCandidate(wb) {
			continue
		}
		// A row with no leg is skipped without consuming the ref, so a later row
		// that does carry one can still verify the store for both of them.
		if stores[i] == nil {
			continue
		}
		ref := storeRefs[i]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		rows, err := cache.controllerDemandReady(stores[i])
		if err != nil {
			// Partial or failed: this store has no verdict to give, so its rows
			// keep the demand they had before this gate existed. Say so on the
			// same channel every other demand probe uses — a gate that has gone
			// inert must not do it silently.
			if stderr != nil {
				fmt.Fprintf(stderr, "poolWakeReadiness: PARTIAL — ready read failed for store %q, wake demand ungated there: %v\n", storeRefLabel(ref), err) //nolint:errcheck
			}
			continue
		}
		r.verified[ref] = true
		for _, row := range rows {
			r.ready[storeScopedBeadKey{StoreRef: ref, ID: row.ID}] = true
		}
	}
	if len(r.verified) == 0 {
		return nil
	}
	return r
}

// storeRefLabel names a census store ref for humans; the city work store is
// carried as the empty ref.
func storeRefLabel(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return "city"
	}
	return ref
}
