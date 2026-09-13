package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// Claim delegates to the wrapped store. Embedding the beads.Store INTERFACE
// hides the optional two-argument claim, and a binding without it is refused at
// the door (errClaimRouteBindingCannotClaim) — which would make these rows state
// the capability check instead of the read-failure rule they are about.
func (s claimRouteFailingStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := s.Store.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	})
	if !ok {
		return beads.Bead{}, false, errors.New("wrapped store has no claim CAS")
	}
	return claimer.Claim(id, assignee)
}

func TestClassRoutedClaimEscalatesOnlyOnNotFound(t *testing.T) {
	class := newClaimRouteClassStore(t)
	mintClaimRouteBead(t, class, "gcg-100", nil)
	route := newClaimRouteFor(t, class)

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
	route := newClaimRouteFor(t, class)

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
	route := newClaimRouteFor(t, class)
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
	route := newClaimRouteFor(t, claimRouteFailingStore{Store: newClaimRouteClassStore(t), err: readErr})
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
	route := newClaimRouteFor(t, claimRouteFailingStore{Store: newClaimRouteClassStore(t), err: refusal})
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
	route := newClaimRouteFor(t, class)
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
	route := newClaimRouteFor(t, class)
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
	route := newClaimRouteFor(t, class)
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
	route := newClaimRouteFor(t, class)
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
	route := newClaimRouteFor(t, class)
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

// TestClassRoutedClaimNeverEscalatesACommittedWorkClaim is the ok=true half of
// the elsewhere guard, which both production call sites already apply
// (`if !ok && hookClaimBeadIsElsewhere(err)` on the ready tier, `if ok` as a
// terminal stop on the routed tier).
//
// hookClaimThroughStore returns (claimed, true, err) for exactly one shape: the
// CAS committed and the canonical readback then failed. BdStore.Get produces an
// ErrNotFound-wrapping error for a plain miss, for an empty result AND for
// beads.ErrIDCollision, so that readback error routinely satisfies
// hookClaimBeadIsElsewhere — on a claim that landed. Escalating it claims the
// same logical bead a second time in the binding and swallows the readback
// failure the caller must stop on.
func TestClassRoutedClaimNeverEscalatesACommittedWorkClaim(t *testing.T) {
	class := newClaimRouteClassStore(t)
	// Co-residence: the binding holds the id too, which is the migrated city's
	// documented steady state and the only reason an escalation could land.
	mintClaimRouteBead(t, class, "gcg-b00", nil)
	route := newClaimRouteFor(t, class)

	for _, tt := range []struct {
		name string
		err  error
	}{
		{"plain readback miss", fmt.Errorf("reloading claimed bead %q: %w", "gcg-b00", fmt.Errorf("getting bead %q: %w", "gcg-b00", beads.ErrNotFound))},
		{"substring id collision", fmt.Errorf("reloading claimed bead %q: %w", "gcg-b00", beads.ErrIDCollision)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			readbackErr := tt.err
			committed := beads.Bead{ID: "gcg-b00", Status: "in_progress", Assignee: "worker-1"}
			ops := classRoutedHookClaimOps(hookClaimOps{
				Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					return committed, true, readbackErr
				},
			}, route)
			claimed, ok, err := ops.Claim(context.Background(), "/work", nil, "gcg-b00", "worker-1")
			if !ok {
				t.Fatalf("routed claim returned ok=%v, want true: the work store's CAS committed, so the caller must be told it owns the bead", ok)
			}
			if !errors.Is(err, readbackErr) {
				t.Fatalf("routed claim returned err=%v, want the canonical-readback failure %v returned unchanged; swallowing it lets the caller stamp and emit against state it never confirmed", err, readbackErr)
			}
			if claimed.ID != committed.ID {
				t.Fatalf("routed claim returned bead %+v, want the work store's committed bead %+v", claimed, committed)
			}
			if held, getErr := class.Get("gcg-b00"); getErr != nil || strings.TrimSpace(held.Assignee) != "" {
				t.Fatalf("the binding's copy of gcg-b00 is claimed for %q; a claim that COMMITTED in the work store must never be re-claimed in a second ledger", held.Assignee)
			}
		})
	}
}

// TestHookClaimClassRouteRefusesABindingThatCannotClaim pins the capability
// check at the door, the shape storebinding.NewBeadsNudgeQueue already uses: a
// leaf without the two-argument CAS claim cannot serve a claim-time route, and
// discovering that per-bead in the middle of a tick is what made a whole
// `gc hook --claim` invocation terminal.
//
// *beads.SQLiteStore is the only production store with it; the other compiled-in
// binding provider (beadsworkspace) opens a *beads.NativeDoltStore, which has
// ReleaseIfCurrent and no Claim.
func TestHookClaimClassRouteRefusesABindingThatCannotClaim(t *testing.T) {
	route, err := newHookClaimClassRoute(claimRouteNoCASStore{Store: beads.NewMemStore()})
	if !errors.Is(err, errClaimRouteBindingCannotClaim) {
		t.Fatalf("newHookClaimClassRoute over a binding without the claim CAS = (route=%v err=%v), want errClaimRouteBindingCannotClaim", route, err)
	}
	if route != nil {
		t.Fatal("a refused binding still produced a route; a claim-time route that cannot claim is worse than none")
	}
}

// TestHookClaimRouteVerdictDegradesRatherThanWedgingTheTick states what
// claimHookWork does with each resolution outcome. A binding that cannot claim
// is a standing property of the city's storage configuration, not a fault on
// this bead: the worker must keep claiming the work it CAN reach and say once,
// loudly, that binding-resident work is unclaimable. Every other resolution
// failure still fails closed.
func TestHookClaimRouteVerdictDegradesRatherThanWedgingTheTick(t *testing.T) {
	opened, err := newHookClaimClassRoute(newClaimRouteClassStore(t))
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	for _, tt := range []struct {
		name      string
		route     *hookClaimClassRoute
		err       error
		wantRoute *hookClaimClassRoute
		wantOK    bool
		wantLog   string
	}{
		{name: "opened", route: opened, wantRoute: opened, wantOK: true},
		{
			name:    "binding cannot claim",
			err:     fmt.Errorf("%w: assignment claim", errClaimRouteBindingCannotClaim),
			wantOK:  true,
			wantLog: "assignment claim",
		},
		{
			name:   "any other resolution failure",
			err:    errors.New("opening the binding: permission denied"),
			wantOK: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got, ok := hookClaimRouteVerdict(tt.route, tt.err, &stderr)
			if ok != tt.wantOK {
				t.Fatalf("hookClaimRouteVerdict ok = %v, want %v (stderr=%q)", ok, tt.wantOK, stderr.String())
			}
			if got != tt.wantRoute {
				t.Fatalf("hookClaimRouteVerdict route = %v, want %v", got, tt.wantRoute)
			}
			if tt.err != nil && !strings.Contains(stderr.String(), tt.wantLog) {
				t.Fatalf("stderr = %q, want it to name %q", stderr.String(), tt.wantLog)
			}
		})
	}
}

// claimRouteNoCASStore models the production binding leaf that has no
// two-argument claim: beads.NativeDoltStore, which beadsworkspace's OpenEngine
// returns. beads.MemStore implements Claim, so the capability has to be hidden
// behind a wrapper that does not.
type claimRouteNoCASStore struct{ beads.Store }

// newClaimRouteFor opens a claim-time class route over a store the row controls.
// It observes no work legs, so nothing can preempt the escalation and the rows
// state the escalation RULE alone; the fan-out ordering the loop imposes on it
// belongs to hook_claim_class_fanout_test.go.
func newClaimRouteFor(t *testing.T, class beads.Store) *hookClaimClassRoute {
	t.Helper()
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatalf("newHookClaimClassRoute: %v", err)
	}
	return route
}

// claimCapableCountedStore is a counted binding that forwards the CAS.
//
// countingClassStore embeds the beads.Store INTERFACE, so it advertises no
// two-argument Claim and newHookClaimClassRoute refuses it outright. That
// refusal is correct and must stay — a route that cannot perform the one write
// it exists for is worse than no route — so the capability is restored here, by
// the fixture that wants it, rather than by widening the counter for every
// caller.
//
// Get is NOT overridden: it promotes off the embedded counter, which is what
// keeps the read count the only observable these rows assert on.
type claimCapableCountedStore struct {
	*countingClassStore
	claims beads.Store
}

func (s claimCapableCountedStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := s.claims.(interface {
		Claim(id, assignee string) (beads.Bead, bool, error)
	})
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("the counted binding's leaf %T has no compare-and-swap claim", s.claims)
	}
	return claimer.Claim(id, assignee)
}

// installClaimCapableCountedBinding is installCountedClassBinding for the claim
// route: the same counter and the same restated census verdict, wrapped so the
// route's construction gate admits it.
func installClaimCapableCountedBinding(t *testing.T, cityPath string, relicFree bool) *countingClassStore {
	t.Helper()
	return installCountedClassBindingWrapped(t, cityPath, relicFree, func(c *countingClassStore) beads.Store {
		return claimCapableCountedStore{countingClassStore: c, claims: c.Store}
	})
}

// claimRouteForCountedCity resolves the route the two retirement rows below
// assert on, and fails the row rather than returning a nil one.
func claimRouteForCountedCity(t *testing.T, cityPath string) *hookClaimClassRoute {
	t.Helper()
	route, err := hookClaimClassRouteForCity(cityPath)
	if err != nil {
		t.Fatalf("resolving the claim-time class route: %v", err)
	}
	if route == nil {
		t.Fatal("a city serving its classes from a relocated binding resolved no claim route")
	}
	return route
}

// TestClaimRouteHoldsSkipsTheProbeOnACensusCleanBinding is the claim path's half
// of the retirement the boot census is taken for.
//
// holds() is the residence probe, reached on every work-scope claim that comes
// back not-found, and it read the binding unconditionally. The by-id door stopped
// doing that when it moved onto storeref (ga-qdt5y.18) and this seam kept its own
// hand-rolled version of the same judgement, so a converged relic-free city still
// paid a binding read per escalated bead here while
// TestBdByIDDoorSkipsTheProbeOnACensusCleanBinding reported the saving taken.
//
// Asserted on the read COUNT for the same reason as that row: the answer is
// false either way, and the work that does not happen is the whole point.
func TestClaimRouteHoldsSkipsTheProbeOnACensusCleanBinding(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	counter := installClaimCapableCountedBinding(t, cityPath, true)
	route := claimRouteForCountedCity(t, cityPath)

	held, err := route.holds("gc-abc123")
	if err != nil {
		t.Fatalf("holds on a work-shaped id: %v", err)
	}
	if held {
		t.Error("the binding claims to hold a work-shaped id it never minted; the escalation would claim through the wrong ledger")
	}
	if counter.gets != 0 {
		t.Errorf("the claim route read the class binding %d time(s) for a work-shaped id on a census-clean binding; the retirement the census was taken for is not being taken", counter.gets)
	}
}

// TestClaimRouteHoldsKeepsTheAuthorityLegOnACensusCleanBinding is the
// must-be-silent counterpart, and it is what stops the row above from being
// satisfied by a holds() that stopped reading altogether.
//
// A clean census retires the RESIDENCE probe — the leg that exists only because
// a migration preserved work-shaped ids. It never retires the authority leg: the
// binding is the sole minter of its namespaces, so for an id inside one it is the
// only store that can answer, and "no relics" says nothing about whether it holds
// this particular bead.
func TestClaimRouteHoldsKeepsTheAuthorityLegOnACensusCleanBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	minted := mustCreateClassBead(t, classStore, beads.Bead{Title: "a step the clean binding holds"})
	counter := installClaimCapableCountedBinding(t, cityPath, true)
	route := claimRouteForCountedCity(t, cityPath)

	resident, err := route.holds(minted.ID)
	if err != nil {
		t.Fatalf("holds on an id only the binding can mint: %v", err)
	}
	if !resident {
		t.Fatal("the binding was reported not to hold a bead it minted; the claim would escalate past the only store that can answer")
	}
	if counter.gets != 1 {
		t.Errorf("the claim route read the class binding %d time(s) for an id only that binding can mint, want exactly 1: zero means a clean census retired the authority leg, which nothing may do, and more than one means the probe is being repeated", counter.gets)
	}
}
