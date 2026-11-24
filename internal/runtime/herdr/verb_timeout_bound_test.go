//go:build !windows

package herdr

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestVerbTimeoutLongerThanDefaultGetsItsOwnBound: a herdr call carrying its
// own --timeout T longer than the default bound is bounded at T plus the
// margin, not at the default. In production a 60s --timeout gets 70s. Against
// a stub, a call answering after the default bound but within T succeeds, and
// a call that never answers is still killed, at T plus the margin.
func TestVerbTimeoutLongerThanDefaultGetsItsOwnBound(t *testing.T) {
	t.Run("production bounds", func(t *testing.T) {
		c := newClient("gc-verb-bound-test", t.TempDir())
		if got, want := c.verbBound(60_000), 70*time.Second; got != want {
			t.Fatalf("verbBound(60s) = %v; want %v", got, want)
		}
		if got, want := c.verbBound(1_000), herdrCLIDefaultTimeout; got != want {
			t.Fatalf("verbBound(1s) = %v; want the %v default", got, want)
		}
	})
	t.Run("answer past the default is kept", func(t *testing.T) {
		c := newBoundedClient(t, "#!/bin/sh\nsleep 0.3\nprintf '{\"result\":{}}'\n")
		c.cliTimeout = 100 * time.Millisecond
		if _, err := c.runVerbTimeout(context.Background(), 400, "agent", "wait", "x"); err != nil {
			t.Fatalf("a call answering after 300ms with --timeout 400ms (default %v) failed: %v", c.cliTimeout, err)
		}
	})
	t.Run("no answer is killed at T plus margin", func(t *testing.T) {
		c := newBoundedClient(t, "#!/bin/sh\nexec sleep "+hangSecs+"\n")
		c.cliTimeout = 100 * time.Millisecond
		const timeoutMS = 400
		bound := timeoutMS*time.Millisecond + testMargin
		start := time.Now()
		_, err := c.runVerbTimeout(context.Background(), timeoutMS, "agent", "wait", "x")
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v; want context.DeadlineExceeded", err)
		}
		if elapsed < bound {
			t.Fatalf("killed after %v; want no earlier than T plus margin (%v)", elapsed, bound)
		}
		if elapsed > bound+testWaitDlay+boundSlack {
			t.Fatalf("returned after %v; want within T plus margin plus WaitDelay", elapsed)
		}
	})
}
