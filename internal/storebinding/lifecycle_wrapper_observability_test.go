package storebinding_test

// Maintenance and cache observability through the wrappers.
//
// A maintenance or cache error owes three things: it is
// contextual, it is observable, and it never triggers a fallback or a provider
// re-resolution. Those pull in opposite directions — "contextual" tempts a
// wrapper into re-wrapping the inner error, and "never falls back" is only
// provable by counting inner calls — so each is pinned separately here, with
// the context living in the EVENT and the error crossing the wrapper verbatim.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// idleNudgeQueue answers every claim with an empty batch and no error: an idle
// queue, which is what a poller sees almost all of the time.
type idleNudgeQueue struct {
	storebinding.NudgeQueue
	claims int
}

func (q *idleNudgeQueue) ClaimDue(storebinding.ClaimTarget, time.Time) ([]nudgequeue.Item, error) {
	q.claims++
	return nil, nil
}

// TestIdleClaimIsNotReportedAsAConflict pins the classification an operator
// reads. A conflict means a compare-and-swap lost to another holder. An empty
// due-set lost to nobody, and reporting it as a conflict would make every idle
// queue in the fleet look permanently contended.
func TestIdleClaimIsNotReportedAsAConflict(t *testing.T) {
	observer := &recordingObserver{}
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	inner := adapters.Nudges
	idle := &idleNudgeQueue{NudgeQueue: inner.Queue}
	inner.Queue = idle
	wrapped, err := storebinding.WrapNudges(inner, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("wrapping the Nudges front doors: %v", err)
	}
	claimed, err := wrapped.Queue.ClaimDue(storebinding.ClaimTarget{QueueKeys: []string{storebindingtest.ConformanceNudgeAgent}}, time.Now().UTC())
	if err != nil {
		t.Fatalf("ClaimDue on an idle queue: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("the idle queue returned %d items; this test would prove nothing", len(claimed))
	}
	if idle.claims != 1 {
		t.Fatalf("an idle claim reached the queue %d times, want exactly 1", idle.claims)
	}
	stream := observer.stream()
	if !containsString(stream, "nudges/wrapped claim ClaimDue ok") {
		t.Errorf("an idle claim is not reported as a healthy claim; the stream is:\n%s", formatStream(stream))
	}
	if containsString(stream, "nudges/wrapped claim ClaimDue conflict") {
		t.Errorf("an idle claim is reported as a lost compare-and-swap; the stream is:\n%s", formatStream(stream))
	}
}

// failingOrdersStore fails the retention sweep and counts attempts.
type failingOrdersStore struct {
	storebinding.OrdersStore
	err   error
	calls int
}

func (o *failingOrdersStore) CloseRuns(context.Context, []string, string) (int, error) {
	o.calls++
	return 0, o.err
}

// TestMaintenanceFailureIsObservableVerbatimAndNotRetried is AC5 for the
// maintenance arm.
func TestMaintenanceFailureIsObservableVerbatimAndNotRetried(t *testing.T) {
	observer := &recordingObserver{}
	failure := errors.New("retention sweep hit a lock timeout")
	failing := &failingOrdersStore{
		OrdersStore: storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t)).Orders,
		err:         failure,
	}
	wrapped, err := storebinding.WrapOrders(failing, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("wrapping the Orders front door: %v", err)
	}

	closed, got := wrapped.CloseRuns(context.Background(), []string{"gco-1"}, "retention")
	if !errors.Is(got, failure) {
		t.Fatalf("CloseRuns = %v, want the binding's own failure", got)
	}
	// Contextual belongs in the event. The RETURNED error is the binding's,
	// unchanged, because a caller matching a sentinel through a maintenance
	// path matches it through every other path too.
	if got.Error() != failure.Error() {
		t.Errorf("CloseRuns returned %q, want the binding's own %q verbatim", got, failure)
	}
	if closed != 0 {
		t.Errorf("a failed sweep reported %d closed runs", closed)
	}
	if failing.calls != 1 {
		t.Fatalf("a failing sweep was attempted %d times, want exactly 1; the wrapper retried or fell back", failing.calls)
	}
	want := "orders/wrapped maintenance CloseRuns failed"
	if !containsString(observer.stream(), want) {
		t.Errorf("the observed stream never reports %q; it is:\n%s", want, formatStream(observer.stream()))
	}
}

// failingShadowSweep fails the shadow retention sweep.
type failingShadowSweep struct {
	storebinding.NudgeShadows
	err   error
	calls int
}

func (s *failingShadowSweep) SweepStale(string, string, time.Time) error {
	s.calls++
	return s.err
}

// TestShadowSweepFailureIsReportedAsMaintenance proves the other maintenance
// path reports the same way. Two maintenance operations classified differently
// is how half a fleet's sweeps become invisible.
func TestShadowSweepFailureIsReportedAsMaintenance(t *testing.T) {
	observer := &recordingObserver{}
	failure := errors.New("shadow sweep could not reach the store")
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	inner := adapters.Nudges
	sweep := &failingShadowSweep{NudgeShadows: inner.Shadows, err: failure}
	inner.Shadows = sweep
	wrapped, err := storebinding.WrapNudges(inner, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("wrapping the Nudges front doors: %v", err)
	}
	got := wrapped.Shadows.SweepStale("gcn-1", "retention", time.Now().UTC())
	if !errors.Is(got, failure) || got.Error() != failure.Error() {
		t.Fatalf("SweepStale = %v, want the binding's own failure verbatim", got)
	}
	if sweep.calls != 1 {
		t.Fatalf("a failing shadow sweep was attempted %d times, want exactly 1", sweep.calls)
	}
	want := "nudges/wrapped maintenance SweepStale failed"
	if !containsString(observer.stream(), want) {
		t.Errorf("the observed stream never reports %q; it is:\n%s", want, formatStream(observer.stream()))
	}
}

// flakyGraphStore fails its first Get and then serves normally.
type flakyGraphStore struct {
	storebinding.GraphStore
	err   error
	calls int
}

func (f *flakyGraphStore) Get(id string) (beads.Bead, error) {
	f.calls++
	if f.calls == 1 {
		return beads.Bead{}, f.err
	}
	return f.GraphStore.Get(id)
}

// TestFailedCachedReadInstallsNothing is AC5 for the cache arm, and it is the
// half a "cache serves the right value" test cannot see: a read that FAILED
// must leave the cache empty. A cache that installed the zero bead would then
// serve a titleless, statusless bead to every later reader, and the store —
// which the wrapper never writes through — would disagree with all of them.
func TestFailedCachedReadInstallsNothing(t *testing.T) {
	observer := &recordingObserver{}
	store := beads.NewMemStore()
	adapters, err := storebinding.NewBeadsAdapters(store,
		storebinding.BeadsAdapterIdentity{OpenerID: "wrappers", ComponentID: "cache", PhysicalID: "memory"},
		storebindingtest.NewMemoryNudgeQueue())
	if err != nil {
		t.Fatalf("projecting the reference binding: %v", err)
	}
	created, err := adapters.Graph.Create(beads.Bead{Title: "survives", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	failure := errors.New("transient read failure")
	flaky := &flakyGraphStore{GraphStore: adapters.Graph, err: failure}
	wrapped, err := storebinding.WrapGraph(flaky, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		Observer:   observer,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}

	if _, err := wrapped.Get(created.ID); !errors.Is(err, failure) {
		t.Fatalf("the first Get = %v, want the injected failure", err)
	}
	if flaky.calls != 1 {
		t.Fatalf("a failing Get was attempted %d times, want exactly 1", flaky.calls)
	}
	got, err := wrapped.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after the failure: %v", err)
	}
	persisted, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("reading the persisted bead directly: %v", err)
	}
	if got.Title != persisted.Title || got.Status != persisted.Status {
		t.Fatalf("after a failed read the wrapper served %+v while the store holds %+v; the failure was cached", got, persisted)
	}
	stream := observer.stream()
	for _, want := range []string{"graph/wrapped cache Get miss", "graph/wrapped read Get failed"} {
		if !containsString(stream, want) {
			t.Errorf("the observed stream never reports %q; it is:\n%s", want, formatStream(stream))
		}
	}
}

// TestWrapRefusesEveryUnavailableClass completes the construction gate: every
// class refuses, not just the two a spot check covers. A class that accepted an
// unavailable binding would hand a live consumer a front door the binding
// already said it could not serve.
func TestWrapRefusesEveryUnavailableClass(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	unavailable := storebinding.ClassWrapping{Binding: wrappedBindingName}
	refusals := map[string]error{}
	_, refusals["graph"] = storebinding.WrapGraph(adapters.Graph, unavailable)
	_, refusals["sessions"] = storebinding.WrapSessions(adapters.Sessions, unavailable)
	_, refusals["orders"] = storebinding.WrapOrders(adapters.Orders, unavailable)
	_, refusals["nudges"] = storebinding.WrapNudges(adapters.Nudges, unavailable)
	_, refusals["messaging"] = storebinding.WrapMessaging(adapters.Messaging, unavailable)
	for class, err := range refusals {
		if !errors.Is(err, storebinding.ErrMissingCapability) {
			t.Errorf("wrapping %s over an unavailable class = %v, want ErrMissingCapability", class, err)
		}
	}

	// A partial front-door set is refused for the same reason: a nil service
	// inside an accepted set is a nil dereference at the first call.
	available := storebinding.ClassWrapping{Binding: wrappedBindingName, Capability: storebindingtest.ReferenceCapability}
	partialNudges := adapters.Nudges
	partialNudges.Shadows = nil
	if _, err := storebinding.WrapNudges(partialNudges, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapNudges with no shadow projection = %v, want ErrInvalidClassWrapping", err)
	}
	partialMessaging := adapters.Messaging
	partialMessaging.Transcripts = nil
	if _, err := storebinding.WrapMessaging(partialMessaging, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapMessaging with no transcript service = %v, want ErrInvalidClassWrapping", err)
	}
	if _, err := storebinding.WrapOrders(nil, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapOrders over a nil front door = %v, want ErrInvalidClassWrapping", err)
	}
	if _, err := storebinding.WrapSessions(nil, available); !errors.Is(err, storebinding.ErrInvalidClassWrapping) {
		t.Errorf("WrapSessions over a nil front door = %v, want ErrInvalidClassWrapping", err)
	}
}

// TestWrappedGraphKeepsEveryDeclaredCapability is AC1's runtime half, over
// both substitution legs, because "survives wrapping OR fails explicitly" is a
// statement about the binding's own answer.
//
// The rule under test is one comparison: for every capability the closed Graph
// contract carries, the WRAPPED front door answers exactly what the BARE front
// door of the same binding answers. A leg that declares claims proves the claim
// reached the store; a leg that does not proves the refusal crossed the wrapper
// unchanged instead of being swallowed, retried, or turned into a nil result.
func TestWrappedGraphKeepsEveryDeclaredCapability(t *testing.T) {
	for _, leg := range wrappedLegs() {
		t.Run(leg.name, func(t *testing.T) {
			runner := storebindingtest.Wrap(t)
			adapters := leg.open(runner)
			bare := adapters.Graph
			wrapped, err := storebinding.WrapGraph(bare, storebinding.ClassWrapping{
				Binding:    wrappedBindingName,
				Capability: leg.capability,
				CacheReads: true,
			})
			if err != nil {
				t.Fatalf("wrapping the Graph front door: %v", err)
			}

			created, err := wrapped.Create(beads.Bead{Title: "capable", Type: "task", Status: "open"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			// Claim: the acquire half of the claim capability. The read-back
			// goes through the BARE front door, which the wrapper is not in.
			_, acquired, claimErr := wrapped.Claim(created.ID, "worker-a")
			_, _, bareClaimErr := bare.Claim("gcg-absent-claim-probe", "worker-a")
			if leg.capability.Claims {
				if claimErr != nil || !acquired {
					t.Fatalf("Claim = (%v, %v), want the claim acquired on a leg that declares claims", acquired, claimErr)
				}
				claimed, err := bare.Get(created.ID)
				if err != nil {
					t.Fatalf("reading the claimed bead through the binding: %v", err)
				}
				if claimed.Assignee != "worker-a" {
					t.Fatalf("the persisted bead is assigned to %q, want %q; the claim never reached the store", claimed.Assignee, "worker-a")
				}
			} else {
				if !errors.Is(claimErr, storebinding.ErrBeadsAdapterCapability) {
					t.Fatalf("Claim on a leg that declares no claims = %v, want the binding's typed capability error", claimErr)
				}
				if !errors.Is(bareClaimErr, storebinding.ErrBeadsAdapterCapability) {
					t.Fatalf("the bare front door refused with %v; the comparison below would not be about wrapping", bareClaimErr)
				}
				if claimErr.Error() != bareClaimErr.Error() {
					t.Errorf("the wrapper refused with %q, the binding with %q; the wrapper rewrote the capability refusal", claimErr, bareClaimErr)
				}
			}

			// Transactions: whatever the binding does with Tx, the wrapper
			// does the same — and when it commits, the commit is durable and
			// the cache does not keep the pre-transaction bead. (A leg that
			// declares no transactions may still SERVE Tx without atomicity;
			// the declaration is about the guarantee, so the wrapper is
			// compared against the binding rather than against the flag.)
			txErr := wrapped.Tx(created.ID, func(tx storebinding.GraphTx) error {
				return tx.SetMetadataBatch(created.ID, map[string]string{"phase": "committed"})
			})
			bareTxErr := bare.Tx(created.ID, func(tx storebinding.GraphTx) error {
				return tx.SetMetadataBatch(created.ID, map[string]string{"probe": "bare"})
			})
			if (txErr == nil) != (bareTxErr == nil) {
				t.Fatalf("Tx through the wrapper = %v, through the binding = %v; the wrapper changed the capability", txErr, bareTxErr)
			}
			if txErr == nil {
				committed, err := bare.Get(created.ID)
				if err != nil {
					t.Fatalf("reading the committed bead through the binding: %v", err)
				}
				if committed.Metadata["phase"] != "committed" {
					t.Fatalf("the persisted bead has metadata %v, want phase=committed", committed.Metadata)
				}
				through, err := wrapped.Get(created.ID)
				if err != nil {
					t.Fatalf("Get after the transaction: %v", err)
				}
				if through.Metadata["phase"] != "committed" {
					t.Fatalf("the wrapper serves %v after a committed transaction; the cache kept the pre-transaction bead", through.Metadata)
				}
			} else if txErr.Error() != bareTxErr.Error() {
				t.Errorf("the wrapper refused the transaction with %q, the binding with %q", txErr, bareTxErr)
			}

			// Graph apply: the capability the closed contract requires as a
			// METHOD rather than a type assertion. Whatever the binding
			// answers, the wrapper answers identically — including the typed
			// refusal, which is how a consumer learns it cannot apply graphs.
			plan := func() *beads.GraphApplyPlan {
				return &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{
					Key:   "root",
					Title: "applied",
					Type:  "task",
				}}}
			}
			result, applyErr := wrapped.ApplyGraphPlan(context.Background(), plan())
			_, bareApplyErr := bare.ApplyGraphPlan(context.Background(), plan())
			if (applyErr == nil) != (bareApplyErr == nil) {
				t.Fatalf("ApplyGraphPlan through the wrapper = %v, through the binding = %v; the wrapper changed the capability", applyErr, bareApplyErr)
			}
			if applyErr != nil {
				if applyErr.Error() != bareApplyErr.Error() {
					t.Errorf("the wrapper refused the apply with %q, the binding with %q", applyErr, bareApplyErr)
				}
				return
			}
			if result == nil || len(result.IDs) == 0 {
				t.Fatalf("ApplyGraphPlan through the wrapper returned %+v, want the applied node ids", result)
			}
			for key, id := range result.IDs {
				if _, err := bare.Get(id); err != nil {
					t.Fatalf("the applied bead %s (node %q) is not in the binding the wrapper wraps: %v", id, key, err)
				}
			}
		})
	}
}

// TestWrappedOrdersRunsRetentionThroughTheStore proves the maintenance path is
// not merely observable but effective: a swept run is closed in the store the
// wrapper never reads around.
func TestWrappedOrdersRunsRetentionThroughTheStore(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	wrapped, err := storebinding.WrapOrders(adapters.Orders, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
	})
	if err != nil {
		t.Fatalf("wrapping the Orders front door: %v", err)
	}
	run, err := wrapped.CreateRun("retention-order", orders.RunOpts{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	closed, err := wrapped.CloseRuns(context.Background(), []string{run.ID}, "retention")
	if err != nil {
		t.Fatalf("CloseRuns: %v", err)
	}
	if closed != 1 {
		t.Fatalf("CloseRuns closed %d runs, want 1", closed)
	}
	// Read back through the UNWRAPPED front door: the wrapper is not in this
	// path, so a sweep that only updated wrapper state would be caught here.
	swept, err := adapters.Orders.Get(run.ID)
	if err != nil {
		t.Fatalf("reading the swept run through the binding: %v", err)
	}
	if swept.Open {
		t.Fatalf("the swept run %s is still open; the retention sweep changed nothing durable", swept.ID)
	}
	if !run.Open {
		t.Fatalf("the fixture run was already closed before the sweep; this assertion would prove nothing")
	}
}

// recordingGraphStore captures exactly what the wrapper handed the binding.
type recordingGraphStore struct {
	storebinding.GraphStore
	created  []beads.Bead
	storages []beads.StorageClass
	queries  []beads.ListQuery
	ready    [][]beads.ReadyQuery
}

func (r *recordingGraphStore) Create(bead beads.Bead) (beads.Bead, error) {
	r.created = append(r.created, bead)
	return r.GraphStore.Create(bead)
}

func (r *recordingGraphStore) CreateWithStorage(bead beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	r.created = append(r.created, bead)
	r.storages = append(r.storages, storage)
	return r.GraphStore.CreateWithStorage(bead, storage)
}

func (r *recordingGraphStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	r.queries = append(r.queries, query)
	return r.GraphStore.List(query)
}

func (r *recordingGraphStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	r.ready = append(r.ready, query)
	return r.GraphStore.Ready(query...)
}

// TestWrapperDoesNotReapplyBeadPolicy pins the layering decision the wrapper
// header states: bead storage policy decorates the canonical store BELOW the
// class projection, so the wrapper forwards creates and queries verbatim. A
// wrapper that stamped a storage class or expanded a read tier would apply the
// deployed policy a second time, and double-applied policy is invisible until a
// bead lands in the wrong tier.
func TestWrapperDoesNotReapplyBeadPolicy(t *testing.T) {
	adapters := storebindingtest.ReferenceAdapters(storebindingtest.Wrap(t))
	recorder := &recordingGraphStore{GraphStore: adapters.Graph}
	wrapped, err := storebinding.WrapGraph(recorder, storebinding.ClassWrapping{
		Binding:    wrappedBindingName,
		Capability: storebindingtest.ReferenceCapability,
		CacheReads: true,
	})
	if err != nil {
		t.Fatalf("wrapping the Graph front door: %v", err)
	}

	bead := beads.Bead{Title: "unpoliced", Type: "task", Status: "open", Labels: []string{"keep"}}
	if _, err := wrapped.Create(bead); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(recorder.created) != 1 {
		t.Fatalf("the binding saw %d creates, want 1", len(recorder.created))
	}
	if got := recorder.created[0]; got.Title != bead.Title || got.Type != bead.Type || got.Status != bead.Status || len(got.Labels) != len(bead.Labels) {
		t.Errorf("the binding received %+v, want the caller's own bead %+v", got, bead)
	}
	if len(recorder.storages) != 0 {
		t.Errorf("the wrapper chose storage classes %v for a plain Create; policy is applied below the contract", recorder.storages)
	}

	query := beads.ListQuery{ParentID: "gcg-root"}
	if _, err := wrapped.List(query); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recorder.queries) != 1 {
		t.Fatalf("the binding saw %d list queries, want 1", len(recorder.queries))
	}
	if !reflect.DeepEqual(recorder.queries[0], query) {
		t.Errorf("the binding received query %+v, want the caller's own %+v; the wrapper expanded a read tier", recorder.queries[0], query)
	}

	readyQuery := beads.ReadyQuery{Assignee: "polly"}
	if _, err := wrapped.Ready(readyQuery); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(recorder.ready) != 1 || len(recorder.ready[0]) != 1 || !reflect.DeepEqual(recorder.ready[0][0], readyQuery) {
		t.Errorf("the binding received ready queries %+v, want exactly the caller's own %+v", recorder.ready, readyQuery)
	}
}
