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

// serverTeardownSessions are the sessions the RUNTIME-CONTRACT-023 cases
// start before a teardown.
var serverTeardownSessions = []string{"server-teardown-contract-a", "server-teardown-contract-b"}

// ServerTeardownFixtures supplies providers for the RUNTIME-CONTRACT-023 cases
// of a runtime that implements [runtime.ServerLifecycleProvider]. A nil field
// skips its cases.
type ServerTeardownFixtures struct {
	// Serving returns a provider that holds no session yet and whose
	// server, if any, can be torn down: every session it starts ends with
	// it.
	Serving func(t *testing.T) runtime.Provider
	// Unstoppable returns a provider and a running session its teardown
	// cannot end, so a session of the city may survive the teardown.
	Unstoppable func(t *testing.T) (runtime.Provider, string)
	// StartConfig is the config the cases start their sessions with; nil
	// starts `sleep 300` in a fresh work dir.
	StartConfig func(t *testing.T) runtime.Config
}

// startConfig is the config f's cases start a session with.
func (f ServerTeardownFixtures) startConfig(t *testing.T) runtime.Config {
	if f.StartConfig != nil {
		return f.StartConfig(t)
	}
	return runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300"}
}

// RunServerTeardownContract runs the RUNTIME-CONTRACT-023 cases, the
// [runtime.ServerLifecycleProvider] contract: TeardownServer, which gc calls
// once every session is drained, leaves no session the provider started
// running when it returns nil; it returns nil when the server is already
// gone, so a second teardown is nil; it returns an error when a session may
// survive it; and ConfigureServer is idempotent, so calling it again on a
// serving provider ends no session.
func RunServerTeardownContract(t *testing.T, f ServerTeardownFixtures) {
	t.Helper()

	t.Run("ServerTeardown_EndsEverySession", func(t *testing.T) {
		if f.Serving == nil {
			t.Skip("no serving-provider fixture")
		}
		if err := checkServerTeardownEndsSessions(t, f.Serving(t), f.startConfig); err != nil {
			t.Error(err)
		}
	})

	t.Run("ServerTeardown_SecondTeardownIsNil", func(t *testing.T) {
		if f.Serving == nil {
			t.Skip("no serving-provider fixture")
		}
		if err := checkSecondTeardownIsNil(t, f.Serving(t), f.startConfig); err != nil {
			t.Error(err)
		}
	})

	t.Run("ServerTeardown_SurvivorIsError", func(t *testing.T) {
		if f.Unstoppable == nil {
			t.Skip("no unstoppable-session fixture")
		}
		sp, name := f.Unstoppable(t)
		if err := checkServerTeardownSurvivor(sp, name); err != nil {
			t.Error(err)
		}
	})

	t.Run("ServerTeardown_ConfigureServerIsIdempotent", func(t *testing.T) {
		if f.Serving == nil {
			t.Skip("no serving-provider fixture")
		}
		if err := checkConfigureServerIdempotent(t, f.Serving(t), f.startConfig); err != nil {
			t.Error(err)
		}
	})
}

// serverLifecycle returns sp's server lifecycle, or an error naming the
// contract sp cannot keep without one.
func serverLifecycle(sp runtime.Provider) (runtime.ServerLifecycleProvider, error) {
	l, ok := sp.(runtime.ServerLifecycleProvider)
	if !ok {
		return nil, fmt.Errorf("runtime contract 023: %T does not implement runtime.ServerLifecycleProvider", sp)
	}
	return l, nil
}

// startServerTeardownSessions starts every serverTeardownSessions session on
// sp, and stops each at the end of the test.
func startServerTeardownSessions(t *testing.T, sp runtime.Provider, cfg func(*testing.T) runtime.Config) error {
	t.Helper()
	for _, name := range serverTeardownSessions {
		if err := sp.Start(context.Background(), name, cfg(t)); err != nil {
			return fmt.Errorf("runtime contract 023: starting %s before the teardown: %w", name, err)
		}
		t.Cleanup(func() { _ = sp.Stop(name) })
	}
	return nil
}

// checkServerTeardownEndsSessions reports how sp breaks RUNTIME-CONTRACT-023
// when it tears its server down under running sessions, or nil when it keeps
// the contract.
func checkServerTeardownEndsSessions(t *testing.T, sp runtime.Provider, cfg func(*testing.T) runtime.Config) error {
	t.Helper()
	l, err := serverLifecycle(sp)
	if err != nil {
		return err
	}
	if err := startServerTeardownSessions(t, sp, cfg); err != nil {
		return err
	}
	if err := l.TeardownServer(); err != nil {
		return fmt.Errorf("runtime contract 023: TeardownServer with sessions it can stop = %w, want nil", err)
	}
	for _, name := range serverTeardownSessions {
		if sp.IsRunning(name) {
			return fmt.Errorf("runtime contract 023: %s is still running after TeardownServer returned nil: nil means no session the runtime started remains", name)
		}
	}
	return nil
}

// checkSecondTeardownIsNil reports how sp breaks RUNTIME-CONTRACT-023 when
// its server is already gone, or nil when it keeps the contract.
func checkSecondTeardownIsNil(t *testing.T, sp runtime.Provider, cfg func(*testing.T) runtime.Config) error {
	t.Helper()
	l, err := serverLifecycle(sp)
	if err != nil {
		return err
	}
	if err := l.TeardownServer(); err != nil {
		return fmt.Errorf("runtime contract 023: TeardownServer with no session started = %w, want nil: a server that is not running is already torn down", err)
	}
	if err := startServerTeardownSessions(t, sp, cfg); err != nil {
		return err
	}
	if err := l.TeardownServer(); err != nil {
		return fmt.Errorf("runtime contract 023: first TeardownServer = %w, want nil", err)
	}
	if err := l.TeardownServer(); err != nil {
		return fmt.Errorf("runtime contract 023: second TeardownServer = %w, want nil: the server is already gone", err)
	}
	return nil
}

// checkServerTeardownSurvivor reports how sp breaks RUNTIME-CONTRACT-023 when
// its teardown cannot end the running session name, or nil when it keeps the
// contract.
func checkServerTeardownSurvivor(sp runtime.Provider, name string) error {
	l, err := serverLifecycle(sp)
	if err != nil {
		return err
	}
	if err := l.TeardownServer(); err == nil {
		return fmt.Errorf("runtime contract 023: TeardownServer = nil while %s may survive it, want an error: nil tells gc stop no session remains", name)
	}
	return nil
}

// checkConfigureServerIdempotent reports how sp breaks RUNTIME-CONTRACT-023
// when ConfigureServer runs again on a serving provider, or nil when it keeps
// the contract. Callers treat its errors as best-effort warnings, so only its
// repeatability is checked: a second call answers as the first did, and
// neither ends a running session.
func checkConfigureServerIdempotent(t *testing.T, sp runtime.Provider, cfg func(*testing.T) runtime.Config) error {
	t.Helper()
	l, err := serverLifecycle(sp)
	if err != nil {
		return err
	}
	if err := startServerTeardownSessions(t, sp, cfg); err != nil {
		return err
	}
	first, second := l.ConfigureServer(), l.ConfigureServer()
	if (first == nil) != (second == nil) {
		return fmt.Errorf("runtime contract 023: ConfigureServer answered %q, then %q: it must be idempotent", fmt.Sprint(first), fmt.Sprint(second))
	}
	for _, name := range serverTeardownSessions {
		if !sp.IsRunning(name) {
			return fmt.Errorf("runtime contract 023: %s stopped running after ConfigureServer ran twice: configuring the server must not end its sessions", name)
		}
	}
	return nil
}
