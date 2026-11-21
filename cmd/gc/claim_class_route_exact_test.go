package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

// The exact-claim rows of the claim-time class routing. The pool-claim rows
// in claim_class_route_test.go own the shared escalation rule; these rows own
// its one exact-claim sharpening — the seam's own Get is the work leg's
// answer, so only its not-found opens the escalation — and the binding leg
// that serves the escalated claim.

// exactClaimRouteRequest builds the claim a hook would plan for a binding-
// resident graph step observed open and unassigned.
func exactClaimRouteRequest(id string) molecule.ClaimExactRequest {
	status := "open"
	assignee := ""
	step := "build"
	root := "gcg-root-1"
	inProgress := "in_progress"
	owner := "worker-1"
	return molecule.ClaimExactRequest{
		ID: id,
		Want: molecule.ClaimExactPreconditions{
			Status:     &status,
			Assignee:   &assignee,
			StepID:     &step,
			RootBeadID: &root,
		},
		OnSuccess: beads.UpdateOpts{Status: &inProgress, Assignee: &owner},
	}
}

// mintClaimRouteStep creates the graph step the escalated exact claim lands
// on, with the identity the request fences.
func mintClaimRouteStep(t *testing.T, store beads.Store, id string) {
	t.Helper()
	mintClaimRouteBead(t, store, id, map[string]string{
		beadmeta.RootBeadIDMetadataKey: "gcg-root-1",
		beadmeta.StepIDMetadataKey:     "build",
	})
}

// TestClassRoutedExactClaimEscalatesOnlyOnTheGetOwnNotFound: the work leg's
// exact claim cannot even read the row — its own Get answers not-found, the
// one signal that opens the class escalation — and the binding serves the
// claim fenced on the same observed identity, memoizing the row so follow-on
// release and stamp writes route to the binding.
func TestClassRoutedExactClaimEscalatesOnlyOnTheGetOwnNotFound(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteStep(t, class, "gcg-100")
	route := newClaimRouteFor(t, class)

	base := hookClaimOps{ClaimExact: func(_ context.Context, _ string, _ []string, _ string, req molecule.ClaimExactRequest) (molecule.ClaimExactResult, error) {
		return molecule.ClaimExactResult{}, fmt.Errorf("claim exact %q: %w", req.ID, beads.ErrNotFound)
	}}
	ops := classRoutedHookClaimOps(base, route)

	res, err := ops.ClaimExact(context.Background(), "/work", nil, "worker-1", exactClaimRouteRequest("gcg-100"))
	if err != nil || res.Outcome != molecule.ClaimExactClaimed {
		t.Fatalf("routed exact claim = (%+v, %v), want a claimed outcome", res, err)
	}
	if res.Bead.ID != "gcg-100" || strings.TrimSpace(res.Bead.Assignee) != "worker-1" {
		t.Fatalf("claimed bead = %s/%q, want gcg-100 owned by worker-1", res.Bead.ID, res.Bead.Assignee)
	}
	held, err := class.Get("gcg-100")
	if err != nil || strings.TrimSpace(held.Assignee) != "worker-1" || !strings.EqualFold(held.Status, "in_progress") {
		t.Fatalf("binding holds gcg-100 as status=%q assignee=%q (err=%v), want in_progress owned by worker-1", held.Status, held.Assignee, err)
	}
	if !route.knownResident("gcg-100") {
		t.Fatalf("the escalation did not memoize gcg-100 as binding-resident, so follow-on release/stamp writes would miss the binding")
	}
}

// TestClassRoutedExactClaimKeepsTheWorkLegsAnswerOnEveryOtherResult is the
// narrowness pin. A settled outcome, a write failure on a row the work leg
// read, and a committed claim whose readback failed are all answers about a
// bead the work store demonstrably holds — escalating any of them would claim
// one logical bead in two ledgers — so each is returned exactly as it came
// back and the binding row is never touched.
func TestClassRoutedExactClaimKeepsTheWorkLegsAnswerOnEveryOtherResult(t *testing.T) {
	for _, tt := range []struct {
		name string
		res  molecule.ClaimExactResult
		err  error
	}{
		{
			name: "a settled stale outcome",
			res:  molecule.ClaimExactResult{Bead: beads.Bead{ID: "gcg-200"}, Outcome: molecule.ClaimExactStale, Fence: molecule.ClaimExactFenceRevision},
		},
		{
			name: "a write failure on a row the leg read",
			err:  errors.New("bd update: database is locked"),
		},
		{
			name: "a committed claim whose readback failed",
			res:  molecule.ClaimExactResult{Bead: beads.Bead{ID: "gcg-200", Status: "in_progress", Assignee: "worker-1"}, Outcome: molecule.ClaimExactClaimed, Fence: molecule.ClaimExactFenceRevision},
			err:  fmt.Errorf("claim exact %q: %w", "gcg-200", molecule.ErrClaimExactReadback),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			class := newClaimRouteClassStore(t)
			mintClaimRouteStep(t, class, "gcg-200")
			route := newClaimRouteFor(t, class)

			base := hookClaimOps{ClaimExact: func(_ context.Context, _ string, _ []string, _ string, _ molecule.ClaimExactRequest) (molecule.ClaimExactResult, error) {
				return tt.res, tt.err
			}}
			ops := classRoutedHookClaimOps(base, route)

			res, err := ops.ClaimExact(context.Background(), "/work", nil, "worker-1", exactClaimRouteRequest("gcg-200"))
			if res.Outcome != tt.res.Outcome || res.Fence != tt.res.Fence || res.Bead.ID != tt.res.Bead.ID {
				t.Fatalf("routed answer = %+v, want the work leg's %+v returned verbatim", res, tt.res)
			}
			if (err == nil) != (tt.err == nil) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("routed error = %v, want the work leg's %v returned verbatim", err, tt.err)
			}
			held, getErr := class.Get("gcg-200")
			if getErr != nil || strings.TrimSpace(held.Assignee) != "" || !strings.EqualFold(held.Status, "open") {
				t.Fatalf("the binding row moved without an absence proof: status=%q assignee=%q (err=%v)", held.Status, held.Assignee, getErr)
			}
			if route.knownResident("gcg-200") {
				t.Fatalf("a bead the work leg answered for was memoized into the binding")
			}
		})
	}
}
