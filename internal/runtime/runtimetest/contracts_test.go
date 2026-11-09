package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stopAnswerProvider answers every Stop with err.
type stopAnswerProvider struct {
	runtime.Provider
	err error
}

func (p stopAnswerProvider) Stop(string) error { return p.err }

// idleAnswerProvider answers every WaitForIdle with err (ctx's error first).
type idleAnswerProvider struct {
	runtime.Provider
	err error
}

func (p idleAnswerProvider) WaitForIdle(ctx context.Context, _ string, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.err
}

// swallowingIdleProvider answers every WaitForIdle with nil, ignoring ctx.
type swallowingIdleProvider struct{ runtime.Provider }

func (swallowingIdleProvider) WaitForIdle(context.Context, string, time.Duration) error { return nil }

var errTransport = errors.New("dial unix /run/backend.sock: connect: connection refused")

func TestCheckStopUnreachable(t *testing.T) {
	cases := []struct {
		name    string
		stopErr error
		wantOK  bool
	}{
		{"swallowed failure", nil, false},
		{"failure certified as absence", fmt.Errorf("backend: %w", runtime.ErrSessionNotFound), false},
		{"transport failure reported", fmt.Errorf("backend stop: %w", errTransport), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStopUnreachable(stopAnswerProvider{Provider: runtime.NewFake(), err: tc.stopErr})
			if (err == nil) != tc.wantOK {
				t.Errorf("checkStopUnreachable(Stop=%v) = %v, want ok=%v", tc.stopErr, err, tc.wantOK)
			}
		})
	}
}

func TestCheckStopGoneSession(t *testing.T) {
	cases := []struct {
		name    string
		stopErr error
		wantOK  bool
	}{
		{"nil for a missing session", nil, true},
		{"missing session reported as failure", errTransport, false},
		{"missing session reported as not found", runtime.ErrSessionNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStopGoneSession(stopAnswerProvider{Provider: runtime.NewFake(), err: tc.stopErr})
			if (err == nil) != tc.wantOK {
				t.Errorf("checkStopGoneSession(Stop=%v) = %v, want ok=%v", tc.stopErr, err, tc.wantOK)
			}
		})
	}
}

// A dead server hosts no session, so teardown absorbs whatever Stop answers
// for it short of a failure: nil, or a gone answer StopForCleanup takes.
func TestCheckStopDeadServer(t *testing.T) {
	cases := []struct {
		name    string
		stopErr error
		wantOK  bool
	}{
		{"nil for a dead server", nil, true},
		{"gone answer absorbed", fmt.Errorf("backend: %w", runtime.ErrSessionNotFound), true},
		{"failure that wedges teardown", fmt.Errorf("backend stop: %w", errTransport), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStopDeadServer(stopAnswerProvider{Provider: runtime.NewFake(), err: tc.stopErr})
			if (err == nil) != tc.wantOK {
				t.Errorf("checkStopDeadServer(Stop=%v) = %v, want ok=%v", tc.stopErr, err, tc.wantOK)
			}
		})
	}
}

func TestCheckIdleWait(t *testing.T) {
	observedTimeout := fmt.Errorf("not idle: %w", context.DeadlineExceeded)
	unobservedTimeout := fmt.Errorf("not idle: %w: %w", context.DeadlineExceeded, runtime.ErrWaitUnobserved)
	unavailable := fmt.Errorf("waiting: %w: %w", runtime.ErrRuntimeUnavailable, errTransport)
	canceled := fmt.Errorf("waiting: %w", context.Canceled)
	const timeout = 10 * time.Millisecond

	cases := []struct {
		name  string
		check func(runtime.Provider, string, time.Duration) error
		sp    runtime.Provider
		ok    bool
	}{
		{"busy: timeout alone", checkIdleWaitBusy, idleAnswerProvider{runtime.NewFake(), observedTimeout}, true},
		{"busy: swallowed timeout", checkIdleWaitBusy, idleAnswerProvider{runtime.NewFake(), nil}, false},
		{"busy: claims unobserved", checkIdleWaitBusy, idleAnswerProvider{runtime.NewFake(), unobservedTimeout}, false},
		{"busy: reported unavailable", checkIdleWaitBusy, idleAnswerProvider{runtime.NewFake(), unavailable}, false},
		{"busy: reported canceled", checkIdleWaitBusy, idleAnswerProvider{runtime.NewFake(), canceled}, false},
		{"unobserved: wraps sentinel", checkIdleWaitUnobserved, idleAnswerProvider{runtime.NewFake(), unobservedTimeout}, true},
		{"unobserved: timeout alone", checkIdleWaitUnobserved, idleAnswerProvider{runtime.NewFake(), observedTimeout}, false},
		{"unobserved: swallowed timeout", checkIdleWaitUnobserved, idleAnswerProvider{runtime.NewFake(), nil}, false},
		{"canceled: ctx error", checkIdleWaitCanceled, idleAnswerProvider{runtime.NewFake(), nil}, true},
		{"canceled: ignored", checkIdleWaitCanceled, swallowingIdleProvider{runtime.NewFake()}, false},
		{"no capability", checkIdleWaitBusy, stopAnswerProvider{Provider: runtime.NewFailFake()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.check(tc.sp, "s", timeout)
			if (err == nil) != tc.ok {
				t.Errorf("check = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// inputAnswerProvider answers every Nudge, SendKeys and Interrupt with err.
type inputAnswerProvider struct {
	runtime.Provider
	err error
}

func (p inputAnswerProvider) Nudge(string, []runtime.ContentBlock) error { return p.err }
func (p inputAnswerProvider) SendKeys(string, ...string) error           { return p.err }
func (p inputAnswerProvider) Interrupt(string) error                     { return p.err }

func TestCheckMissingSessionInput(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		wantOK bool
	}{
		{"reported delivered", nil, false},
		{"absence not certified", fmt.Errorf("backend: %w", runtime.ErrRuntimeUnavailable), false},
		{"absence reported", fmt.Errorf("session %q: %w", missingInputSessionName, runtime.ErrSessionNotFound), true},
	}
	for _, tc := range cases {
		for _, in := range missingSessionInputs {
			t.Run(tc.name+"/"+in.name, func(t *testing.T) {
				err := checkMissingSessionInput(inputAnswerProvider{Provider: runtime.NewFake(), err: tc.err}, in)
				if (err == nil) != tc.wantOK {
					t.Errorf("checkMissingSessionInput(%s=%v) = %v, want ok=%v", in.name, tc.err, err, tc.wantOK)
				}
			})
		}
	}
}

// defaultServerTeardownConfig is the start config of fixtures that set none.
var defaultServerTeardownConfig = ServerTeardownFixtures{}.startConfig

// serverFake is a [runtime.ServerLifecycleProvider] over a fake whose
// teardown stops every session it lists, with one flaw switched on per case.
type serverFake struct {
	*runtime.Fake
	keepsSessions     bool // TeardownServer stops nothing and answers nil
	failsWhenGone     bool // TeardownServer with no session errors
	swallowsSurvivor  bool // TeardownServer answers nil though a Stop failed
	configureStopsAll bool // ConfigureServer stops every session
}

func (f serverFake) stopAll() error {
	names, _ := f.ListRunning("")
	var errs []error
	for _, name := range names {
		errs = append(errs, f.Stop(name))
	}
	return errors.Join(errs...)
}

func (f serverFake) ConfigureServer() error {
	if f.configureStopsAll {
		return f.stopAll()
	}
	return nil
}

func (f serverFake) TeardownServer() error {
	names, _ := f.ListRunning("")
	switch {
	case f.keepsSessions:
		return nil
	case f.failsWhenGone && len(names) == 0:
		return errors.New("no server running on /tmp/gc-test.sock")
	}
	if err := f.stopAll(); err != nil && !f.swallowsSurvivor {
		return err
	}
	return nil
}

// Kills each RUNTIME-CONTRACT-023 check that would pass a teardown breaking
// it: one that leaves sessions running, fails on a server already gone,
// answers nil over a survivor, or a ConfigureServer that ends sessions.
func TestCheckServerTeardown(t *testing.T) {
	unstoppable := func(f serverFake) (runtime.Provider, string) {
		const name = "survivor"
		_ = f.Start(context.Background(), name, runtime.Config{})
		f.StopErrors[name] = errTransport
		return f, name
	}
	for _, tc := range []struct {
		name  string
		fake  serverFake
		check func(t *testing.T, f serverFake) error
		ok    bool
	}{
		{"ends sessions: honest", serverFake{}, func(t *testing.T, f serverFake) error {
			return checkServerTeardownEndsSessions(t, f, defaultServerTeardownConfig)
		}, true},
		{"ends sessions: keeps them", serverFake{keepsSessions: true}, func(t *testing.T, f serverFake) error {
			return checkServerTeardownEndsSessions(t, f, defaultServerTeardownConfig)
		}, false},
		{"second teardown: honest", serverFake{}, func(t *testing.T, f serverFake) error {
			return checkSecondTeardownIsNil(t, f, defaultServerTeardownConfig)
		}, true},
		{"second teardown: fails when gone", serverFake{failsWhenGone: true}, func(t *testing.T, f serverFake) error {
			return checkSecondTeardownIsNil(t, f, defaultServerTeardownConfig)
		}, false},
		{"survivor: reported", serverFake{}, func(_ *testing.T, f serverFake) error { return checkServerTeardownSurvivor(unstoppable(f)) }, true},
		{"survivor: swallowed", serverFake{swallowsSurvivor: true}, func(_ *testing.T, f serverFake) error { return checkServerTeardownSurvivor(unstoppable(f)) }, false},
		{"configure: honest", serverFake{}, func(t *testing.T, f serverFake) error {
			return checkConfigureServerIdempotent(t, f, defaultServerTeardownConfig)
		}, true},
		{"configure: ends sessions", serverFake{configureStopsAll: true}, func(t *testing.T, f serverFake) error {
			return checkConfigureServerIdempotent(t, f, defaultServerTeardownConfig)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			f.Fake = runtime.NewFake()
			if err := tc.check(t, f); (err == nil) != tc.ok {
				t.Errorf("check = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	if err := checkServerTeardownEndsSessions(t, runtime.NewFake(), defaultServerTeardownConfig); err == nil {
		t.Error("a provider without ServerLifecycleProvider passed RUNTIME-CONTRACT-023")
	}
}
