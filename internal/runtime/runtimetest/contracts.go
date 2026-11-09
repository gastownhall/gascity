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
// provider cannot observe its inventory at all, while teardown absorbs a
// server confirmed dead. A nil field skips its cases, so a provider whose test
// environment cannot build that state opts out.
type StopFixtures struct {
	// Responsive returns a provider whose backend answers and holds no
	// session named contractSessionName.
	Responsive func(t *testing.T) runtime.Provider
	// Unreachable returns a provider whose backend cannot be observed at
	// all (its transport fails) while its server may still be alive, so it
	// cannot tell whether a session exists.
	Unreachable func(t *testing.T) runtime.Provider
	// DeadServer returns a provider whose server is confirmed dead: its
	// socket is gone and nothing listens where it was bound, so it hosts no
	// session at all.
	DeadServer func(t *testing.T) runtime.Provider
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
// on a responsive provider stops cleanly; an unreachable provider reports the
// failure rather than certifying absence, so [runtime.StopForCleanup]
// propagates it; and with the server confirmed dead the same teardown
// succeeds, so it never wedges on a session that cannot exist.
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

	t.Run("StopContract_DeadServerIsAbsorbedByCleanup", func(t *testing.T) {
		if f.DeadServer == nil {
			t.Skip("no dead-server fixture")
		}
		if err := checkStopDeadServer(f.DeadServer(t)); err != nil {
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

// checkStopDeadServer reports how sp breaks RUNTIME-CONTRACT-003 when its
// server is confirmed dead, or nil when it keeps the contract. Stop itself may
// report the missing server (tmux does); teardown must absorb it.
func checkStopDeadServer(sp runtime.Provider) error {
	if err := runtime.StopForCleanup(sp, contractSessionName); err != nil {
		return fmt.Errorf("runtime contract 003: StopForCleanup with the server confirmed dead = %w, want nil: a dead server hosts no session", err)
	}
	return nil
}

// missingInputSessionName is the session the RUNTIME-CONTRACT-019 cases
// drive. No fixture ever starts it, so on a responsive provider it is a
// session the runtime can tell is absent.
const missingInputSessionName = "never-started-input-contract-session"

// missingSessionInput is one driving call RUNTIME-CONTRACT-019 covers.
type missingSessionInput struct {
	name string
	send func(runtime.Provider, string) error
}

// missingSessionInputs are the driving calls whose nil answer a caller
// reads as delivered.
var missingSessionInputs = []missingSessionInput{
	{"Nudge", func(sp runtime.Provider, name string) error { return sp.Nudge(name, runtime.TextContent("hello")) }},
	{"SendKeys", func(sp runtime.Provider, name string) error { return sp.SendKeys(name, "Enter") }},
	{"Interrupt", func(sp runtime.Provider, name string) error { return sp.Interrupt(name) }},
}

// RunMissingSessionInputContract runs the RUNTIME-CONTRACT-019 cases against
// providers from newProvider, each responsive and holding no session named
// missingInputSessionName: Nudge, SendKeys and Interrupt to that session
// return an error wrapping [runtime.ErrSessionNotFound], never the nil a
// caller reads as delivered. The contract is scoped to the exec (RPP)
// provider: the generic seam adapter still answers nil for the other
// providers, which RunSessionTests allows.
func RunMissingSessionInputContract(t *testing.T, newProvider func(t *testing.T) runtime.Provider) {
	t.Helper()
	for _, in := range missingSessionInputs {
		t.Run("MissingSessionInput_"+in.name+"IsNotDelivered", func(t *testing.T) {
			if err := checkMissingSessionInput(newProvider(t), in); err != nil {
				t.Error(err)
			}
		})
	}
}

// checkMissingSessionInput reports how sp breaks RUNTIME-CONTRACT-019 for
// in, or nil when it keeps the contract.
func checkMissingSessionInput(sp runtime.Provider, in missingSessionInput) error {
	err := in.send(sp, missingInputSessionName)
	if err == nil {
		return fmt.Errorf("runtime contract 019: %s to a missing session of a responsive provider = nil, want runtime.ErrSessionNotFound: nil reads as delivered", in.name)
	}
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		return fmt.Errorf("runtime contract 019: %s to a missing session of a responsive provider = %w, want an error wrapping runtime.ErrSessionNotFound", in.name, err)
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
	switch {
	case waitErr == nil:
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) on a session that stays busy = nil, want a timeout error", name)
	case errors.Is(waitErr, runtime.ErrWaitUnobserved), errors.Is(waitErr, runtime.ErrRuntimeUnavailable):
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) on a session read busy = %w, want the timeout alone: a restart decider reads runtime.ErrWaitUnobserved and runtime.ErrRuntimeUnavailable as no running turn", name, waitErr)
	case errors.Is(waitErr, context.Canceled):
		return fmt.Errorf("runtime contract 016: WaitForIdle(%q) on a session read busy = %w, want a timeout: nothing canceled the wait", name, waitErr)
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
