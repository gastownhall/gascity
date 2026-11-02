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
