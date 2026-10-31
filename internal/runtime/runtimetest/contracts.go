package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// contractSessionName is the session the Stop contract cases target. No
// fixture ever starts it, so on a responsive provider it is a missing session.
const contractSessionName = "never-started-contract-session"

// defaultIdleWaitTimeout bounds each RUNTIME-CONTRACT-016 wait when the
// fixtures set none: long enough for a polling provider to take several
// readings, short enough to keep the suite fast.
const defaultIdleWaitTimeout = 600 * time.Millisecond

// idleWaitBoundSlack is how far past its timeout a wait may return before the
// suite reports the timeout as no hard bound: process spawn and one in-flight
// observation, not a second wait.
const idleWaitBoundSlack = 3 * time.Second

// StopFixtures supplies providers for the RUNTIME-CONTRACT-003 cases: Stop is
// nil for a session a responsive provider does not have, and an error when the
// provider cannot observe its inventory at all. A nil field skips its cases,
// so a provider whose test environment cannot build that state opts out.
type StopFixtures struct {
	// Responsive returns a provider whose backend answers and holds no
	// session named contractSessionName.
	Responsive func(t *testing.T) runtime.Provider
	// Unreachable returns a provider whose backend cannot be observed at
	// all (its transport fails), so it cannot tell whether a session exists.
	Unreachable func(t *testing.T) runtime.Provider
}

// IdleWaitFixtures supplies sessions for the RUNTIME-CONTRACT-016 cases of a
// provider that implements [runtime.IdleWaitProvider]. Each fixture returns a
// provider and the name of a session it prepared (and registered cleanup for).
// A nil field skips its cases.
type IdleWaitFixtures struct {
	// Busy returns a session that every reading shows busy and never idle,
	// so a wait reaches its timeout having observed a running turn.
	Busy func(t *testing.T) (runtime.Provider, string)
	// Unobserved returns a session no reading of which shows it busy or
	// idle (every read fails or is indeterminate), so a wait reaches its
	// timeout having observed nothing.
	Unobserved func(t *testing.T) (runtime.Provider, string)
	// Timeout is each wait's bound; zero uses defaultIdleWaitTimeout.
	Timeout time.Duration
}

// RunStopContractTests runs the RUNTIME-CONTRACT-003 cases: a missing session
// on a responsive provider stops cleanly, and an unreachable provider reports
// the failure rather than certifying absence, so [runtime.StopForCleanup]
// propagates it.
func RunStopContractTests(t *testing.T, f StopFixtures) {
	t.Helper()

	t.Run("StopContract_GoneSessionIsNil", func(t *testing.T) {
		if f.Responsive == nil {
			t.Skip("no responsive-provider fixture")
		}
		if err := checkStopGoneSession(f.Responsive(t)); err != nil {
			t.Error(err)
		}
	})

	t.Run("StopContract_UnreachableRuntimeIsError", func(t *testing.T) {
		if f.Unreachable == nil {
			t.Skip("no unreachable-provider fixture")
		}
		if err := checkStopUnreachable(f.Unreachable(t)); err != nil {
			t.Error(err)
		}
	})
}

// checkStopGoneSession reports how sp breaks RUNTIME-CONTRACT-003 for a
// session it does not have, or nil when it keeps the contract.
func checkStopGoneSession(sp runtime.Provider) error {
	if err := sp.Stop(contractSessionName); err != nil {
		return fmt.Errorf("runtime contract 003: Stop on a missing session of a responsive provider = %w, want nil", err)
	}
	if err := runtime.StopForCleanup(sp, contractSessionName); err != nil {
		return fmt.Errorf("runtime contract 003: StopForCleanup on a missing session of a responsive provider = %w, want nil", err)
	}
	return nil
}

// checkStopUnreachable reports how sp breaks RUNTIME-CONTRACT-003 when its
// backend cannot be observed, or nil when it keeps the contract.
func checkStopUnreachable(sp runtime.Provider) error {
	err := sp.Stop(contractSessionName)
	if err == nil {
		return errors.New("runtime contract 003: Stop on an unreachable runtime = nil, want an error: it cannot certify the session absent")
	}
	if errors.Is(err, runtime.ErrSessionNotFound) {
		return fmt.Errorf("runtime contract 003: Stop on an unreachable runtime = %w, want an error that is not runtime.ErrSessionNotFound", err)
	}
	if err := runtime.StopForCleanup(sp, contractSessionName); err == nil {
		return errors.New("runtime contract 003: StopForCleanup on an unreachable runtime = nil, want the Stop failure propagated")
	}
	return nil
}

// RunIdleWaitContractTests runs the RUNTIME-CONTRACT-016 cases: a wait is
// bounded by its timeout, a timeout after a busy reading is the timeout alone,
// and a timeout with no busy reading wraps [runtime.ErrWaitUnobserved].
func RunIdleWaitContractTests(t *testing.T, f IdleWaitFixtures) {
	t.Helper()
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = defaultIdleWaitTimeout
	}

	t.Run("IdleWaitContract_BusyTimeoutIsObserved", func(t *testing.T) {
		if f.Busy == nil {
			t.Skip("no busy-session fixture")
		}
		sp, name := f.Busy(t)
		if err := checkIdleWaitBusy(sp, name, timeout); err != nil {
			t.Error(err)
		}
	})

	t.Run("IdleWaitContract_UnobservedTimeoutWrapsErrWaitUnobserved", func(t *testing.T) {
		if f.Unobserved == nil {
			t.Skip("no unobserved-session fixture")
		}
		sp, name := f.Unobserved(t)
		if err := checkIdleWaitUnobserved(sp, name, timeout); err != nil {
			t.Error(err)
		}
	})

	t.Run("IdleWaitContract_CanceledContextIsError", func(t *testing.T) {
		if f.Busy == nil {
			t.Skip("no busy-session fixture")
		}
		sp, name := f.Busy(t)
		if err := checkIdleWaitCanceled(sp, name, timeout); err != nil {
			t.Error(err)
		}
	})
}

// idleWaiter returns sp as an [runtime.IdleWaitProvider], or an error when a
// fixture handed the idle-wait cases a provider without the capability.
func idleWaiter(sp runtime.Provider) (runtime.IdleWaitProvider, error) {
	w, ok := sp.(runtime.IdleWaitProvider)
	if !ok {
		return nil, fmt.Errorf("%T does not implement runtime.IdleWaitProvider", sp)
	}
	return w, nil
}

// timedIdleWait runs one wait and reports its error and whether it returned
// within the hard bound.
func timedIdleWait(ctx context.Context, w runtime.IdleWaitProvider, name string, timeout time.Duration) (waitErr, boundErr error) {
	start := time.Now()
	waitErr = w.WaitForIdle(ctx, name, timeout)
	if elapsed := time.Since(start); elapsed > timeout+idleWaitBoundSlack {
		return waitErr, fmt.Errorf("runtime contract 016: WaitForIdle(%q, %s) returned after %s: timeout is not a hard bound", name, timeout, elapsed)
	}
	return waitErr, nil
}

// checkIdleWaitBusy reports how sp breaks RUNTIME-CONTRACT-016 for a session
// every reading shows busy, or nil when it keeps the contract.
func checkIdleWaitBusy(sp runtime.Provider, name string, timeout time.Duration) error {
	w, err := idleWaiter(sp)
	if err != nil {
		return err
	}
	waitErr, boundErr := timedIdleWait(context.Background(), w, name, timeout)
	if boundErr != nil {
		return boundErr
	}
	if waitErr == nil {
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) on a session that stays busy = nil, want a timeout error", name)
	}
	if errors.Is(waitErr, runtime.ErrWaitUnobserved) {
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) on a session read busy = %w, want the timeout alone, not runtime.ErrWaitUnobserved", name, waitErr)
	}
	return nil
}

// checkIdleWaitUnobserved reports how sp breaks RUNTIME-CONTRACT-016 for a
// session no reading shows busy, or nil when it keeps the contract.
func checkIdleWaitUnobserved(sp runtime.Provider, name string, timeout time.Duration) error {
	w, err := idleWaiter(sp)
	if err != nil {
		return err
	}
	waitErr, boundErr := timedIdleWait(context.Background(), w, name, timeout)
	if boundErr != nil {
		return boundErr
	}
	if waitErr == nil {
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) on a session never read idle = nil, want a timeout error wrapping runtime.ErrWaitUnobserved", name)
	}
	if !errors.Is(waitErr, runtime.ErrWaitUnobserved) {
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) with no busy reading = %w, want an error wrapping runtime.ErrWaitUnobserved", name, waitErr)
	}
	return nil
}

// checkIdleWaitCanceled reports how sp breaks the IdleWaitProvider contract
// for a caller whose context already ended, or nil when it keeps it.
func checkIdleWaitCanceled(sp runtime.Provider, name string, timeout time.Duration) error {
	w, err := idleWaiter(sp)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitErr, boundErr := timedIdleWait(ctx, w, name, timeout)
	if boundErr != nil {
		return boundErr
	}
	if waitErr == nil {
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) under a canceled context = nil, want context.Canceled", name)
	}
	if !errors.Is(waitErr, context.Canceled) {
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) under a canceled context = %w, want context.Canceled", name, waitErr)
	}
	return nil
}
