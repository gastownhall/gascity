package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// postStartObservationBound is the longest runPreparedStartCandidate waits for
// any one observation it makes after provider.Start has returned: the
// ErrStateSync recovery, the ErrSessionExists collision check, the
// post-stability liveness check and the pane read of a resume that check finds
// dead (postStartDeathPane). The call under the wait can hang on a wedged
// tmux subprocess or a stalled store and nothing cancels it, so on expiry the
// wait is abandoned instead of holding the async start slot. An abandoned
// observation defers the start as runtime-unavailable (SESSION-RUNTIME-009);
// an abandoned pane read reads empty, and the death the liveness check already
// found still counts.
const postStartObservationBound = 10 * time.Second

// startFailureProbeBudget is the total time a failed start spends on the two
// probes it makes to classify the failure, the rate-limit screen peek and the
// pending-create identity reads, together. A peek that does not answer reads as
// "no rate-limit screen". A read that does not answer leaves the attribution
// unknown, so the start defers instead of rolling the create back.
const startFailureProbeBudget = 10 * time.Second

// commitPhaseBound is the longest the async start goroutine waits for the
// commit and settle phase that follows runPreparedStartCandidate while it holds
// its start slot. The phase reads and writes the session store and can stop or
// probe the runtime, none of it cancellable, so on expiry the wait is abandoned
// and the slot is released (SESSION-RUNTIME-017). The bound covers the whole
// phase, so it is wider than one post-start observation.
const commitPhaseBound = 30 * time.Second

// postStartObservationKey names what ObserveBounded keeps at one in-flight
// observation: one session in one city. The city is part of it because two
// cities run sessions of the same name, and one city's wedged runtime must not
// defer the other's. A path and a session name never contain NUL, so the
// joined key is unambiguous.
func postStartObservationKey(cityPath, sessionName string) string {
	return cityPath + "\x00" + sessionName
}

// observeSessionBounded runs observe, an observation of the session sessionName
// in cityPath, for no longer than ctx allows. It goes through
// worker.ObserveBounded, so at most one observation of the session is in flight
// and an expired wait is an error wrapping runtime.ErrRuntimeUnavailable. A
// blank session name is not bounded: it never reaches the runtime (the
// unbounded observation answers runtime.ErrSessionNotFound without any I/O), so
// there is no call to wedge.
func observeSessionBounded[T any](ctx context.Context, cityPath, sessionName string, observe func() (T, error)) (T, error) {
	if strings.TrimSpace(sessionName) == "" {
		return observe()
	}
	return worker.ObserveBounded(ctx, postStartObservationKey(cityPath, sessionName), func(context.Context) (T, error) {
		return observe()
	})
}

// workerObserveSessionTargetBounded is
// workerObserveSessionTargetWithRuntimeHintsWithConfig held to
// postStartObservationBound. The bound is derived from ctx, so it also ends
// when the controller does, and it covers the handle's store reads as well as
// the runtime probes behind them.
func workerObserveSessionTargetBounded(ctx context.Context, cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.LiveObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, postStartObservationBound)
	defer cancel()
	return observeSessionBounded(ctx, cityPath, target, func() (worker.LiveObservation, error) {
		return workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	})
}

// observeRuntimeProviderLivenessBounded is observeRuntimeProviderLiveness held
// to postStartObservationBound, for a start with no session bead to resolve a
// handle from.
func observeRuntimeProviderLivenessBounded(ctx context.Context, cityPath string, sp runtime.Provider, name string, processNames []string) (running bool, alive bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, postStartObservationBound)
	defer cancel()
	type liveness struct{ running, alive bool }
	got, err := observeSessionBounded(ctx, cityPath, name, func() (liveness, error) {
		isRunning, isAlive, observeErr := observeRuntimeProviderLiveness(sp, name, processNames)
		return liveness{running: isRunning, alive: isAlive}, observeErr
	})
	return got.running, got.alive, err
}

// readPendingCreateIdentityBounded is readPendingCreateIdentity with every
// metadata read held to ctx. A read that does not answer in time is
// unverifiable like any other failed read, so the attribution comes out
// unknown unless the reads that did answer already decide it.
func readPendingCreateIdentityBounded(ctx context.Context, cityPath string, info sessionpkg.Info, sessionName string, sp runtime.Provider) pendingCreateIdentity {
	return readPendingCreateIdentityVia(info, sp, func(key string) (string, error) {
		return observeSessionBounded(ctx, cityPath, sessionName, func() (string, error) {
			return sp.GetMeta(sessionName, key)
		})
	})
}

// commitAsyncStartBounded is commitAsyncStartResultWithContext held to
// commitPhaseBound, under the same per-session key as the post-start
// observations. It returns whether the commit succeeded. When the commit does
// not answer in time, or an observation of the session is already outstanding
// so it is refused without running, it logs commit_unavailable and returns
// false; the waiter writes nothing to the session, and a commit that was
// started keeps running on the controller's ctx and finishes on its own. The
// pending claim and its lease stay as they are and the reconciler's repair
// paths pick the row up when the lease ages out.
//
// dropLease releases the runtime lease the start holds, and this function owns
// calling it exactly once. Releasing a lease that carries a row record reads
// and writes the store, so it is never run on a path that gave up on the store:
//   - the commit finished: dropLease runs before this returns, as it always did;
//   - the commit was refused: nothing ran and nothing will, so dropLease runs
//     detached;
//   - the commit was abandoned: the commit's own goroutine runs it when the
//     commit returns, so a commit that lands late does so under the lease that
//     fences it and is not refused as stale.
//
// The wait ends on the bound only, never on ctx: controller shutdown must not
// abandon a commit, whose shutdown branch settles the row on ctx. The commit
// itself runs on ctx unchanged.
func commitAsyncStartBounded(
	ctx context.Context,
	cityPath string,
	result startResult,
	sp runtime.Provider,
	store beads.Store,
	clk clock.Clock,
	rec events.Recorder,
	wave int,
	stdout, stderr io.Writer,
	trace *sessionReconcilerTraceCycle,
	dropLease func(),
) (committed bool) {
	name := result.prepared.candidate.name()
	template := result.prepared.candidate.tp.TemplateName
	// The commit owns result from here on. The waiter keeps its own copies of
	// the two fields it logs, so the two sides share no state after expiry.
	started := result.started
	phases := result.phases
	base := ctx
	if base == nil {
		base = context.Background()
	}
	if dropLease == nil {
		dropLease = func() {}
	}
	// commitRunning is the commit's side of the hand-off: running until the
	// commit returns, then finished, or abandoned once the waiter gave up on it.
	const (
		commitRunning int32 = iota
		commitFinished
		commitAbandoned
	)
	var phase atomic.Int32
	waiterDone := false
	defer func() {
		// A panic out of the wait (the commit's own is recovered inside it)
		// must not strand the lease.
		if !waiterDone {
			dropLease()
		}
	}()
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(base), commitPhaseBound)
	defer cancel()
	committed, err := observeSessionBounded(waitCtx, cityPath, name, func() (bool, error) {
		ok := commitAsyncStartResultWithContext(ctx, result, sp, store, clk, rec, wave, stdout, stderr, trace)
		if !phase.CompareAndSwap(commitRunning, commitFinished) {
			// The waiter gave up while this ran: the lease is this goroutine's.
			dropLease()
		}
		return ok, nil
	})
	waiterDone = true
	switch {
	case err == nil:
		dropLease()
		return committed
	case errors.Is(err, worker.ErrObservationOutstanding):
		go dropLease()
	case !phase.CompareAndSwap(commitRunning, commitAbandoned):
		// The commit returned in the instant the wait expired: it did not
		// abandon the lease, so the waiter, whose commit just answered the
		// store, takes it down.
		dropLease()
		logLifecycleOutcome(stderr, "start", wave, name, template, "commit_unavailable", started, time.Now(), err, phases)
		return false
	}
	logLifecycleOutcome(stderr, "start", wave, name, template, "commit_unavailable", started, time.Now(), err, phases)
	return false
}
