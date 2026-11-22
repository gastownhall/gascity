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
	"fmt"

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

// beadCreator is the store a POST /v0/beads body is created in and read back
// from: a work handle, or the class handle the body's class is served from.
type beadCreator interface {
	Create(beads.Bead) (beads.Bead, error)
	Get(id string) (beads.Bead, error)
}

// createStoreForBead picks the store a POST /v0/beads body is created in.
//
// The rule has two halves and the second is what keeps this change safe. On a
// city with no class binding — the shipped default — the answer is exactly
// the rig's work store (findWorkStore), and the resolver is never consulted: a
// placement plan over a topology whose work leg is nil has no leg at all, and
// turning today's "rig is required" into a planning error would break a working
// create. On a converged city an infrastructure-class body is placed at the
// binding, because the work ledger it would otherwise land in is the store the
// city moved that class off; a bead minted there carries the work ledger's id
// prefix and cannot be relocated afterwards by copying it. The bead is created
// through the class's own handle, which wraps that binding, and which refuses
// where this site may not serve the class.
//
// An infrastructure class combined with an explicit rig is refused rather than
// resolved. A binding is city-keyed — there is one per city and no per-rig
// binding to route to — so the request names something that does not exist, and
// both silent answers are lies: honoring the rig re-creates the stranded write,
// ignoring it returns a bead that is not where the caller was told to look.
//
// Placement is by CLASS and by rig, and b.ParentID is deliberately not
// consulted. Following it would be the intuitive rule and the wrong one: a
// graph-class step whose parent is a work bead in a rig ledger would be minted
// under the rig's prefix, in the store the city moved the graph class off, and
// no later copy can change an id. ParentID stays a weak city-scoped reference
// (see beads.Bead.ParentID) precisely so a cross-store parent is a normal
// shape here rather than a placement input.
func (s *Server) createStoreForBead(b beads.Bead, rig string) (beadCreator, error) {
	class := coordclass.Classify(b)
	st := s.state.Stores()
	if !class.IsInfrastructure() {
		return s.storeOrRigRequired(st, rig)
	}
	topo := st.Topology()
	if len(topo.Bindings) == 0 && topo.Refused == nil {
		return s.storeOrRigRequired(st, rig)
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
		return s.storeOrRigRequired(st, rig)
	}
	store, err := classCreator(st, class)
	if err != nil {
		return nil, err
	}
	if rig != "" {
		return nil, apierr.InvalidRequest.Msg("rig must be empty for a " + class.String() + " bead: this city serves that class from a city-scoped store, not from any rig")
	}
	return store, nil
}

// classCreator is the handle st serves infrastructure class c from.
//
// A handle this site refuses (a worker site's per-class refusal) is refused
// here, as the standing refusal it carries, so the create is the 400 a refused
// city's plan gives rather than the 500 of a failed write.
//
// Only the graph and sessions classes reach here from POST /v0/beads: every
// other infrastructure class's rows are owned, and refuseGenericWrite refuses
// a generic create of one before the store is chosen. The other arms keep the
// map total.
func classCreator(st storeref.Stores, c coordclass.Class) (beadCreator, error) {
	switch c {
	case coordclass.ClassGraph:
		return unlessRefused(st.Graph())
	case coordclass.ClassSessions:
		return unlessRefused(st.Sessions())
	case coordclass.ClassMessaging:
		return unlessRefused(st.Mail())
	case coordclass.ClassOrders:
		return unlessRefused(st.Orders())
	case coordclass.ClassNudges:
		return unlessRefused(st.Nudges())
	default:
		return nil, apierr.Internal.Msg(fmt.Sprintf("no class handle serves %q", c.String()))
	}
}

// unlessRefused is handle as the store a create is made in, or the 400 of the
// standing refusal it carries.
func unlessRefused[H interface {
	apiClassStore
	beadCreator
}](handle H) (beadCreator, error) {
	if refusing, ok := classAs[storeref.RefusingStore](handle); ok {
		return nil, apierr.InvalidRequest.Msg(refusing.StorageRefusal().Error())
	}
	return handle, nil
}

// storeOrRigRequired is findWorkStore with its miss spelled as the refusal
// callers already receive.
func (s *Server) storeOrRigRequired(st storeref.Stores, rig string) (beadCreator, error) {
	store, ok := findWorkStore(st, s.state.CityName(), rig)
	if !ok {
		return nil, errRigRequired()
	}
	return store, nil
}
