package main

// Claim-time write routing by coordination class.
//
// `gc hook --claim` issues every claim-time write through
// beads.NewBdStore(dir, ...) — a bd subprocess rooted in the agent's WORK
// directory (hookClaimBdStoreContext). A relocated coordination class is not a
// bd workspace and cannot be expressed as a hookStore{dir, env} at all, so on a
// split city those writes run against a ledger that cannot see the bead.
//
// The read half closed first: the generated work query now reads through the
// federated `gc ready`, which covers the binding in process (ga-bvdha). That
// made relocated graph work VISIBLE to the worker without making it claimable —
// an assigned graph step was re-served and re-skipped every tick and no worker
// ever ran it. This file is the write half. It adds a CLASS axis to the existing
// hookClaimOps seam, alongside the rig axis in claim_cross_store.go, rather than
// a parallel claim path.
//
// # The order is WORK first, and that is not the by-id order
//
// A claim is a WRITE, so the store is found by PROBE — ask which store holds the
// bead — never by routing unconditionally on an id prefix. `gc storage migrate`
// preserves ids (infra_class_migrate.go), so a relocated bead keeps its
// HQ/rig-era prefix and a prefix route would miss exactly the beads that moved;
// the same reason bdByIDClassDoor.resolve probes residence.
//
// What differs from the by-id door is the ORDER of the probe, and the difference
// is load-bearing on the one id a probe order can disagree about: a CO-RESIDENT
// bead, which is the documented steady state of a migrated city because the
// migration copies and never deletes back.
//
//   - storeref.ClassCandidates and bdByIDClassDoor lead with the class store.
//     Their caller holds only an id.
//   - `gc ready` — the federated reader that produced this claim's candidate —
//     leads with the CITY work store and runs the graph leg LAST, so a
//     co-resident id resolves to the work store's row (#5148/#5158/#5161,
//     ready_federation.go).
//
// The claim must agree with the reader that served it, and the failure if it
// does not is not cosmetic: claiming the class copy while the reader keeps
// answering from the still-open work copy re-serves the same bead every tick,
// which is the treadmill this slice exists to end. So the work store is probed
// FIRST — by running the existing work-scope write and letting it answer — and
// the binding is reached only where that write proves the bead is not there.
// Co-residence therefore keeps the WORK copy, byte-identically to today.
//
// # The escalation signal is the existing one, unwidened
//
// beads.ErrNotFound is the ONLY error that proves the bead is not in the store
// the write ran against (hookClaimBeadIsElsewhere). A write timeout, store
// contention or a controller-socket flap leaves ownership unresolved on a bead
// the session may already own, and must keep failing closed — so those are
// returned unchanged and never retried against a second store. This file
// consumes that predicate; it does not widen it.
//
// A read failure from the binding is likewise an error and never absence:
// reading "the binding could not answer" as "the bead is not there" is the
// root-loss shape this whole lane exists to prevent. The one error that is not a
// fault is the one-shot funnel's standing refusal, handled exactly as
// classRoutedStoreForID handles it.
//
// # A single-store city takes none of this
//
// hookClaimClassRouteForCity gates on graphClassBinding — store identity, the
// same question resolveClassStore asks — and returns nil for a city that
// relocates nothing. classRoutedHookClaimOps then returns the ops value it was
// handed, unwrapped, so every claim-time write is the exact call it is today.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// hookClaimClassRoute is the opened coordination-class front door a claim-time
// write falls back to, plus the per-invocation record of which bead ids the
// binding was PROVED to hold.
//
// The CAS stays the store's. Claims go through the closed contract's Claim —
// the same acquire half `gc bd update --claim` routes through (#5132) — rather
// than a read-then-write here, which would lose the single-winner guarantee.
//
// resident is a memo, not a cache with a lifetime: one `gc hook --claim` is one
// process, the claim path is strictly sequential (claimHookWorkWithRunner loops
// stores, tryHookClaim walks candidates), and the map dies with the invocation.
// Its job is to spare the follow-on writes for a bead the claim already resolved
// a doomed bd subprocess each.
type hookClaimClassRoute struct {
	// class is the raw binding store, needed by the lifecycle projector, which
	// takes a beads.Store rather than the closed contract.
	class beads.Store
	graph storebinding.GraphStore

	resident map[string]bool
}

// newHookClaimClassRoute opens the claim-time class front door over an already
// resolved binding store. Split from hookClaimClassRouteForCity so the routing
// is testable against a store a test controls rather than only against a city on
// disk.
func newHookClaimClassRoute(class beads.Store) (*hookClaimClassRoute, error) {
	graph, err := storebinding.NewBeadsGraphStore(class)
	if err != nil {
		return nil, fmt.Errorf("projecting the claim-time class front door: %w", err)
	}
	return &hookClaimClassRoute{class: class, graph: graph, resident: map[string]bool{}}, nil
}

// hookClaimClassRouteForCity resolves the claim-time class front door for a
// city, or (nil, nil) when the city relocates no coordination class.
//
// The funnel is the same one cityQueryTopology already entered to decide whether
// this invocation's work query federates at all, and it is memoized per city
// (cli_storage_routes.go), so a hook that reaches here opens no second binding.
func hookClaimClassRouteForCity(cityPath string) (*hookClaimClassRoute, error) {
	class, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated {
		return nil, nil
	}
	return newHookClaimClassRoute(class)
}

// knownResident reports whether an earlier probe in THIS invocation already
// proved the binding holds id. It never probes: a false answer means "not proved
// yet", and every caller that gets one still runs its work-scope write first.
func (r *hookClaimClassRoute) knownResident(id string) bool {
	if r == nil {
		return false
	}
	return r.resident[strings.TrimSpace(id)]
}

// holds probes the binding for id and memoizes the answer.
//
// An error is a read that FAILED, never absence. The single exception is the
// one-shot funnel's standing refusal on a WORK-shaped id: it says this city's
// storage configuration cannot be served, which is a fact about the city and
// none about a particular bead, and a refused city still serves work from its
// work ledger — so the caller's own work-store answer stands. An id only the
// binding could own (a reserved class prefix) has nowhere else to live, so for
// that one the refusal is the answer and surfaces.
func (r *hookClaimClassRoute) holds(id string) (bool, error) {
	id = strings.TrimSpace(id)
	if r == nil || id == "" {
		return false, nil
	}
	if known, ok := r.resident[id]; ok {
		return known, nil
	}
	_, err := r.graph.Get(id)
	switch {
	case err == nil:
		r.resident[id] = true
		return true, nil
	case errors.Is(err, beads.ErrNotFound):
		r.resident[id] = false
		return false, nil
	case isStandingStorageRefusal(err) && !bdIDIsClassReserved(id):
		return false, nil
	default:
		return false, fmt.Errorf("reading %q from the relocated class binding: %w", id, err)
	}
}

// routes reports whether a claim-time write for id must run against the binding
// instead of the work store, given the error the work-scope write returned.
//
// workErr is the ONLY thing that can open the escalation: nil, or anything other
// than the not-found that proves the bead is not in the store that answered,
// means the work store's answer is the answer.
func (r *hookClaimClassRoute) routes(id string, workErr error) (bool, error) {
	if r == nil || !hookClaimBeadIsElsewhere(workErr) {
		return false, nil
	}
	return r.holds(id)
}

// claim acquires a binding-resident bead through the closed graph contract,
// applying the same post-mutation classification hookClaimWithBdStore applies to
// the work store so the two claim paths report a lost race, a stale projection
// and a canonical-readback failure identically.
func (r *hookClaimClassRoute) claim(beadID, assignee string) (beads.Bead, bool, error) {
	return hookClaimThroughStore(beadID, assignee,
		func() (beads.Bead, bool, error) { return r.graph.Claim(beadID, assignee) },
		r.graph.Get)
}

// listContinuation reads a continuation group out of the binding and records
// every member as resident, so the per-sibling assignment that follows does not
// re-probe (or re-fail) one bd subprocess at a time.
func (r *hookClaimClassRoute) listContinuation(rootID, group string) ([]beads.Bead, error) {
	siblings, err := r.graph.List(beads.ListQuery{
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
		},
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("listing continuation group %q of %q in the relocated class binding: %w", group, rootID, err)
	}
	for _, sibling := range siblings {
		if id := strings.TrimSpace(sibling.ID); id != "" {
			r.resident[id] = true
		}
	}
	return siblings, nil
}

// emitExecutionStepStarted records the durable lifecycle-start fact against the
// binding that owns the step.
//
// It mirrors hookEmitExecutionStepStarted, whose own comment asserts that "the
// hook's bd context owns both the claimed graph step and its workflow root" —
// true on a single-store city and false on a split one, where both live in the
// binding and the work-directory bd context can read neither. Routing it is what
// makes that sentence true again; EmitLifecycle still performs the authoritative
// graph.v2 root validation before recording anything.
func (r *hookClaimClassRoute) emitExecutionStepStarted(step beads.Bead) {
	rec := openCityRecorder(io.Discard)
	if closer, ok := rec.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // lifecycle events are best-effort
	}
	_ = executionevent.EmitLifecycle(rec, r.class, events.ExecutionStepStarted, step, eventActor())
}

// classRoutedHookClaimOps returns ops whose claim-time writes fall back to the
// relocated coordination-class binding for a bead the agent's work store does
// not hold.
//
// A nil route returns ops UNCHANGED — not a wrapper that always delegates — so a
// single-store city runs the identical function values it runs today and pays
// nothing for a seam it cannot use.
//
// Every wrapped seam applies the same rule: run the work-scope write; escalate
// only on the not-found that proves the bead is not there; take the binding only
// when it is proved to hold the id. The one exception is ListContinuation, which
// is a QUERY and has no not-found to escalate on — see below.
func classRoutedHookClaimOps(ops hookClaimOps, route *hookClaimClassRoute) hookClaimOps {
	if route == nil {
		return ops
	}
	ops.applyDefaults()
	base := ops

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		if route.knownResident(beadID) {
			return route.claim(beadID, assignee)
		}
		claimed, ok, err := base.Claim(ctx, dir, env, beadID, assignee)
		routed, probeErr := route.routes(beadID, err)
		switch {
		case probeErr != nil:
			return beads.Bead{}, false, probeErr
		case routed:
			return route.claim(beadID, assignee)
		default:
			return claimed, ok, err
		}
	}

	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		write := func() error { return route.graph.Update(beadID, beads.UpdateOpts{Metadata: patch}) }
		if route.knownResident(beadID) {
			return write()
		}
		err := base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		routed, probeErr := route.routes(beadID, err)
		switch {
		case probeErr != nil:
			return probeErr
		case routed:
			return write()
		default:
			return err
		}
	}

	ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, error) {
		if route.knownResident(beadID) {
			return route.graph.Get(beadID)
		}
		bead, err := base.ReadWorkMeta(ctx, dir, env, beadID, assignee)
		routed, probeErr := route.routes(beadID, err)
		switch {
		case probeErr != nil:
			return beads.Bead{}, probeErr
		case routed:
			return route.graph.Get(beadID)
		default:
			return bead, err
		}
	}

	// A continuation LIST is the one claim-time call with no not-found to
	// escalate on: a query against a store that holds no member of the group
	// returns an empty list, not an error. Escalating on EMPTY is what keeps
	// this honest — the work store's answer is never replaced, only an answer
	// it had nothing to say about is filled, and only when the binding is
	// proved to hold the group's root. A list error still fails loud: returning
	// an empty list from the wrong store because a read failed is precisely the
	// silent-empty this seam must not reproduce.
	ops.ListContinuation = func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
		if route.knownResident(rootID) {
			return route.listContinuation(rootID, group)
		}
		siblings, err := base.ListContinuation(ctx, dir, env, rootID, group)
		if err != nil || len(siblings) > 0 {
			return siblings, err
		}
		held, probeErr := route.holds(rootID)
		switch {
		case probeErr != nil:
			return nil, probeErr
		case held:
			return route.listContinuation(rootID, group)
		default:
			return siblings, nil
		}
	}

	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		write := func() error { return route.graph.Update(beadID, beads.UpdateOpts{Assignee: &assignee}) }
		if route.knownResident(beadID) {
			return write()
		}
		err := base.AssignContinuation(ctx, dir, env, beadID, assignee)
		routed, probeErr := route.routes(beadID, err)
		switch {
		case probeErr != nil:
			return probeErr
		case routed:
			return write()
		default:
			return err
		}
	}

	// The lifecycle-start emission reads the step's workflow root, so it belongs
	// in the store the claim landed in. It routes on the MEMO alone and never
	// probes: a step this invocation did not route is one the work store
	// answered for, and emitting it anywhere else would be a second opinion
	// about ownership rather than a consequence of the claim.
	ops.EmitExecutionStepStarted = func(step beads.Bead, dir string, env []string, assignee string) {
		if !route.knownResident(step.ID) {
			base.EmitExecutionStepStarted(step, dir, env, assignee)
			return
		}
		route.emitExecutionStepStarted(step)
	}

	return ops
}
