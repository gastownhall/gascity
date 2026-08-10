package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The claim-time class routing unit rows. The split-topology conformance suite
// (I5, I15) owns the end-to-end statement over both store arrangements; these
// rows own the escalation RULE itself — which errors open it, which never do,
// and what each wrapped seam does on either side of it — because that rule is
// where a claim write can corrupt ownership rather than merely fail.

// newClaimRouteClassStore opens the store a split city serves its coordination
// classes from: a real beads.SQLiteStore under the graph class's reserved
// prefix, which is what internal/storebinding/sqlite's OpenEngine opens. It is
// not a MemStore, because the CAS a routed claim acquires through is a
// capability only the real store has.
func newClaimRouteClassStore(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the class binding store: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	return store
}

// mintClaimRouteBead creates a bead in the binding with a pinned id, the way a
// relocated graph step exists there.
func mintClaimRouteBead(t *testing.T, store beads.Store, id string, metadata map[string]string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{ID: id, Title: "graph step " + id, Type: "task", Metadata: metadata})
	if err != nil {
		t.Fatalf("minting %s in the class binding: %v", id, err)
	}
	return created
}

// notFoundClaim is the work-scope claim of a bead the work store does not hold:
// the not-found that is the ONLY signal permitted to open the class escalation.
func notFoundClaim(t *testing.T, wantID string) hookClaimFunc {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		if beadID != wantID {
			t.Errorf("work-scope claim called for %q, want %q", beadID, wantID)
		}
		return beads.Bead{}, false, beads.ErrNotFound
	}
}

// claimRouteFailingStore is a binding whose reads fail with a supplied error, so
// a row can state what the routing does with a read that FAILED as distinct from
// one that answered "absent".
type claimRouteFailingStore struct {
	beads.Store
	err error
}

func (s claimRouteFailingStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

func TestClassRoutedClaimEscalatesOnlyOnNotFound(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-100", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}

	// The escalation: the work store proves it does not hold the bead, the
	// binding does, and the claim lands there.
	ops := classRoutedHookClaimOps(hookClaimOps{Claim: notFoundClaim(t, "gcg-100")}, route)
	claimed, ok, err := ops.Claim(context.Background(), "/work", nil, "gcg-100", "worker-1")
	if err != nil || !ok {
		t.Fatalf("routed claim of gcg-100 = (ok=%v err=%v), want a successful claim", ok, err)
	}
	if strings.TrimSpace(claimed.Assignee) != "worker-1" {
		t.Fatalf("routed claim returned assignee %q, want %q", claimed.Assignee, "worker-1")
	}
	held, err := class.Get("gcg-100")
	if err != nil || strings.TrimSpace(held.Assignee) != "worker-1" || !strings.EqualFold(held.Status, "in_progress") {
		t.Fatalf("binding holds gcg-100 as status=%q assignee=%q (err=%v), want in_progress owned by worker-1", held.Status, held.Assignee, err)
	}
}

// TestClassRoutedClaimFailsClosedOnEveryOtherError is the narrowness pin. An
// error that does not PROVE the bead is elsewhere leaves ownership unresolved on
// a bead this session may already own, so it must be returned as-is and never
// retried against a second store — retrying it is how one bead gets claimed
// twice.
func TestClassRoutedClaimFailsClosedOnEveryOtherError(t *testing.T) {
	class := newClaimRouteClassStore(t)
	// The binding DOES hold the bead, so the only thing keeping the claim off it
	// is the classification of the work store's error.
	mintClaimRouteBead(t, class, "gcg-200", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}

	for _, tt := range []struct {
		name string
		err  error
	}{
		{"write timeout", context.DeadlineExceeded},
		{"store contention", errors.New("bd update --claim: database is locked")},
		{"controller socket flap", errors.New("dial unix .gc/controller.sock: connect: connection refused")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workErr := tt.err
			ops := classRoutedHookClaimOps(hookClaimOps{
				Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					return beads.Bead{}, false, workErr
				},
			}, route)
			_, ok, err := ops.Claim(context.Background(), "/work", nil, "gcg-200", "worker-1")
			if ok || !errors.Is(err, workErr) {
				t.Fatalf("claim = (ok=%v err=%v), want the work store's own error returned unchanged; only beads.ErrNotFound may send a claim to a second store", ok, err)
			}
			if held, getErr := class.Get("gcg-200"); getErr != nil || strings.TrimSpace(held.Assignee) != "" {
				t.Fatalf("the binding's copy of gcg-200 was claimed for %q after a %s; an unresolved work-store mutation must not be retried elsewhere", held.Assignee, tt.name)
			}
		})
	}
}

// TestClassRoutedClaimKeepsTheWorkCopyOnCoResidence states the tie-break. A
// migrated city holds the same id in both stores (`gc storage migrate` copies
// with ids preserved and never deletes back), and the federated `gc ready` that
// served this claim its candidate dedupes such an id to the WORK row. The claim
// must agree, or it takes the class copy while the reader keeps re-serving the
// still-open work copy every tick.
func TestClassRoutedClaimKeepsTheWorkCopyOnCoResidence(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-300", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	workClaimed := false
	ops := classRoutedHookClaimOps(hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			workClaimed = true
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
	}, route)
	if _, ok, err := ops.Claim(context.Background(), "/work", nil, "gcg-300", "worker-1"); !ok || err != nil {
		t.Fatalf("co-resident claim = (ok=%v err=%v), want the work store's successful claim", ok, err)
	}
	if !workClaimed {
		t.Fatal("the work-scope claim never ran; the work store is probed FIRST so co-residence resolves to its copy")
	}
	if held, err := class.Get("gcg-300"); err != nil || strings.TrimSpace(held.Assignee) != "" {
		t.Fatalf("the binding's copy of gcg-300 was claimed for %q; a co-resident id must keep the WORK copy, which is what the reader that served it answers", held.Assignee)
	}
}

// TestClassRoutedClaimSurfacesABindingReadFailure pins that a read that FAILED
// is never read as absence. Flattening it would let a claim fall back to the
// work store's not-found and report the bead as gone while the binding still
// holds it — the root-loss shape.
func TestClassRoutedClaimSurfacesABindingReadFailure(t *testing.T) {
	readErr := errors.New("binding unreachable: i/o timeout")
	route, err := newHookClaimClassRoute(claimRouteFailingStore{Store: newClaimRouteClassStore(t), err: readErr})
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	ops := classRoutedHookClaimOps(hookClaimOps{Claim: notFoundClaim(t, "gcg-400")}, route)
	_, ok, err := ops.Claim(context.Background(), "/work", nil, "gcg-400", "worker-1")
	if ok || !errors.Is(err, readErr) {
		t.Fatalf("claim over an unreachable binding = (ok=%v err=%v), want the read failure surfaced; absence and unreachability are not the same answer", ok, err)
	}
}

// TestClassRoutedClaimTreatsAStandingRefusalAsACityFact mirrors
// classRoutedStoreForID and bdByIDClassDoor.resolve: the one-shot funnel's
// standing refusal says this CITY's storage configuration cannot be served,
// which is a statement about the city and none about a particular bead. A
// refused city still serves work from its work ledger, so a work-shaped id keeps
// its own store's answer; a reserved-prefix id has nowhere else to live, so the
// refusal is the answer and surfaces.
func TestClassRoutedClaimTreatsAStandingRefusalAsACityFact(t *testing.T) {
	refusal := standingStorageRefusal{err: errors.New("this city's storage configuration cannot be served; run `gc storage migrate`")}
	route, err := newHookClaimClassRoute(claimRouteFailingStore{Store: newClaimRouteClassStore(t), err: refusal})
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	for _, tt := range []struct {
		name, id  string
		wantErrIs error
	}{
		{"work-shaped id — the work store's answer stands", "gc-500", beads.ErrNotFound},
		{"reserved-prefix id — only the binding could own it", "gcg-500", refusal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ops := classRoutedHookClaimOps(hookClaimOps{Claim: notFoundClaim(t, tt.id)}, route)
			_, ok, err := ops.Claim(context.Background(), "/work", nil, tt.id, "worker-1")
			if ok || !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("claim of %s on a refused city = (ok=%v err=%v), want %v", tt.id, ok, err, tt.wantErrIs)
			}
		})
	}
}

// TestClassRoutedStampAndReadFollowTheClaim covers the writes that hang off the
// claim. A stamp that ran against the work store would drop gc.work_branch and
// the durable session back-reference on the floor for exactly the beads the
// routing exists for, and the dashboard's run detail reads those.
func TestClassRoutedStampAndReadFollowTheClaim(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-600", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	ops := classRoutedHookClaimOps(hookClaimOps{
		Claim: notFoundClaim(t, "gcg-600"),
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return beads.ErrNotFound
		},
		ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
			return beads.Bead{}, beads.ErrNotFound
		},
	}, route)

	patch := map[string]string{
		beadmeta.WorkBranchMetadataKey: "feat/split",
		beadmeta.SessionIDMetadataKey:  "gcg-sess-1",
	}
	if err := ops.StampWorkMeta(context.Background(), "/work", nil, "gcg-600", "worker-1", patch); err != nil {
		t.Fatalf("routed stamp: %v", err)
	}
	stamped, err := ops.ReadWorkMeta(context.Background(), "/work", nil, "gcg-600", "worker-1")
	if err != nil {
		t.Fatalf("routed readback: %v", err)
	}
	for key, want := range patch {
		if got := strings.TrimSpace(stamped.Metadata[key]); got != want {
			t.Errorf("routed readback of gcg-600 has %s=%q, want %q", key, got, want)
		}
	}
}

// TestClassRoutedContinuationEscalatesOnEmpty covers the one claim-time call
// with no not-found to escalate on: a LIST against a store holding no member of
// the group returns an empty slice, not an error.
//
// The rule is monotone — the work store's answer is never replaced, only an
// answer it had nothing to say about is filled — which is also the fix for the
// branch bug this seam was warned about, where the continuation list returned an
// empty slice from the wrong store while claiming to fail loud.
func TestClassRoutedContinuationEscalatesOnEmpty(t *testing.T) {
	class := newClaimRouteClassStore(t)
	root := mintClaimRouteBead(t, class, "gcg-700", nil)
	group := map[string]string{
		beadmeta.RootBeadIDMetadataKey:        root.ID,
		beadmeta.ContinuationGroupMetadataKey: "batch-1",
	}
	sibling := mintClaimRouteBead(t, class, "gcg-701", group)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	ops := classRoutedHookClaimOps(hookClaimOps{
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		AssignContinuation: func(context.Context, string, []string, string, string) error {
			t.Error("the work-scope assign ran for a binding-resident sibling; listing it through the binding must record its residence")
			return nil
		},
	}, route)

	siblings, err := ops.ListContinuation(context.Background(), "/work", nil, root.ID, "batch-1")
	if err != nil {
		t.Fatalf("routed continuation list: %v", err)
	}
	if len(siblings) != 1 || siblings[0].ID != sibling.ID {
		t.Fatalf("routed continuation list = %+v, want exactly %s", siblings, sibling.ID)
	}
	if err := ops.AssignContinuation(context.Background(), "/work", nil, sibling.ID, "worker-1"); err != nil {
		t.Fatalf("routed continuation assign: %v", err)
	}
	assigned, err := class.Get(sibling.ID)
	if err != nil || strings.TrimSpace(assigned.Assignee) != "worker-1" {
		t.Fatalf("binding holds %s assigned to %q (err=%v), want worker-1", sibling.ID, assigned.Assignee, err)
	}
}

// TestClassRoutedContinuationKeepsANonEmptyWorkAnswer is the other half of the
// monotone rule: a group the work store DOES answer for is never re-asked, so a
// work-resident continuation group keeps the exact list it has today.
func TestClassRoutedContinuationKeepsANonEmptyWorkAnswer(t *testing.T) {
	class := newClaimRouteClassStore(t)
	// A binding that also holds the root, so only the ordering can keep the
	// work answer.
	mintClaimRouteBead(t, class, "gcg-800", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	work := []beads.Bead{{ID: "gc-1", Status: "open"}}
	ops := classRoutedHookClaimOps(hookClaimOps{
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return work, nil
		},
	}, route)
	siblings, err := ops.ListContinuation(context.Background(), "/work", nil, "gcg-800", "batch-1")
	if err != nil {
		t.Fatalf("continuation list: %v", err)
	}
	if len(siblings) != 1 || siblings[0].ID != "gc-1" {
		t.Fatalf("continuation list = %+v, want the work store's own answer %+v", siblings, work)
	}
}

// TestClassRoutedContinuationListStillFailsLoud pins the branch bug this seam
// must not reproduce: a list ERROR is an error. Answering it with an empty slice
// from another store would report a continuation group as absent because a read
// failed, which is the silent-empty the file comment used to deny.
func TestClassRoutedContinuationListStillFailsLoud(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-900", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	listErr := errors.New("bd list: context deadline exceeded")
	ops := classRoutedHookClaimOps(hookClaimOps{
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, listErr
		},
	}, route)
	if _, err := ops.ListContinuation(context.Background(), "/work", nil, "gcg-900", "batch-1"); !errors.Is(err, listErr) {
		t.Fatalf("continuation list over a failing work store = %v, want the error surfaced", err)
	}
}

// TestClassRoutedLifecycleEmissionFollowsTheClaimOnly pins the narrowest of the
// wrapped seams. The lifecycle-start emission reads the step's workflow root, so
// it belongs in the store the claim landed in — and NOWHERE else. It routes on
// the memo alone: a step this invocation never routed is one the work store
// answered for, and emitting it against the binding would be a second opinion
// about ownership rather than a consequence of the claim.
func TestClassRoutedLifecycleEmissionFollowsTheClaimOnly(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-a00", nil)
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	workEmissions := 0
	base := hookClaimOps{
		Claim: notFoundClaim(t, "gcg-a00"),
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {
			workEmissions++
		},
	}

	// Before any routed write, the emission stays on the work store.
	ops := classRoutedHookClaimOps(base, route)
	ops.EmitExecutionStepStarted(beads.Bead{ID: "gcg-a00"}, "/work", nil, "worker-1")
	if workEmissions != 1 {
		t.Fatalf("work-scope emissions = %d, want 1 before the claim routes; the memo, not a probe, is what moves this seam", workEmissions)
	}

	// After the claim routes the same id, it follows.
	if _, ok, err := ops.Claim(context.Background(), "/work", nil, "gcg-a00", "worker-1"); !ok || err != nil {
		t.Fatalf("routed claim = (ok=%v err=%v)", ok, err)
	}
	ops.EmitExecutionStepStarted(beads.Bead{ID: "gcg-a00"}, "/work", nil, "worker-1")
	if workEmissions != 1 {
		t.Fatalf("work-scope emissions = %d, want 1 — after the claim landed in the binding the emission must not run against the ledger that cannot read the step's root", workEmissions)
	}
}

// TestClassRoutedHookClaimOpsIsInertWithoutABinding is the single-store
// byte-identity statement at the unit level: no route means the caller's own ops
// value comes straight back, not a wrapper that delegates.
func TestClassRoutedHookClaimOpsIsInertWithoutABinding(t *testing.T) {
	assertHookClaimOpsUnwrapped(t, classRoutedHookClaimOps(defaultedHookClaimOps(), nil), defaultedHookClaimOps())

	route, err := hookClaimClassRouteForCity(t.TempDir())
	if err != nil {
		t.Fatalf("hookClaimClassRouteForCity on a city with no [storage]: %v", err)
	}
	if route != nil {
		t.Fatal("hookClaimClassRouteForCity opened a class route for a city that relocates nothing")
	}
}
