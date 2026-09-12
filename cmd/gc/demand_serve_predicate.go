package main

// The agreement predicate: would a T-worker's own query serve this row?
//
// The pool is a PULL system. The controller does not choose which bead a seat
// picks up; it scales capacity on evidence that work exists, and the worker
// discovers and claims for itself. That only converges if the two readers agree
// about what counts as work for a template — and they did not.
//
// The controller's demand loop counted any ready, unassigned row whose route
// normalized to T. The worker's Tier-3 query serves a strictly smaller set: it
// passes --exclude-type=epic and one --exclude-label per dispatch hold, and it
// matches the route by EXACT string. So three classes of row were permanent
// capacity demand that no worker could ever claim, and a fourth joined them
// when the hook began hiding it:
//
//   - a routed EPIC (an unassigned parent has no executable spec — the query
//     excludes it deliberately, workquery.go),
//   - a bead parked on hold:mayor / hold:external (parked precisely because the
//     next actor is not this worker),
//   - a route stamped with a live slot suffix, "<base>-N", which the demand side
//     normalizes to <base> and every raw consumer rejects,
//   - a step of a workflow whose instantiation aborted partway (molecule_failed,
//     ga-033u0e): a v1 step keeps the gc.routed_to it was created with, so the
//     reader returns it and the hook strips it.
//
// Each one spawns a seat, the seat's hook reads empty, it drains, and the
// controller counts the row again on the next tick. Forever.
//
// This file is the one predicate both sides now answer with. The serving rules
// come from internal/config — the same value the shell flags are rendered from —
// so a flag the query gains cannot silently fail to reach the controller. The
// route half mirrors hookClaimMatchesRoute exactly, because that is the function
// that will actually accept or reject the claim.

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// demandServableForTemplates reports the template a row is capacity demand for,
// among the templates a store group is counting, or ok=false when no worker for
// any of them would be served it.
//
// The route candidate is normalized (agentutil.NormalizePoolRouteTarget) only to
// bridge the intra-tick window: the same-tick canonicalize pass persists the
// collapse before this runs, so what is counted here is servable in the store
// THIS tick. Without the collapse, counting only exact forms would leave a "-N"
// row counted by neither side — invisible dead work, the dead-drop
// NormalizePoolRouteTarget exists to close.
func demandServableForTemplates(cfg *config.City, b beads.Bead, templates map[string]struct{}) (string, bool) {
	if !demandRowServable(b) {
		return "", false
	}
	for _, candidate := range controllerDemandRouteCandidates(b) {
		normalized := agentutil.NormalizePoolRouteTarget(cfg, candidate)
		if _, ok := templates[normalized]; ok {
			return normalized, true
		}
	}
	return "", false
}

// demandRowServable applies the route-independent half of the Tier-3 serving
// rules to one row: the exclusions a worker's query enforces regardless of which
// template it is asking for.
func demandRowServable(b beads.Bead) bool {
	rules := config.PoolDemandServeRulesForQuery()
	if rules.RequireUnassigned && strings.TrimSpace(b.Assignee) != "" {
		return false
	}
	// Each dimension matches whichever filter has the LAST WORD on what the
	// worker is served. They are not the same comparison, and pretending they
	// were is how the controller ends up counting a row no worker will take (or
	// refusing to count one that every worker would).
	//
	// TYPE: exact, case-sensitive. The serving filter behind `gc ready` compares
	// with a Go map lookup on the raw type (filterReadyBeads), bd's own
	// --exclude-type lands in SQL as `issue_type NOT IN (?)` over a value that is
	// alias-expanded but NOT case-folded, and no hook-side post-filter looks at
	// type at all. So a bead typed "Epic" IS served, and counting it is agreement
	// — declining to count it would be the controller inventing an exclusion the
	// query does not have.
	beadType := strings.TrimSpace(b.Type)
	for _, excluded := range rules.ExcludeTypes {
		if beadType == excluded {
			return false
		}
	}
	// LABEL: case-insensitive. Here the query is NOT the last word: whatever
	// `gc ready` returns, the hook re-applies the hold filter in Go with
	// EqualFold (isHeldHookCandidate) before serving a candidate, so a
	// "Hold:Mayor" bead is served by the reader and then stripped by the hook.
	// The worker never sees it, so it is not capacity demand.
	for _, label := range b.Labels {
		label = strings.TrimSpace(label)
		for _, excluded := range rules.ExcludeLabels {
			if strings.EqualFold(label, excluded) {
				return false
			}
		}
	}
	// PARTIAL WORKFLOW: exact. A pour that aborts partway marks every bead it
	// already wrote molecule_failed=true, and a non-graph (v1) recipe is never
	// fenced, so its steps still carry the gc.routed_to they were created with
	// and the reader serves them. The hook then strips them
	// (isFailedPartialMoleculeHookCandidate), so no worker ever holds one: a
	// row counted here is a seat that spawns, reads empty, drains and is
	// counted again next tick, forever. Same rule as the hook and the
	// reconciler's count-form — beadmeta.MoleculeFailed — so the three cannot
	// disagree by spelling (ga-033u0e).
	if beadmeta.MoleculeFailed(b.Metadata[beadmeta.MoleculeFailedMetadataKey]) {
		return false
	}
	return true
}

// demandRowClaimability names whether a trigger row a demand-spawned seat failed
// to claim is still claimable by a worker right now — and, when it is not, WHY.
// A worker's Tier-3 ready query excludes both a blocked and a deferred row, so a
// routed-but-blocked row that drained a seat is correct pull (nothing was
// claimable), NOT a demand/claim divergence — the dominant false-positive class
// the divergence classifier was over-counting.
//
// The reason is the answer, not a bare "not ready", because the two consumers —
// classifyDemandTrigger and the drain-ack open arm
// (firstOpenClaimableAssignedWorkBeadInStoreByIdentifiers) — act differently on
// each cause: a deferral is PROOF of non-claimability, while an unproven
// blockedness reading is only a question, to be settled against live deps. Naming
// the cause here keeps that distinction in one place. Reconstructing it by
// elimination at a call site ("the only remaining reason is …") would be valid
// only while this function has exactly these clauses, and would break silently
// the moment a third exclusion is added.
type demandRowClaimability string

const (
	// demandRowClaimable: nothing bead-local keeps a worker from claiming the row.
	// Reached only on POSITIVE evidence of unblockedness — an is_blocked
	// projection that reads explicitly false, which is bd's own answer, computed
	// through the ready-projection enrichment's `bd sql`/`bd blocked` door.
	demandRowClaimable demandRowClaimability = "claimable"
	// demandRowDeferred: defer_until, or bd's indefinite deferral, gates the row
	// (beads.IsDeferred). Both are FRESH bead-local fields — never stale — so this
	// is proof that no worker could have claimed the row.
	demandRowDeferred demandRowClaimability = "deferred"
	// demandRowBlockednessUnproven: the bead alone cannot settle whether the row is
	// blocked, so each consumer re-derives it from live dependencies
	// (beadHasUnmetPlainBlocksDep). Two readings land here, and neither is proof:
	//
	//   - is_blocked reads TRUE. bd's projection is DENORMALIZED and can read
	//     stale-true after a blocker closes (issueops.countStaleIsBlockedSQL /
	//     `bd recompute-blocked` exist precisely to repair it), so acting on it
	//     alone would bury a row whose blocking deps are in fact met.
	//   - is_blocked is ABSENT. That is the production reading (see
	//     classifyDemandRowClaimability) and it is no evidence at all.
	//
	// Collapsing both into one cause is not elimination-by-default: every consumer
	// switches on this case explicitly, and both readings get the same treatment
	// because neither carries information the live-dep derivation does not.
	demandRowBlockednessUnproven demandRowClaimability = "blockedness_unproven"
)

// classifyDemandRowClaimability answers the claimability question for one row.
// Deferral is checked first: it is the cause that is proof, so a row that is both
// deferred and flagged blocked classifies as deferred.
//
// An ABSENT is_blocked projection is not evidence of unblockedness, and this
// predicate does not read it as any. bd's `list --json` / `show --json` payloads
// do not carry the column, so BdStore.toBead leaves IsBlocked nil on every read;
// beadFromNativeIssue cannot set it either. The projection is supplied only by
// the CachingStore's ready-projection enrichment (enrichReadyProjectionForCache,
// which asks bd through a separate `bd sql`/`bd blocked` door at cache-prime
// time), so it is visible only on non-live cached reads — and both of this
// predicate's callers read off that path: the drain-ack finders force live=true
// and the divergence consumer uses a fresh BdStore.Get. Absence is therefore what
// production actually hands this function, and classifying it as
// demandRowBlockednessUnproven is what keeps the blocked half reachable on every
// store class instead of inert.
//
// Reporting "unproven" is deliberately NOT the answer the ready query's
// dependency-derived fallback gives — cachedBeadReady's nil branch re-derives
// from the row's own direct blocking edges and CAN find the row blocked. This
// single-bead predicate holds no deps, so it names what it does not know and
// leaves the derivation to the consumer that can make it.
//
// Both callers pass a row already constrained to status=="open", so a raw
// "blocked" status string never reaches here.
func classifyDemandRowClaimability(b beads.Bead, now time.Time) demandRowClaimability {
	if beads.IsDeferred(b, now) {
		return demandRowDeferred
	}
	if b.IsBlocked == nil || *b.IsBlocked {
		return demandRowBlockednessUnproven
	}
	return demandRowClaimable
}

// beadHasUnmetPlainBlocksDep re-derives whether a row is really blocked, from its
// live dependencies rather than bd's denormalized is_blocked projection. It is
// the shared settlement both consumers of classifyDemandRowClaimability run on a
// demandRowBlockednessUnproven row — the drain-ack open arm before suppressing a
// possible strand, classifyDemandTrigger before bucketing a possible divergence —
// so the verdict no longer depends on whether the store class happens to carry
// the projection at all.
//
// It confirms on a plain `blocks` edge ONLY — narrower than the
// DepList → IsReadyBlockingDependencyType → DependencySatisfied derivation used
// for blocked_by enrichment, and deliberately so. Target-not-closed is not proof
// of non-claimability for the other ready-blocking types: a `waits-for` gate
// opens through bd-native state independently of its target's own status
// (internal/formula/compile.go mints exactly that shape, a gate edge onto a step
// that stays open; native_dolt_store.go's ready filter states it directly — "a
// waits-for edge gates on the spawner's children rather than the spawner's own
// status" — and declines the full predicate for the same reason), bdstore.go's
// inline-dep derivation records that the flat DependencySatisfied rule "is
// deliberately NOT reused" for a row bd has already offered, and the
// `bd-gate-open` fixture pins the counterexample: a waits-for edge onto an OPEN
// target reads is_blocked = 0, bd's ready OFFERS the row, and the direct-dep
// predicate would hide it. Reusing that predicate here — in the one situation
// where the row's blockedness is already unsettled — would second-guess a bd
// verdict and silence a claimable row, the exact false negative this settlement
// exists to prevent. An unmet `waits-for` / `conditional-blocks` target therefore
// reads as NOT blocked.
//
// Residual this narrowing does NOT close: a plain `blocks` edge onto a PINNED
// blocker is satisfied natively by bd (native_dolt_store.go, same comment), while
// beads.Dep carries no pin visibility — so that row still confirms as blocked
// even though bd would offer it. Closing it needs the blocker's pin state on the
// dependency read, not a wider edge-type set.
//
// A dep or blocker read that FAILS is returned, not swallowed, and each consumer
// fails closed on it: the drain-ack classifier surfaces the error to be logged
// and never manufactures the alarm from an unreadable store, and the divergence
// classifier reports unknown rather than inventing the metric it exists to keep
// honest.
func beadHasUnmetPlainBlocksDep(store beads.Store, id string) (bool, error) {
	deps, err := store.DepList(id, "down")
	if err != nil {
		return false, err
	}
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		blocker, err := store.Get(dep.DependsOnID)
		if err != nil {
			return false, err
		}
		if !beads.DependencySatisfied(blocker.Status, blocker.Metadata[beadmeta.WorkOutcomeMetadataKey]) {
			return true, nil
		}
	}
	return false, nil
}

// routeCollapseRewriteTarget returns the canonical base route a slot-suffixed
// route should be persisted as, or "" when the route is already canonical or is
// not a collapsible slot form.
//
// NormalizePoolRouteTarget owns the decision — base names, non-pool agents,
// unknown agents, non-numeric and out-of-range suffixes are all returned
// unchanged and therefore left alone here. This wrapper adds only the cheap
// pre-filter that keeps the steady-state backlog scan off the per-bead agent
// walk: a collapsible route's local segment ends in "-<digits>".
func routeCollapseRewriteTarget(cfg *config.City, routedTo string) string {
	routedTo = strings.TrimSpace(routedTo)
	if routedTo == "" || !routeHasSlotSuffixShape(routedTo) {
		return ""
	}
	collapsed := agentutil.NormalizePoolRouteTarget(cfg, routedTo)
	if collapsed == "" || collapsed == routedTo {
		return ""
	}
	return collapsed
}

// routeHasSlotSuffixShape reports whether a route's local segment ends in a
// "-<digits>" slot suffix. Pure shape, no config: it is the pre-filter, not the
// decision.
func routeHasSlotSuffixShape(routedTo string) bool {
	_, local := config.ParseQualifiedName(routedTo)
	idx := strings.LastIndexByte(local, '-')
	if idx < 0 || idx == len(local)-1 {
		return false
	}
	for _, r := range local[idx+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// collapseSlotSuffixedRoutedWork persists the base route for open, unassigned
// rows routed at a live slot suffix, alongside the legacy-bound canonicalization
// that already runs in this pass.
//
// Why persist rather than only normalize on read: the readers that reject "-N"
// are shell, jq and bd flags (the generated query's --metadata-field, the
// claim's raw string compare) and cannot call into Go. One write fixes every raw
// consumer at once — and the alternative, counting only exact forms, would leave
// the row claimable by nobody and visible to nobody.
//
// Idempotent by construction: it writes only when the collapse differs from what
// is persisted, so steady state performs no writes. Same error discipline as its
// sibling — a write failure is logged and skipped, never blocking reconciliation.
func collapseSlotSuffixedRoutedWork(cfg *config.City, workBeads []beads.Bead, workStores []beads.Store, stderr io.Writer) {
	if cfg == nil || len(workBeads) != len(workStores) {
		return
	}
	for i, wb := range workBeads {
		if wb.Status != "open" || strings.TrimSpace(wb.Assignee) != "" {
			continue
		}
		store := workStores[i]
		if store == nil {
			continue
		}
		collapsed := routeCollapseRewriteTarget(cfg, wb.Metadata[beadmeta.RoutedToMetadataKey])
		if collapsed == "" {
			continue
		}
		opts := beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: collapsed}}
		if err := store.Update(wb.ID, opts); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "collapseSlotSuffixedRoutedWork: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
}
