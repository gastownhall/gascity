package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// Shadow debts (DES-NQL §5.2, ga-fsp5w9). The file queue's flock covers
// state.json and nothing else: a transition that owes an item's gc:nudge shadow
// a terminal write records the debt in state.json under the lock (commit), the
// verb writes the shadow after unlocking (pay), and a second short hold records
// the payment (confirm). A debt a payer did not settle, because it crashed, ran
// out of budget or met a failing store, is paid later by the controller
// (runNudgeQueueMaintenanceSweep, the queue's Maintain), so convergence needs
// only the controller.

const (
	// nudgeShadowPayLease is how long a verb that took a debt in its commit
	// has to pay and confirm it before another payer may take it.
	nudgeShadowPayLease = 30 * time.Second
	// nudgeShadowPayBatch bounds how many leftover debts one controller pass
	// takes.
	nudgeShadowPayBatch = 16
	// nudgeControllerPayBudget bounds the payments of one controller pass. The
	// pass runs on the supervisor's dispatch loop or the orders lane, so a
	// hung store must not hold it; the next pass takes what this one did not
	// reach.
	nudgeControllerPayBudget = 5 * time.Second
	// nudgeShadowRetryBase and nudgeShadowRetryMax bound the backoff after a
	// failed payment: base * 2^(attempts-1), capped.
	nudgeShadowRetryBase = 15 * time.Second
	nudgeShadowRetryMax  = 5 * time.Minute
)

// errNoNudgesStoreForDebt is a payment that found no nudges store to write a
// known shadow bead in.
var errNoNudgesStoreForDebt = errors.New("no nudges store is open")

// queuedNudgeHasShadow reports whether item can have a gc:nudge shadow, and so
// owes it a terminal write when it leaves the queue: it names one, or it was
// enqueued while no store opened (paid through Terminalize's find-by-id
// fallback). A deferred submit never had a shadow.
func queuedNudgeHasShadow(item queuedNudge) bool {
	return item.BeadID != "" || item.Source != nudgequeue.SourceSubmit
}

// nudgeShadowLedger collects, during one queue callback run, the debts the run
// took. A callback may run twice (a dry run, then the locked run), so the run
// resets the ledger at its start. A nil ledger records debts untaken.
type nudgeShadowLedger struct {
	now   time.Time
	taken []queuedNudge
}

func newNudgeShadowLedger(now time.Time) *nudgeShadowLedger {
	return &nudgeShadowLedger{now: now}
}

func (l *nudgeShadowLedger) reset() {
	if l != nil {
		l.taken = nil
	}
}

// owe records on item, which the caller is moving out of the serving buckets,
// the terminal record its shadow owes, and takes the debt for this call. An
// item with no shadow owes nothing.
func (l *nudgeShadowLedger) owe(item *queuedNudge, state, reason, commitBoundary string, at time.Time) {
	if !queuedNudgeHasShadow(*item) {
		item.Shadow = nil
		return
	}
	item.Shadow = &nudgequeue.ShadowDebt{
		State:          state,
		Reason:         reason,
		CommitBoundary: commitBoundary,
		At:             at.UTC(),
	}
	l.take(item)
}

// take leases item's debt to this call.
func (l *nudgeShadowLedger) take(item *queuedNudge) {
	if l == nil || item.Shadow == nil {
		return
	}
	debt := *item.Shadow
	debt.NotBefore = l.now.Add(nudgeShadowPayLease).UTC()
	debt.Leased = true
	item.Shadow = &debt
	taken := *item
	taken.Shadow = &debt
	l.taken = append(l.taken, taken)
}

// nudgeDebtPayable reports whether a payer may take item's debt at now.
func nudgeDebtPayable(item queuedNudge, now time.Time) bool {
	return item.Shadow != nil && !item.Shadow.Paid && !item.Shadow.NotBefore.After(now)
}

// nudgeDebtLeased reports whether a payer holds item's debt at now: it took
// the debt, holds a copy, and may be writing the shadow until its lease runs
// out. A debt backing off after a failed payment is not held.
func nudgeDebtLeased(item queuedNudge, now time.Time) bool {
	return item.Shadow != nil && item.Shadow.Leased && !item.Shadow.Paid && item.Shadow.NotBefore.After(now)
}

// takeDueNudgeShadowDebts leases up to limit debts whose lease or backoff has
// run out, oldest bucket first: the dead letters, then the retired items.
func takeDueNudgeShadowDebts(state *nudgeQueueState, ledger *nudgeShadowLedger, limit int) {
	for _, bucket := range [][]queuedNudge{state.Dead, state.Retired} {
		for i := range bucket {
			if len(ledger.taken) >= limit {
				return
			}
			if nudgeDebtPayable(bucket[i], ledger.now) {
				ledger.take(&bucket[i])
			}
		}
	}
}

// nudgeDebtPayment is the result of one payment.
type nudgeDebtPayment struct {
	item    queuedNudge
	reached bool
	err     error
}

// payNudgeShadowDebts writes each taken debt's shadow until deadline. It runs
// outside the queue lock. A Verify debt reads the shadow first and writes only
// if it is not terminal; the others Terminalize, which is idempotent and
// tolerates a missing bead.
func payNudgeShadowDebts(front *nudgequeue.Store, taken []queuedNudge, deadline time.Time) []nudgeDebtPayment {
	payments := make([]nudgeDebtPayment, 0, len(taken))
	for _, item := range taken {
		if time.Now().After(deadline) {
			payments = append(payments, nudgeDebtPayment{item: item})
			continue
		}
		payments = append(payments, nudgeDebtPayment{item: item, reached: true, err: payNudgeShadowDebt(front, item)})
	}
	return payments
}

func payNudgeShadowDebt(front *nudgequeue.Store, item queuedNudge) error {
	debt := item.Shadow
	if front == nil {
		if item.BeadID == "" {
			// Nothing names a shadow and no store can look one up: the
			// write Terminalize would make is a no-op, as it is today.
			return nil
		}
		return errNoNudgesStoreForDebt
	}
	if debt.Verify {
		shadow, ok, err := front.FindIncludingTerminal(item.ID)
		if err != nil {
			return err
		}
		if ok && nudgequeue.IsTerminalState(shadow.State) {
			return nil
		}
	}
	return front.Terminalize(item, debt.State, debt.Reason, debt.CommitBoundary, debt.At)
}

// nudgeDebtKey matches a paid item to its queue entry: an id re-enqueued
// under a new creation time is a different item.
type nudgeDebtKey struct {
	id      string
	created int64
}

func nudgeDebtKeyOf(item queuedNudge) nudgeDebtKey {
	return nudgeDebtKey{id: item.ID, created: item.CreatedAt.UnixNano()}
}

// nudgeShadowRetryDelay is the backoff after a debt's attempts-th failure.
func nudgeShadowRetryDelay(attempts int) time.Duration {
	delay := nudgeShadowRetryBase
	for i := 1; i < attempts && delay < nudgeShadowRetryMax; i++ {
		delay *= 2
	}
	if delay > nudgeShadowRetryMax {
		delay = nudgeShadowRetryMax
	}
	return delay
}

// confirmNudgeShadowDebts records payments in one short hold
// (applyNudgeDebtPayments).
func confirmNudgeShadowDebts(cityPath string, payments []nudgeDebtPayment, now time.Time) error {
	if len(payments) == 0 {
		return nil
	}
	return withNudgeQueueState(cityPath, "confirm_debts", func(state *nudgeQueueState) error {
		applyNudgeDebtPayments(state, payments, now)
		return nil
	})
}

// nudgeDebtLeaseHeld reports whether item's debt is still under the lease the
// payer of paid took. A lease is known by its end, NotBefore: a payer takes a
// debt only once the lease before it has run out, so its lease ends later than
// any lease it took over, and a payer whose lease ran out finds its
// successor's NotBefore on the debt, not its own.
func nudgeDebtLeaseHeld(item, paid queuedNudge) bool {
	return item.Shadow != nil && paid.Shadow != nil && item.Shadow.Leased &&
		item.Shadow.NotBefore.Equal(paid.Shadow.NotBefore)
}

// applyNudgeDebtPayments records payments on state: a paid retired item is
// removed, a paid dead letter is marked paid, a failed debt backs off, and a
// debt the payer did not reach is released. Each ends the payer's lease. A
// payment is recorded only while its payer still holds the lease it took
// (nudgeDebtLeaseHeld): an item that is gone, a debt already paid, and a debt
// another payer took once this payer's lease ran out are skipped. Ending a
// successor's lease would let an enqueue reuse the shadow while the
// successor's write to it is in flight (dropRetiredDebtsOnShadow).
func applyNudgeDebtPayments(state *nudgeQueueState, payments []nudgeDebtPayment, now time.Time) {
	byKey := make(map[nudgeDebtKey]nudgeDebtPayment, len(payments))
	for _, payment := range payments {
		byKey[nudgeDebtKeyOf(payment.item)] = payment
	}
	settle := func(item *queuedNudge) (paid bool) {
		payment, ok := byKey[nudgeDebtKeyOf(*item)]
		if !ok || item.Shadow == nil || item.Shadow.Paid || !nudgeDebtLeaseHeld(*item, payment.item) {
			return false
		}
		debt := *item.Shadow
		debt.Leased = false
		switch {
		case !payment.reached:
			debt.NotBefore = time.Time{}
		case payment.err != nil:
			debt.Attempts++
			debt.LastError = payment.err.Error()
			debt.NotBefore = now.Add(nudgeShadowRetryDelay(debt.Attempts)).UTC()
		default:
			debt.Paid = true
			debt.NotBefore = time.Time{}
			debt.LastError = ""
		}
		item.Shadow = &debt
		return debt.Paid
	}
	for i := range state.Dead {
		settle(&state.Dead[i])
	}
	retired := state.Retired[:0]
	for _, item := range state.Retired {
		if settle(&item) {
			continue
		}
		retired = append(retired, item)
	}
	state.Retired = retired
}

// settleNudgeShadowDebts is the pay and confirm phases a verb runs after its
// commit. It is a seam so a test can drop them, as a crash between the commit
// and the payment would. Tests that replace it must stay serial.
var settleNudgeShadowDebts = payAndConfirmNudgeShadowDebts

// payAndConfirmNudgeShadowDebts pays the debts a verb's commit took and
// confirms them.
// It runs after the queue lock is released, writing with store, or with a store
// it opens (and closes) itself when store is the zero store. A payment that
// fails, and a confirm that fails, leave the debt for a later payer; neither
// undoes the committed transition, so both are warnings, not errors.
//
// Inside a hook the work must end with the time the hook's deadline leaves
// (hookWorkEnd): the commit before it may be an ack, and gc hook run discards
// the output of a child that overruns, which would lose a nudge the ack
// retired. There the tail pays only while that time remains, confirms only if
// it still does, and is left behind when it does not end in time, a store call
// that hangs included. A debt it did not confirm stays leased to it until
// nudgeShadowPayLease runs out, and then the controller pays it. A tail left
// behind outlives this call, and the caller closes store once the call
// returns, so the tail pays through a store it opens and closes itself, never
// through store.
func payAndConfirmNudgeShadowDebts(cityPath string, store beads.NudgesStore, taken []queuedNudge, budget time.Duration, now time.Time) {
	if len(taken) == 0 {
		return
	}
	payBy := time.Now().Add(budget)
	end, inHook := hookWorkEnd()
	if !inHook {
		payAndConfirmNudgeShadowDebtsBy(cityPath, store, taken, payBy, now, nil)
		return
	}
	if !time.Now().Before(end) {
		warnNudgeShadowDebtsLeft(len(taken))
		return
	}
	if end.Before(payBy) {
		payBy = end
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		payAndConfirmNudgeShadowDebtsBy(cityPath, beads.NudgesStore{}, taken, payBy, now, hookDeadlineStillLeavesTime)
	}()
	timer := time.NewTimer(time.Until(end))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		warnNudgeShadowDebtsLeft(len(taken))
		if nudgeHookTailAbandoned != nil {
			nudgeHookTailAbandoned(done)
		}
	}
}

// nudgeHookTailAbandoned is a test seam called with the tail's done channel
// when payAndConfirmNudgeShadowDebts leaves a tail behind at a hook's
// deadline, so a test can wait it out. Nil in production. Tests that set it
// must stay serial.
var nudgeHookTailAbandoned func(done <-chan struct{})

// payAndConfirmNudgeShadowDebtsBy pays taken until payBy and confirms the
// payments. A non-nil mayConfirm is asked before the confirm; when it says
// no, the payments go unrecorded and the debts stay leased.
func payAndConfirmNudgeShadowDebtsBy(cityPath string, store beads.NudgesStore, taken []queuedNudge, payBy, now time.Time, mayConfirm func() bool) {
	var front *nudgequeue.Store
	if store.Store != nil {
		front = nudgeFrontDoor(store)
	} else {
		maint := nudgeMaintenanceStore{cityPath: cityPath}
		defer maint.close() //nolint:errcheck // best-effort
		front = maint.frontDoor()
	}
	payments := payNudgeShadowDebts(front, taken, payBy)
	for _, payment := range payments {
		if payment.reached && payment.err != nil {
			warnNudgeShadowDebt(payment.item, payment.err)
		}
	}
	if mayConfirm != nil && !mayConfirm() {
		return
	}
	if err := confirmNudgeShadowDebts(cityPath, payments, now); err != nil {
		warnNudgeShadowDebt(queuedNudge{}, fmt.Errorf("recording shadow payments: %w", err))
	}
}

// warnNudgeShadowDebtsLeft says that n debts were left for the controller at a
// hook's deadline.
func warnNudgeShadowDebtsLeft(n int) {
	warnNudgeShadowDebt(queuedNudge{}, fmt.Errorf("hook deadline: leaving %d nudge shadow write(s) to the controller", n))
}

func warnNudgeShadowDebt(item queuedNudge, err error) {
	if nudgeWarningWriter == nil {
		return
	}
	if item.ID == "" {
		fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: %v\n", err) //nolint:errcheck
		return
	}
	fmt.Fprintf(nudgeWarningWriter, "gc nudge: warning: marking nudge %q %s: %v\n", item.ID, item.Shadow.State, err) //nolint:errcheck
}

// verifyDebtForLegacyDeadLetter gives a dead letter an older binary wrote, one
// that names a shadow but carries no debt, a one-time Verify debt, left for the
// controller to pay.
func verifyDebtForLegacyDeadLetter(item *queuedNudge, now time.Time) {
	if item.Shadow != nil || item.BeadID == "" {
		return
	}
	reason := strings.TrimSpace(item.LastError)
	if reason == "" {
		reason = "failed"
	}
	at := now
	if !item.DeadAt.IsZero() {
		at = item.DeadAt
	}
	item.Shadow = &nudgequeue.ShadowDebt{
		State:  terminalStateForDeadQueuedNudge(*item),
		Reason: reason,
		At:     at.UTC(),
		Verify: true,
	}
}
