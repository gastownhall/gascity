package storeref

// Typed executors (typed-stores T7b, plan §4.4): the plan legs a consumer
// reads, as handles. A visitor receives a TypedLeg, whose reader is a
// read-only leg minted for the leg's gate role, so the split-strict gate sees
// each read as the plan declared it; a write goes through a ResidentStore the
// executor mints for the one leg that earned it (WalkOwner, Sweep, an Owner).
// The raw Leg forms (Union, Walk, EachLeg) stay for the consumers T7c-T7i
// convert.

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// TypedLeg is one plan leg as a consumer reads it.
type TypedLeg struct {
	// Reader reads the leg's store through a leg layer of the plan's gate
	// role (PlanLeg.Gate): federated, relic, or strict.
	Reader beads.FederatedReader

	// Strict reads the same store through a strict leg (MintStrictLeg),
	// whatever the plan's gate role: for a read the consumer makes unmarked
	// today (the order dispatcher's HasOpenWork and cursor, dispatch's
	// live-root scan and child listing), so converting it keeps the gate as
	// sensitive as it was (plan §4.2).
	Strict  beads.FederatedReader
	Ref     StoreRef
	Role    Role
	OnError ErrPolicy
}

// typedLeg mints l's readers.
func typedLeg(l PlanLeg) TypedLeg {
	return TypedLeg{
		Reader:  legReader(l),
		Strict:  beads.MintStrictLeg(l.Leg.Store, string(l.Leg.Ref)),
		Ref:     l.Leg.Ref,
		Role:    l.Role,
		OnError: l.OnError,
	}
}

// legReader is l's read-only leg, for its gate role.
func legReader(l PlanLeg) beads.FederatedReader {
	ref := string(l.Leg.Ref)
	switch l.Gate {
	case beads.FederatedLegRole:
		return beads.MintFederatedLeg(l.Leg.Store, ref)
	case beads.RelicLegRole:
		return beads.MintRelicLeg(l.Leg.Store, ref)
	default:
		return beads.MintStrictLeg(l.Leg.Store, ref)
	}
}

// EachTypedLeg is EachLeg with typed legs: it visits every leg of a plan in
// order and performs no I/O.
func EachTypedLeg(p ResolvedPlan, visit func(TypedLeg)) {
	for _, l := range p.Legs {
		visit(typedLeg(l))
	}
}

// TypedWalk is Walk with typed legs.
func TypedWalk(p ResolvedPlan, visit func(TypedLeg) (stop bool, err error)) (WalkResult, error) {
	out, _, err := walkPlan(p, func(l PlanLeg) (bool, error) { return visit(typedLeg(l)) })
	return out, err
}

// TypedUnion is Union with typed legs.
func TypedUnion[T any](p ResolvedPlan, id func(T) string, fn func(TypedLeg) ([]T, error)) (UnionResult[T], error) {
	return unionPlan(p, id, func(l PlanLeg) ([]T, error) { return fn(typedLeg(l)) })
}

// WalkOwner walks a Union plan until visit proves the leg it is on holds what
// it was looking for (stop), and returns that leg's store as the writable
// handle the caller then writes through: a drain unit convoy minted where its
// member lives, a lazy convoy leg, a bead's scope. No proof is a zero handle
// with a nil error. The walk's policies are Walk's.
func WalkOwner(p ResolvedPlan, visit func(TypedLeg) (stop bool, err error)) (beads.ResidentStore, WalkResult, error) {
	out, stopped, err := walkPlan(p, func(l PlanLeg) (bool, error) { return visit(typedLeg(l)) })
	if err != nil || stopped < 0 {
		return beads.ResidentStore{}, out, err
	}
	leg := p.Legs[stopped].Leg
	return beads.MintResident(leg.Store, string(leg.Ref)), out, nil
}

// Sweep visits every leg of a Union plan with a writable handle on it, for a
// sweep that writes through the legs it reads (the retired-session reassign,
// order retention prune, workflow delete). The handle is minted over the leg's
// reader, so its reads keep the leg's gate role and its writes go to the
// leg's store. The walk's policies are Walk's.
func Sweep(p ResolvedPlan, visit func(TypedLeg, beads.ResidentStore) error) (WalkResult, error) {
	out, _, err := walkPlan(p, func(l PlanLeg) (bool, error) {
		leg := typedLeg(l)
		return false, visit(leg, beads.MintResidentLeg(leg.Reader))
	})
	return out, err
}

// WorkResidual is t's work leg as the writable handle on it: the by-id
// residual a folded miss falls back to, unprobed (cmd/gc's
// by_id_residency.go). A topology with no work store answers a zero handle.
func WorkResidual(t Topology) beads.ResidentStore {
	if t.Work.Store == nil {
		return beads.ResidentStore{}
	}
	return beads.MintResident(t.Work.Store, string(t.Work.Ref))
}

// ResolveOwnerPreferring probes a FirstOwner plan for id with LIVE reads (past
// any cache, through the leg's role-carrying live reader) and returns the
// first leg whose row prefer accepts — the closed copy, for R4's relic events.
// A read that fails before such a row is found is returned, since the failed
// leg may hold it; with none preferred, the first row found wins; with none
// found, beads.ErrNotFound. Every leg is read, the work residual included: the
// question is which copy, not where to read next. A nil prefer accepts the
// first row. A FatalUnlessResolved leg that failed outranks a rejected row
// (TestResolveOwnerPreferringPrecedence).
//
// cmd/gc's committedCloseOwner (api_state.go) is not this executor and stays
// its own code: it skips a failed read and still takes a later closed copy,
// and with no closed copy falls back to its first store whatever was read.
// That is a choice of which store to apply a committed close's effects in,
// not a by-id resolution, and its R4 conversion (T7g) keeps it.
func ResolveOwnerPreferring(p ResolvedPlan, id string, prefer func(beads.Bead) bool) (Owner, error) {
	id, err := checkFirstOwnerPlan(p, id, "ResolveOwnerPreferring")
	if err != nil {
		return Owner{}, err
	}
	var first *Owner
	var unresolved legFailure
	for _, leg := range p.Legs {
		b, err := beads.LiveFor(probeReader(leg)).Get(id)
		if err == nil {
			owner := ownerOf(leg.Leg)
			owner.Bead, owner.Read = b, true
			if prefer == nil || prefer(b) {
				return owner, nil
			}
			if first == nil {
				first = &owner
			}
			continue
		}
		skip, fail := classifyProbeError(leg, id, err)
		switch {
		case skip:
			continue
		case leg.OnError == PolicyFatalUnlessResolved:
			unresolved.remember(leg.Leg.Ref, fail)
			continue
		default:
			return Owner{Ref: leg.Leg.Ref}, fail
		}
	}
	switch {
	case first != nil && unresolved.err == nil:
		return *first, nil
	case unresolved.err != nil:
		return Owner{Ref: unresolved.ref}, unresolved.err
	default:
		return Owner{}, beads.ErrNotFound
	}
}

// walkPlan is the leg walk Walk, TypedWalk, WalkOwner and Sweep share. It
// returns the index of the leg visit stopped on, or -1.
func walkPlan(p ResolvedPlan, visit func(PlanLeg) (stop bool, err error)) (WalkResult, int, error) {
	var out WalkResult
	if p.Mode != ModeUnion {
		return out, -1, fmt.Errorf("storeref: Walk needs a %s plan, got %s", ModeUnion, p.Mode)
	}
	for i, leg := range p.Legs {
		out.Visited++
		stop, err := visit(leg)
		if err != nil {
			if leg.OnError == PolicyRefusalTolerated && IsStandingRefusal(err) {
				// A refused city still serves WORK, and a leg carrying this
				// policy was only ever consulted in case the binding held the
				// answer. The city is refused, not degraded.
				continue
			}
			if leg.OnError != PolicyPartialDegrade {
				out.Partial = true
				return out, -1, fmt.Errorf("reading %s: %w", legName(leg.Leg.Ref), err)
			}
			out.Partial = true
			out.LegErrors = append(out.LegErrors, LegError{Ref: leg.Leg.Ref, Err: err})
			continue
		}
		if stop {
			out.Stopped = true
			out.StoppedAt = leg.Leg.Ref
			return out, i, nil
		}
	}
	return out, -1, nil
}

// unionPlan is the merge Union and TypedUnion share.
func unionPlan[T any](p ResolvedPlan, id func(T) string, fn func(PlanLeg) ([]T, error)) (UnionResult[T], error) {
	var out UnionResult[T]
	seen := make(map[string]bool)
	walked, _, err := walkPlan(p, func(leg PlanLeg) (bool, error) {
		items, ferr := fn(leg)
		if ferr != nil {
			return false, ferr
		}
		for _, item := range items {
			if id != nil {
				if key := id(item); key != "" {
					if seen[key] {
						continue
					}
					seen[key] = true
				}
			}
			out.Items = append(out.Items, item)
		}
		return false, nil
	})
	out.Partial = walked.Partial
	out.LegErrors = walked.LegErrors
	if err != nil {
		out.Items = nil
		out.Partial = true
		return out, err
	}
	return out, nil
}

// BindLegs is p with each leg's store replaced by bind's answer for it: the
// same federation read through other instances of its stores, such as a
// request-bound clone of each (cmd/gc's step reminder over the controller's
// stores; T7e's ScopedStoreLike). Ref, Role, OnError and Gate are kept, and
// the legs are not deduped again: a plan's shape is decided over the stores
// it was planned over, so a bind that answers one clone for two legs reads
// both. The first error bind returns is returned, with the leg it was for.
func BindLegs(p ResolvedPlan, bind func(beads.Store) (beads.Store, error)) (ResolvedPlan, error) {
	out := p
	out.Legs = make([]PlanLeg, len(p.Legs))
	for i, l := range p.Legs {
		store, err := bind(l.Leg.Store)
		if err != nil {
			return ResolvedPlan{}, fmt.Errorf("binding %s: %w", legName(l.Leg.Ref), err)
		}
		if beads.IsNilStore(store) {
			return ResolvedPlan{}, fmt.Errorf("binding %s: the bind answered no store", legName(l.Leg.Ref))
		}
		l.Leg.Store = store
		out.Legs[i] = l
	}
	return out, nil
}
