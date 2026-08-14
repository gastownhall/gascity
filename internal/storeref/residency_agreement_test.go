package storeref

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The reader-agreement property.
//
// The D1-D9 divergence class is one shape: the surface that COUNTS work and the
// surface that CLAIMS it read different store sets, so a bead is demanded and
// never claimable (the spawn/idle treadmill) or claimable and never demanded
// (the stranded claim). Stated as a property over the resolver's own plans, it
// is checkable without a city:
//
//	every bead the RoutedWork/Census plans surface is reachable through the
//	AssignedWork claim-escalation plan, and vice versa.
//
// The controls fail with their own signatures: a HELD bead is excluded from
// both sides (an exclusion failure, not an agreement failure) and a bead
// resident in NO leg is absent from both (a fixture failure). Neither can be
// mistaken for the divergence the property is named for.

const holdLabel = "hold:manual"

// beadID is the identity function the union's first-leg-wins dedupe runs on.
func beadID(b beads.Bead) string { return b.ID }

// listOpenLeg is the per-leg read the demand union performs. It applies the
// SAME hold exclusion the claim side applies, so a hold can never look like a
// disagreement.
func listOpenLeg(l Leg) ([]beads.Bead, error) {
	got, err := l.Store.ListOpen()
	if err != nil {
		return nil, err
	}
	out := make([]beads.Bead, 0, len(got))
	for _, b := range got {
		if held(b) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func held(b beads.Bead) bool {
	for _, l := range b.Labels {
		if l == holdLabel {
			return true
		}
	}
	return false
}

func containsID(items []beads.Bead, id string) bool {
	for _, b := range items {
		if b.ID == id {
			return true
		}
	}
	return false
}

func idsOf(items []beads.Bead) []string {
	out := make([]string, 0, len(items))
	for _, b := range items {
		out = append(out, b.ID)
	}
	sort.Strings(out)
	return out
}

// seedAgreementFixture puts one open routed bead in every leg, plus the two
// shapes the property is really about: a CO-RESIDENT id (the same id in work
// and in a binding) and a MIGRATE-PRESERVED id (a work-shaped id resident only
// in the binding). It returns the ids it seeded, excluding the held control.
func seedAgreementFixture(t *testing.T, f topoFixture) (seeded []string, coResident, relic, heldID, ghostID string) {
	t.Helper()
	// Everything below walks the TOPOLOGY's slices, never the fixture's maps:
	// a map walk picks an arbitrary binding to co-seed and turns the asymmetry
	// assertion into a coin flip.
	f.work.seed(t, "ga-work-1")
	seeded = append(seeded, "ga-work-1")
	for _, leg := range f.topo.Rigs {
		name := strings.TrimPrefix(string(leg.Ref), "rig:")
		id := name + "-only-1"
		f.rigs[name].seed(t, id)
		seeded = append(seeded, id)
	}
	for _, b := range f.topo.Bindings {
		id := b.Prefixes[0] + "-only-1"
		f.bindings[b.Leg.Ref].seed(t, id)
		seeded = append(seeded, id)
	}

	if len(f.topo.Bindings) > 0 {
		first := f.topo.Bindings[0]

		// Co-resident: minted inside THIS binding's reserved namespace and
		// present in both the binding and the work ledger. This is the pair
		// whose two orders (by-id binding-first, claim work-first) must be a
		// DOCUMENTED asymmetry rather than an accident.
		coResident = first.Prefixes[0] + "-coresident-1"
		f.work.seed(t, coResident)
		f.bindings[first.Leg.Ref].seed(t, coResident)
		seeded = append(seeded, coResident)

		// Relic: `gc storage migrate` preserves ids, so a relocated bead keeps
		// its work-era prefix and is invisible to a namespace-gated resolver.
		// A binding that has RETIRED its residence probe is asserting it holds
		// none, so seeding one there would contradict the topology bit.
		if !first.MintsReserved || first.HasLegacyResidents {
			relic = "ga-relic-1"
			f.bindings[first.Leg.Ref].seed(t, relic)
			seeded = append(seeded, relic)
		}
	}

	// Control: a held bead, excluded from BOTH sides.
	heldID = "ga-held-1"
	if _, err := f.work.Create(beads.Bead{ID: heldID, Title: heldID, Type: "task", Labels: []string{holdLabel}}); err != nil {
		t.Fatalf("seed held bead: %v", err)
	}
	// Control: an id resident in no leg at all.
	ghostID = "ga-ghost-1"

	sort.Strings(seeded)
	return seeded, coResident, relic, heldID, ghostID
}

// claimLegFor walks the claim-escalation plan and reports the leg that would
// take the claim, exactly as the executor pins it.
func claimLegFor(t *testing.T, plan ResolvedPlan, id string) (StoreRef, bool) {
	t.Helper()
	_, ref, err := ResolveOwner(plan, id)
	switch {
	case err == nil:
		return ref, true
	case errors.Is(err, beads.ErrNotFound):
		return "", false
	default:
		t.Fatalf("claim escalation for %q: unexpected error: %v", id, err)
		return "", false
	}
}

func TestReaderAgreement(t *testing.T) {
	for _, f := range allTopologies() {
		if f.topo.Refused != nil {
			continue // T3 plans do not exist; TestRefusedTopologyFailsLoud covers it.
		}
		t.Run(f.name, func(t *testing.T) {
			seeded, coResident, relic, heldID, ghostID := seedAgreementFixture(t, f)

			demandPlan := mustPlan(t, RoutedWork{}, f.topo)
			escalation := mustPlan(t, AssignedWork{Purpose: AssignedWorkClaimEscalation}, f.topo)

			demand, err := Union(demandPlan, beadID, listOpenLeg)
			if err != nil {
				t.Fatalf("demand union: %v", err)
			}
			if demand.Partial {
				t.Fatalf("demand union reported Partial over healthy legs: %v", demand.LegErrors)
			}

			// 1. demand == claimable, both directions.
			demandIDs := idsOf(demand.Items)
			var claimable []string
			for _, id := range seeded {
				if _, ok := claimLegFor(t, escalation, id); ok {
					claimable = append(claimable, id)
				}
			}
			sort.Strings(claimable)
			if strings.Join(demandIDs, ",") != strings.Join(claimable, ",") {
				t.Fatalf("demand and claim disagree:\n demand:    %v\n claimable: %v", demandIDs, claimable)
			}

			// 2a. Demand/claim convergence on a co-resident id: the copy the
			// union attributes and the copy the claim takes are the SAME leg.
			if coResident != "" {
				claimLeg, ok := claimLegFor(t, escalation, coResident)
				if !ok {
					t.Fatalf("co-resident %q is not claimable", coResident)
				}
				demandLeg := legThatContributed(t, demandPlan, coResident)
				if claimLeg != demandLeg {
					t.Fatalf("co-resident %q: demand counts it on %q but the claim lands on %q — the demand/claim divergence this property exists for", coResident, demandLeg, claimLeg)
				}
				if claimLeg != WorkRef {
					t.Fatalf("co-resident %q converged on %q, want the work leg: binding-LAST on the federation intents is what makes the two orders agree", coResident, claimLeg)
				}

				// 2b. The DOCUMENTED asymmetry of the intent pair: the by-id
				// read surface pins the BINDING for the same id, because the
				// binding is the sole minter of its namespace. Pinned here so
				// a future "fix" that flips either order has to change a row.
				byID := mustPlan(t, ByID{ID: coResident}, f.topo)
				_, byIDRef, err := ResolveOwner(byID, coResident)
				if err != nil {
					t.Fatalf("ByID(%q): %v", coResident, err)
				}
				if byIDRef == claimLeg {
					t.Fatalf("ByID and claim escalation both pinned %q for the co-resident id; the asymmetry is deliberate and must stay visible", byIDRef)
				}
			}

			// 3. Release closes the loop: a claim taken through the escalation
			// plan is visible to the sweep union, carrying its leg's StoreRef.
			if relic != "" {
				claimLeg, ok := claimLegFor(t, escalation, relic)
				if !ok {
					t.Fatalf("migrate-preserved relic %q is not claimable", relic)
				}
				store := f.legStore(claimLeg)
				if store == nil {
					t.Fatalf("claim leg %q has no fixture store", claimLeg)
				}
				assignee := "agent-1"
				if err := store.Update(relic, beads.UpdateOpts{Assignee: &assignee}); err != nil {
					t.Fatalf("claim %q on %q: %v", relic, claimLeg, err)
				}
				sweep := mustPlan(t, AssignedWork{Identifiers: []string{assignee}}, f.topo)
				found, err := Union(sweep, beadID, func(l Leg) ([]beads.Bead, error) {
					return l.Store.ListByAssignee(assignee, "", 0)
				})
				if err != nil {
					t.Fatalf("assigned-work sweep: %v", err)
				}
				if !containsID(found.Items, relic) {
					t.Fatalf("claim on %q is invisible to the assigned-work sweep (%v) — the release/wake blindness shape", claimLeg, idsOf(found.Items))
				}
				if !planCarriesRef(sweep, claimLeg) {
					t.Fatalf("the sweep plan has no leg with ref %q, so the wake filter can never match the claim's StoreRef", claimLeg)
				}
			}

			// Control (exclusion signature): the held bead is in NEITHER set.
			if containsID(demand.Items, heldID) {
				t.Fatalf("held bead %q leaked into demand — an EXCLUSION failure, not an agreement failure", heldID)
			}
			if _, ok := claimLegFor(t, escalation, heldID); !ok {
				t.Fatalf("held bead %q is not resident anywhere; the exclusion control is not exercising a real bead", heldID)
			}

			// Control (fixture signature): a bead in no leg is absent from both.
			if containsID(demand.Items, ghostID) {
				t.Fatalf("ghost id %q appeared in demand — fixture rot", ghostID)
			}
			if _, ok := claimLegFor(t, escalation, ghostID); ok {
				t.Fatalf("ghost id %q resolved to a leg — fixture rot", ghostID)
			}
		})
	}
}

// legThatContributed reports the first leg of a union plan that holds id — the
// leg first-leg-wins attributes the row to.
func legThatContributed(t *testing.T, p ResolvedPlan, id string) StoreRef {
	t.Helper()
	for _, l := range p.Legs {
		if _, err := l.Leg.Store.Get(id); err == nil {
			return l.Leg.Ref
		}
	}
	t.Fatalf("no leg of %s holds %q", p, id)
	return ""
}

func planCarriesRef(p ResolvedPlan, ref StoreRef) bool {
	for _, l := range p.Legs {
		if l.Leg.Ref == ref {
			return true
		}
	}
	return false
}
