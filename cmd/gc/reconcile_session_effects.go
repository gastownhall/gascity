package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"time"
)

// The session keys' effect executor (CONTRACT C1.9, P4 spec §3.4): every
// provider mutation a session key decides runs here, never inline in a
// worker, so a three-minute start cannot hold a reload barrier or trip the
// stuck-reconcile alert. Each effect has a deadline. Whatever the effect does,
// it is settled exactly once, by the deadline at the latest, so every issued
// ledger entry reaches committed or failed in bounded time. Then the allocator
// is woken (C5.12) and its key enqueued: urgently after a success, so a
// completion never waits behind its key's backoff, and with backoff after a
// failure or panic, so a failing effect is not retried hot (C1.4b).

// sessionEffectKind says what an effect does to its runtime. Only starts are
// told apart: a provider swap waits for them (C4.4 step 3).
type sessionEffectKind uint8

const (
	effectStart sessionEffectKind = iota + 1
	effectStop
	effectOther
)

// effectCancelBound is how long waitStarts waits for canceled starts to
// return (C4.4 step 3).
const effectCancelBound = 10 * time.Second

// errEffectBusy refuses a second effect for a key, and errEffectsClosed any
// effect once the executor stops.
var (
	errEffectBusy    = errors.New("session effect: key has an effect in flight")
	errEffectsClosed = errors.New("session effect: executor stopped")
)

// sessionEffect is one effect. Run performs it under a context that ends at
// Deadline. Settle records its outcome (ledger Commit or Fail, a ticket's
// Resolve): with Run's error, a recovered panic, or the context's error when
// the deadline or a cancel comes first. Run must check its context before any
// write that would commit a late result.
type sessionEffect struct {
	Kind     sessionEffectKind
	Deadline time.Time
	Run      func(ctx context.Context) error
	Settle   func(err error)
}

// inflightEffect is one submitted effect.
type inflightEffect struct {
	kind     sessionEffectKind
	deadline time.Time
	cancel   context.CancelFunc
	returned chan struct{} // closed when Run returns, which may be after the settle
}

// effectExecutor runs session effects, at most one per key (C5.5: while one
// is in flight its key's decide is read-only).
type effectExecutor struct {
	// done runs after each settle, with the error the effect settled with:
	// enqueue the key and wake the allocator.
	done   func(k rowKey, err error)
	stderr io.Writer

	mu       sync.Mutex
	ctx      context.Context // ends at stop's deadline, not at the workers' cancel
	cancel   context.CancelFunc
	closed   bool
	inflight map[rowKey]*inflightEffect // until settled
	// running holds every effect whose Run has not returned, including one
	// abandoned at its deadline.
	running map[*inflightEffect]bool
	wg      sync.WaitGroup
}

func newEffectExecutor(done func(rowKey, error), stderr io.Writer) *effectExecutor {
	ctx, cancel := context.WithCancel(context.Background())
	return &effectExecutor{
		done: done, stderr: stderr, ctx: ctx, cancel: cancel,
		inflight: make(map[rowKey]*inflightEffect), running: make(map[*inflightEffect]bool),
	}
}

// inFlight returns k's unsettled effect's deadline.
func (x *effectExecutor) inFlight(k rowKey) (time.Time, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if e := x.inflight[k]; e != nil {
		return e.deadline, true
	}
	return time.Time{}, false
}

// submit starts e for k. It refuses, running nothing, while k has an effect
// in flight or after stop; the caller then settles whatever it issued.
func (x *effectExecutor) submit(k rowKey, e sessionEffect) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	switch {
	case x.closed:
		return errEffectsClosed
	case x.inflight[k] != nil:
		return errEffectBusy
	}
	ctx, cancel := context.WithDeadline(x.ctx, e.Deadline)
	f := &inflightEffect{kind: e.Kind, deadline: e.Deadline, cancel: cancel, returned: make(chan struct{})}
	x.inflight[k], x.running[f] = f, true
	x.wg.Add(1)
	go x.run(ctx, k, e, f)
	return nil
}

// run performs e and settles it when Run returns or ctx ends, whichever is
// first; a result that is ready when ctx ends wins. A Run that ignores its
// context is abandoned at the deadline; it keeps any name lock it holds, so it
// still serializes its runtime name. A panic in Run or Settle is recovered and
// logged: one bad effect never takes the process down.
func (x *effectExecutor) run(ctx context.Context, k rowKey, e sessionEffect, f *inflightEffect) {
	defer x.wg.Done()
	defer f.cancel()
	result := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(x.stderr, "v2 reconciler: effect for %s/%s panicked: %v\n%s", k.Leg, k.ID, r, debug.Stack()) //nolint:errcheck // best-effort stderr
				result <- fmt.Errorf("session effect for %s/%s panicked: %v", k.Leg, k.ID, r)
			}
			x.mu.Lock()
			delete(x.running, f)
			x.mu.Unlock()
			close(f.returned)
		}()
		result <- e.Run(ctx)
	}()
	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		select {
		case err = <-result:
		default:
			err = ctx.Err()
			fmt.Fprintf(x.stderr, "v2 reconciler: effect for %s/%s still running when its context ended; settled as %v\n", k.Leg, k.ID, err) //nolint:errcheck // best-effort stderr
		}
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(x.stderr, "v2 reconciler: settling effect for %s/%s panicked: %v\n", k.Leg, k.ID, r) //nolint:errcheck // best-effort stderr
			}
		}()
		e.Settle(err)
	}()
	x.mu.Lock()
	delete(x.inflight, k)
	x.mu.Unlock()
	x.done(k, err)
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
		if f.kind == effectStart {
			starts = append(starts, f)
		}
	}
	x.mu.Unlock()
	if returnedWithin(starts, bound) {
		return nil
	}
	for _, f := range starts {
		f.cancel()
	}
	if returnedWithin(starts, effectCancelBound) {
		return nil
	}
	return fmt.Errorf("v2 reconciler: in-flight session starts still running %s after cancel", effectCancelBound)
}

// returnedWithin reports whether every effect's Run returns within d.
func returnedWithin(effects []*inflightEffect, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for _, f := range effects {
		select {
		case <-f.returned:
		case <-t.C:
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
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-joined:
	case <-t.C:
		fmt.Fprintln(x.stderr, "v2 reconciler: session effects still running at the shutdown deadline; canceling them") //nolint:errcheck // best-effort stderr
	}
	x.cancel()
}
