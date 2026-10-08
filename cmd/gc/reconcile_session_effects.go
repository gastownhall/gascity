package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// The planner's effect executor (CONTRACT v5 P3): every provider mutation and
// row write the planner admits runs here, never on the planner goroutine, so
// a three-minute start cannot hold up a pass. Each effect runs under the
// deadline its intent carries (admit sets it, P3). Whatever the effect does,
// it settles exactly once, by the deadline at the latest: the executor posts
// one settlement, which the planner drains into its in-flight map, backoff
// table and event recorder (P1). The clock and the goroutine spawn are
// injected, so tests and the simulator (D1a) control both.

// effectCancelBound is how long waitStarts waits for canceled starts to
// return (C4.4 step 3).
const effectCancelBound = 10 * time.Second

// Effect causes the executor sets: a Run that outlived its deadline (or the
// shutdown deadline) or panicked, and an intent whose kind has no registered
// effect. A start or create submitted while starts are closed settles with
// causeSwapPause (P7; no backoff).
const (
	causeDeadline = "deadline"
	causePanic    = "panic"
	causeNoEffect = "no-effect"
)

// errEffectBusy refuses a second effect for a key, and errEffectsClosed any
// effect once the executor stops.
var (
	errEffectBusy    = errors.New("session effect: key has an effect in flight")
	errEffectsClosed = errors.New("session effect: executor stopped")
)

// sessionEffect is one effect. Run performs it under a context that ends at
// Deadline (a real deadline: ctx.Err() reads DeadlineExceeded then) and
// returns its settlement; the executor stamps the key, Kind, Reason, Seq
// and Token on it. Run must check its context before any write that would
// commit a late result.
type sessionEffect struct {
	Kind   string // the intent kind; a provider swap waits for intentStart
	Reason string // the intent's reason, for the pass record
	Seq    uint64 // the in-flight entry's, echoed in the settlement
	// Token is a create's instance token: its single-flight key, since it
	// has no row yet, and on every settlement, so its entry clears (P5).
	Token string
	// Finalize marks the stop verb's finalize: every refused or failed
	// settlement it posts carries causeFinalizePrefix, so its own refusals
	// back it off (P4).
	Finalize bool
	Deadline time.Time
	Run      func(ctx context.Context) settlement
}

// finalizeCause is s's cause as a finalize records it: a refusal or failure
// carries causeFinalizePrefix once.
func finalizeCause(s settlement) string {
	if (s.Outcome != settledRefused && s.Outcome != settledFailed) || strings.HasPrefix(s.Cause, causeFinalizePrefix) {
		return s.Cause
	}
	return causeFinalizePrefix + s.Cause
}

// inflightEffect is one submitted effect.
type inflightEffect struct {
	kind     string
	deadline time.Time
	cancel   context.CancelFunc
	returned chan struct{} // closed when Run returns, which may be after the settle
}

// effectKey is an effect's single-flight key: its row, or a create's token.
type effectKey struct {
	row   rowKey
	token string
}

// effectExecutor runs session effects, at most one per key (C5.5: while one
// is in flight its key's decide is read-only).
type effectExecutor struct {
	// post receives every settlement, after its key leaves inflight. It must
	// not block: the planner's settlement queue.
	post   func(settlement)
	clock  plannerClock
	spawn  func(func()) // go f() in production
	stderr io.Writer

	mu           sync.Mutex
	ctx          context.Context // ends at stop's deadline, not at the workers' cancel
	cancel       context.CancelFunc
	closed       bool
	startsClosed bool                          // a provider swap is in progress (P7)
	inflight     map[effectKey]*inflightEffect // until settled
	// running holds every effect whose Run has not returned, including one
	// abandoned at its deadline.
	running map[*inflightEffect]bool
	wg      sync.WaitGroup
}

func newEffectExecutor(post func(settlement), stderr io.Writer) *effectExecutor {
	ctx, cancel := context.WithCancel(context.Background())
	return &effectExecutor{
		post: post, clock: realPlannerClock{}, spawn: func(f func()) { go f() }, stderr: stderr, ctx: ctx, cancel: cancel,
		inflight: make(map[effectKey]*inflightEffect), running: make(map[*inflightEffect]bool),
	}
}

// inFlight reports whether k has an unsettled effect.
func (x *effectExecutor) inFlight(k rowKey) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.inflight[effectKey{row: k}] != nil
}

// submitIntent submits the registered effect of it for one pass, under the
// in-flight entry's seq. The effect is built inside Run, off the planner
// goroutine. The pass submits registered kinds only; a kind with no
// registered effect settles refused with cause no-effect (a finalize's with
// the finalize prefix, as run adds it).
func (x *effectExecutor) submitIntent(p *effectPass, it intent, seq uint64) error {
	build := effectRegistry[it.Kind]
	run := func(ctx context.Context) settlement {
		if build == nil {
			return settlement{Outcome: settledRefused, Cause: causeNoEffect}
		}
		return build(p, it)(ctx)
	}
	return x.submit(it.Key, sessionEffect{Kind: it.Kind, Reason: it.Reason, Seq: seq, Token: it.Create.Token, Finalize: it.Finalize, Deadline: it.Deadline, Run: run})
}

// submit starts e for k, or for e's token when it has one (a create). It
// refuses, running nothing and posting nothing, while that key has an effect
// in flight or after stop: the caller then settles the in-flight entry it
// added (planner.submit does). A start or create submitted while starts are
// closed runs nothing and settles refused with cause swap-pause.
func (x *effectExecutor) submit(k rowKey, e sessionEffect) error {
	key := effectKey{row: k, token: e.Token}
	x.mu.Lock()
	switch {
	case x.closed:
		x.mu.Unlock()
		return errEffectsClosed
	case x.inflight[key] != nil:
		x.mu.Unlock()
		return errEffectBusy
	case x.startsClosed && (e.Kind == intentStart || e.Kind == intentCreate):
		x.mu.Unlock()
		x.post(settlement{Key: k, Kind: e.Kind, Seq: e.Seq, Token: e.Token, Outcome: settledRefused, Cause: causeSwapPause})
		return nil
	}
	ctx, cancel := context.WithCancel(x.ctx)
	f := &inflightEffect{kind: e.Kind, deadline: e.Deadline, cancel: cancel, returned: make(chan struct{})}
	x.inflight[key], x.running[f] = f, true
	x.wg.Add(1)
	x.mu.Unlock()
	x.spawn(func() { x.run(ctx, key, e, f) })
	return nil
}

// closeStarts and openStarts bracket a provider swap (P7): admission already
// defers starts and creates while the planner's pause is set, and this
// refuses one admitted before the pause but submitted after it. A start
// submitted before it is running, so waitStarts waits for it.
func (x *effectExecutor) closeStarts() { x.setStartsClosed(true) }
func (x *effectExecutor) openStarts()  { x.setStartsClosed(false) }

func (x *effectExecutor) setStartsClosed(closed bool) {
	x.mu.Lock()
	x.startsClosed = closed
	x.mu.Unlock()
}

// run performs e and settles it when Run returns or its deadline or the
// shutdown deadline comes, whichever is first; a result that is ready then
// wins. A Run that ignores its context is abandoned at the deadline; it keeps
// any name lock it holds, so it still serializes its runtime name, and if it
// lands later its event is still posted, alone. An abandoned create settles
// ambiguous, never failed: its row write may have begun (P5). A panic in Run
// is recovered and logged, and settles failed: one bad effect never takes
// the process down.
func (x *effectExecutor) run(base context.Context, key effectKey, e sessionEffect, f *inflightEffect) {
	k := key.row
	defer x.wg.Done()
	// Run's context is released when Run returns, not at the settlement, so
	// an effect abandoned at its deadline sees DeadlineExceeded.
	ctx, stopDeadline := x.clock.WithDeadline(base, e.Deadline)
	result := make(chan settlement, 1)
	x.spawn(func() {
		defer f.cancel()
		defer stopDeadline()
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(x.stderr, "v2 reconciler: effect for %s/%s panicked: %v\n%s", k.Leg, k.ID, r, debug.Stack()) //nolint:errcheck // best-effort stderr
				result <- settlement{Outcome: settledFailed, Cause: causePanic, Err: fmt.Errorf("session effect for %s/%s panicked: %v", k.Leg, k.ID, r)}
			}
			x.mu.Lock()
			delete(x.running, f)
			x.mu.Unlock()
			close(f.returned)
		}()
		result <- e.Run(ctx)
	})
	timer := x.clock.NewTimer(e.Deadline.Sub(x.clock.Now()))
	defer timer.Stop()
	var s settlement
	returned := false
	select {
	case s = <-result:
		returned = true
	case <-timer.C():
	case <-base.Done():
	}
	if !returned {
		select {
		case s = <-result:
		default:
			err := base.Err()
			if err == nil {
				err = context.DeadlineExceeded
			}
			s = settlement{Outcome: settledFailed, Cause: causeDeadline, Err: err}
			if e.Kind == intentCreate {
				s.Outcome = settledAmbiguous
			}
			fmt.Fprintf(x.stderr, "v2 reconciler: effect for %s/%s still running when its context ended; settled as %v\n", k.Leg, k.ID, s.Err) //nolint:errcheck // best-effort stderr
			x.spawn(func() {
				if late := <-result; late.Event != nil {
					x.post(settlement{Event: late.Event})
				}
			})
		}
	}
	s.Key, s.Kind, s.Reason, s.Seq, s.Token = k, e.Kind, e.Reason, e.Seq, e.Token
	if e.Finalize {
		s.Cause = finalizeCause(s)
	}
	x.mu.Lock()
	delete(x.inflight, key)
	x.mu.Unlock()
	x.post(s)
}

// waitStarts waits until every start running when it is called has
// returned from its provider calls, a start abandoned at its deadline
// included (CONTRACT C4.4 step 3: a provider swap must not list the old
// provider's sessions while a start may still create one). Past bound it
// cancels them and waits at most effectCancelBound more, so a hung provider
// cannot wedge the reload; a start still running then is an error, and the
// reload aborts.
func (x *effectExecutor) waitStarts(bound time.Duration) error {
	x.mu.Lock()
	var starts []*inflightEffect
	for f := range x.running {
		if f.kind == intentStart {
			starts = append(starts, f)
		}
	}
	x.mu.Unlock()
	if x.returnedWithin(starts, bound) {
		return nil
	}
	for _, f := range starts {
		f.cancel()
	}
	if x.returnedWithin(starts, effectCancelBound) {
		return nil
	}
	return fmt.Errorf("v2 reconciler: in-flight session starts still running %s after cancel", effectCancelBound)
}

// returnedWithin reports whether every effect's Run returns within d.
func (x *effectExecutor) returnedWithin(effects []*inflightEffect, d time.Duration) bool {
	t := x.clock.NewTimer(d)
	defer t.Stop()
	for _, f := range effects {
		select {
		case <-f.returned:
		case <-t.C():
			return false
		}
	}
	return true
}

// close stops admission: every later submit is refused. v2's stop closes it
// before joining its workers, so a worker still running submits nothing
// (C1.8).
func (x *effectExecutor) close() {
	x.mu.Lock()
	x.closed = true
	x.mu.Unlock()
}

// stop closes admission and waits for the effects in flight until deadline,
// the shutdown deadline v2's stop shares with its workers (P4 F15). At the
// deadline it cancels every effect's context, which settles each at once.
func (x *effectExecutor) stop(deadline time.Time) {
	x.close()
	joined := make(chan struct{})
	go func() {
		x.wg.Wait()
		close(joined)
	}()
	t := x.clock.NewTimer(deadline.Sub(x.clock.Now()))
	defer t.Stop()
	select {
	case <-joined:
	case <-t.C():
		fmt.Fprintln(x.stderr, "v2 reconciler: session effects still running at the shutdown deadline; canceling them") //nolint:errcheck // best-effort stderr
	}
	x.cancel()
}
