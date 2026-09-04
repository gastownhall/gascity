package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// A due condition order is a wake demand, not a request for a turn in the
// rotation.
//
// The per-tick dispatch budget exists to spread clock-driven maintenance
// (cooldown/cron/event) across ticks. On maintainer-city — ~40 enabled orders,
// budget 4, a ~20 min tick — the sweeps were due at every tick and spent the
// budget before the rotation ever reached pr-merge-queue, whose condition check
// reports "a merge bead is ready right now". It went 11 h without a dispatch
// with no gate error, no suppression event and nothing in the journal to show
// for it (ga-unaz7). These tests pin the two halves of the fix: a due condition
// order fires regardless of the budget, and it still answers to every gate that
// bounds it.

// conditionBudgetOrder is the condition-triggered exec order under test, whose
// check verdict the caller chooses: "true" passes, "false" fails.
func conditionBudgetOrder(check string) orders.Order {
	return orders.Order{Name: "queue-c", Trigger: "condition", Check: check, Exec: "true"}
}

// cooldownBudgetOrder is a cooldown exec order that has never run, so it is due
// on the first tick it is offered.
func cooldownBudgetOrder(name string) orders.Order {
	return orders.Order{Name: name, Trigger: "cooldown", Interval: "1h", Exec: "true"}
}

// newConditionBudgetDispatcher builds a single-store dispatcher over aa with a
// faked exec runner and the given per-tick budget, and returns it with the
// buffer its dispatch errors land in.
func newConditionBudgetDispatcher(t *testing.T, aa []orders.Order, budget int) (*memoryOrderDispatcher, *bytes.Buffer, beads.Store) {
	t.Helper()
	store := beads.NewMemStore()
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	m := ad.(*memoryOrderDispatcher)
	m.maxDispatchesPerTick = budget
	var stderr bytes.Buffer
	m.stderr = lockedStderr(&stderr)
	t.Cleanup(func() { m.dispatchCancel() })
	return m, &stderr, store
}

// runCountFor counts the beads carrying an order's run label.
func runCountFor(t *testing.T, store beads.Store, scoped string) int {
	t.Helper()
	return len(trackingBeads(t, store, orders.RunLabel(scoped)))
}

// TestDispatchFiresDueConditionOrderOutsideTheRotationBudget is the defect:
// with the budget spent by cooldown sweeps ahead of it in the rotation, a
// condition order whose check has just passed must still fire this tick.
func TestDispatchFiresDueConditionOrderOutsideTheRotationBudget(t *testing.T) {
	aa := []orders.Order{
		cooldownBudgetOrder("sweep-a"),
		cooldownBudgetOrder("sweep-b"),
		conditionBudgetOrder("true"),
	}
	m, _, store := newConditionBudgetDispatcher(t, aa, 1)
	cityPath := t.TempDir()

	m.dispatch(context.Background(), cityPath, time.Now())
	drainOrderDispatch(t, m)

	if got := runCountFor(t, store, "queue-c"); got != 1 {
		t.Fatalf("condition order runs after a tick whose budget the sweeps spent = %d, want 1 — the budget starved a due condition order", got)
	}
	if got := runCountFor(t, store, "sweep-a"); got != 1 {
		t.Fatalf("sweep-a runs = %d, want 1: the budgeted order at the head of the rotation must still fire", got)
	}
	// Control: the budget still binds the orders it is for. Without this the
	// assertion above would also pass for a change that simply deleted it.
	if got := runCountFor(t, store, "sweep-b"); got != 0 {
		t.Fatalf("sweep-b runs = %d, want 0: a budget of 1 must defer the second due cooldown order", got)
	}

	// And the rotation still advances, so the deferred sweep fires next tick.
	m.dispatch(context.Background(), cityPath, time.Now().Add(time.Second))
	drainOrderDispatch(t, m)
	if got := runCountFor(t, store, "sweep-b"); got != 1 {
		t.Fatalf("sweep-b runs after the second tick = %d, want 1: the rotation cursor no longer advances", got)
	}
}

// TestDispatchConditionOrderStillHonoursOpenTrackingGate: unbudgeted is not
// ungated. A dispatch already recorded in flight suppresses the next one.
func TestDispatchConditionOrderStillHonoursOpenTrackingGate(t *testing.T) {
	a := conditionBudgetOrder("true")
	m, _, store := newConditionBudgetDispatcher(t, []orders.Order{a}, 1)

	front := orders.NewStore(beads.OrdersStore{Store: store})
	if _, err := front.CreateRun(a.ScopedName(), orders.RunOpts{}); err != nil {
		t.Fatalf("seed open tracking bead: %v", err)
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	drainOrderDispatch(t, m)

	if got := runCountFor(t, store, a.ScopedName()); got != 1 {
		t.Fatalf("runs = %d, want 1 (the seeded open tracking bead only): the open-tracking gate stopped holding a condition order", got)
	}
}

// TestDispatchConditionOrderStillHonoursOpenWorkGate: an open wisp root
// carrying the order's run label means the previous dispatch's work is still
// moving, and that suppresses the fire whether or not a budget was consulted.
func TestDispatchConditionOrderStillHonoursOpenWorkGate(t *testing.T) {
	a := conditionBudgetOrder("true")
	m, _, store := newConditionBudgetDispatcher(t, []orders.Order{a}, 1)

	front := orders.NewStore(beads.OrdersStore{Store: store})
	run, err := front.CreateRun(a.ScopedName(), orders.RunOpts{})
	if err != nil {
		t.Fatalf("seed tracking bead: %v", err)
	}
	closed := "closed"
	if err := store.Update(run.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close tracking bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "wisp",
		Labels:   []string{orders.RunLabel(a.ScopedName())},
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
	}); err != nil {
		t.Fatalf("seed open wisp root: %v", err)
	}

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	drainOrderDispatch(t, m)

	if got := runCountFor(t, store, a.ScopedName()); got != 2 {
		t.Fatalf("beads carrying the run label = %d, want 2 (the closed tracking bead and the open wisp): the open-work gate stopped holding a condition order", got)
	}
}

// TestDispatchConditionOrderWithFailingCheckDoesNotFire is the control on the
// unbudgeted pass: it is selected by the check's verdict, not by the trigger
// type.
func TestDispatchConditionOrderWithFailingCheckDoesNotFire(t *testing.T) {
	a := conditionBudgetOrder("false")
	m, _, store := newConditionBudgetDispatcher(t, []orders.Order{a}, 1)

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	drainOrderDispatch(t, m)

	if got := runCountFor(t, store, a.ScopedName()); got != 0 {
		t.Fatalf("runs = %d, want 0: a condition order whose check fails must not fire", got)
	}
}

// TestDispatchBudgetExhaustionLogsDeferredDueOrders: the starvation this whole
// change is about was invisible. A tick that stops on its budget says so, and
// names what it did not reach.
func TestDispatchBudgetExhaustionLogsDeferredDueOrders(t *testing.T) {
	aa := []orders.Order{cooldownBudgetOrder("sweep-a"), cooldownBudgetOrder("sweep-b")}
	m, stderr, _ := newConditionBudgetDispatcher(t, aa, 1)

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	drainOrderDispatch(t, m)

	logged := stderr.String()
	if !strings.Contains(logged, "per-tick budget 1 spent") {
		t.Fatalf("dispatch stderr = %q, want a line reporting the spent per-tick budget", logged)
	}
	if !strings.Contains(logged, "sweep-b") {
		t.Fatalf("dispatch stderr = %q, want the deferred order named", logged)
	}
}

// TestDispatchLogsNoDeferralWhenEveryDueOrderFires keeps the line above from
// becoming per-tick noise on a city that is keeping up.
func TestDispatchLogsNoDeferralWhenEveryDueOrderFires(t *testing.T) {
	aa := []orders.Order{cooldownBudgetOrder("sweep-a"), cooldownBudgetOrder("sweep-b")}
	m, stderr, _ := newConditionBudgetDispatcher(t, aa, 4)

	m.dispatch(context.Background(), t.TempDir(), time.Now())
	drainOrderDispatch(t, m)

	if logged := stderr.String(); strings.Contains(logged, "deferred to a later tick") {
		t.Fatalf("dispatch stderr = %q, want no deferral line when the budget covered every due order", logged)
	}
}
