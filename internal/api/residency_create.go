package api

// Where a created bead goes.
//
// Every other residency seam on this plane answers "where does this bead
// LIVE" — a probe with an id in hand and a store that either holds it or does
// not. Create is the one question the topology alone can answer and no probe
// can: the bead does not exist yet, so there is nothing to look for, and the
// only thing that decides the store is the class the body describes.
//
// That is why this file executes a ModeSingleOwner plan rather than reusing
// resolveBeadOwner. A placement plan names the owner; a by-id plan names a
// probe ORDER whose leading leg is not a placement.

import (
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// errRigRequired is the pre-existing refusal for a rig-less create on a city
// with no single obvious store. Held as one value so the placement seam cannot
// drift from the message clients already match on.
func errRigRequired() error {
	return apierr.InvalidRequest.Msg("rig is required when multiple rigs are configured")
}

// createStoreForBead picks the store a POST /v0/beads body is created in.
//
// The rule has two halves and the second is what keeps this change safe. On a
// city with no class binding — the shipped default — the answer is exactly
// findStore(rig), byte for byte, and the resolver is never consulted: a
// placement plan over a topology whose work leg is nil has no leg at all, and
// turning today's "rig is required" into a planning error would break a working
// create. On a converged city an infrastructure-class body is placed at the
// binding, because the work ledger it would otherwise land in is the store the
// city moved that class off; a bead minted there carries the work ledger's id
// prefix and cannot be relocated afterwards by copying it.
//
// An infrastructure class combined with an explicit rig is refused rather than
// resolved. A binding is city-keyed — there is one per city and no per-rig
// binding to route to — so the request names something that does not exist, and
// both silent answers are lies: honoring the rig re-creates the stranded write,
// ignoring it returns a bead that is not where the caller was told to look.
func (s *Server) createStoreForBead(b beads.Bead, rig string) (beads.Store, error) {
	class := coordclass.Classify(b)
	if !class.IsInfrastructure() {
		return s.storeOrRigRequired(rig)
	}
	topo := s.residencyTopology()
	if len(topo.Bindings) == 0 && topo.Refused == nil {
		return s.storeOrRigRequired(rig)
	}
	// A standing refusal with no binding to attach it to still has to reach
	// Plan: it means this build cannot serve the class's store, and falling
	// through to the work ledger is the one answer that is never right.
	plan, err := storeref.Plan(storeref.Class{C: class}, topo)
	if err != nil {
		// A standing refusal reaches here as an error, and refusing the create
		// is the point: this build must not serve the binding that owns the
		// class, and writing to the work ledger instead is precisely the
		// stranded write the refusal exists to prevent.
		return nil, apierr.InvalidRequest.Msg(err.Error())
	}
	if !plan.TouchesBinding() {
		return s.storeOrRigRequired(rig)
	}
	if rig != "" {
		return nil, apierr.InvalidRequest.Msg("rig must be empty for a " + class.String() + " bead: this city serves that class from a city-scoped store, not from any rig")
	}
	owner, err := storeref.ResolvePlacement(plan)
	if err != nil {
		return nil, apierr.Internal.Msg(err.Error())
	}
	return owner.Store, nil
}

// storeOrRigRequired is findStore with its nil case spelled as the refusal
// callers already receive.
func (s *Server) storeOrRigRequired(rig string) (beads.Store, error) {
	store := s.findStore(rig)
	if store == nil {
		return nil, errRigRequired()
	}
	return store, nil
}
