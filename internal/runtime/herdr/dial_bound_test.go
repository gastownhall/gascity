package herdr

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// TestDialSocketBoundsTheConnect pins the bound that sessionEventOpTimeout's own
// doc comment claims for the dial, and that the dial did not have: both callers
// set the connection deadline only after DialContext returns, and subscribeCycle
// dials under the long-lived stream context, which carries no deadline by design,
// so a connect that never completed hung the stream loop the bound exists to
// protect.
//
// Every arm goes through dialSocket and reads the context the connect actually
// received, because that is the only thing a listener-free test can observe: a real
// unix connect completes or fails at once unless the listener's backlog is
// saturated, so a missing bound looks exactly like a present one.
func TestDialSocketBoundsTheConnect(t *testing.T) {
	// dialSlack bounds how far past the op bound a correct deadline may land,
	// measured from a clock read taken immediately before dialSocket rather than
	// after it, so only the setup inside the call counts and nothing that happens
	// to the test goroutine afterwards does. The arms assert the deadline IS the
	// bound to within that, not merely no later than it: a one-nanosecond bound is
	// also "no later than" the op bound, and would otherwise pass. Keeping it tight
	// is what makes a merely-wrong bound, 9.9s for 10s, fail.
	const dialSlack = 100 * time.Millisecond

	// dialedUnder runs dialSocket with the connect replaced by dial, which
	// receives the bounded context.
	dialedUnder := func(t *testing.T, ctx context.Context, dial func(context.Context) error) error {
		t.Helper()
		c := &client{
			session:  "gc-test",
			sockPath: filepath.Join(t.TempDir(), "herdr.sock"),
			dialUnix: func(dctx context.Context, _ string) (net.Conn, error) {
				return nil, dial(dctx)
			},
		}
		_, err := c.dialSocket(ctx)
		return err
	}

	// deadlineOf captures the connect's deadline and refuses the dial. It also
	// returns the moment just before dialSocket ran, which is what the arms
	// measure the deadline against.
	notDialing := errors.New("not dialing in a unit test")
	deadlineOf := func(t *testing.T, ctx context.Context) (deadline time.Time, start time.Time, ok bool) {
		t.Helper()
		var got context.Context
		start = time.Now()
		if err := dialedUnder(t, ctx, func(dctx context.Context) error {
			got = dctx
			return notDialing
		}); !errors.Is(err, notDialing) {
			t.Fatalf("dialSocket error = %v, want the fake connect's own error", err)
		}
		deadline, ok = got.Deadline()
		return deadline, start, ok
	}

	t.Run("a caller without a deadline gets the op bound", func(t *testing.T) {
		deadline, start, ok := deadlineOf(t, context.Background())
		if !ok {
			t.Fatal("the connect ran with no deadline, so a stalled dial is unbounded")
		}
		if window := deadline.Sub(start); window < sessionEventOpTimeout || window > sessionEventOpTimeout+dialSlack {
			t.Errorf("connect deadline %v after the call, want the op bound %v", window, sessionEventOpTimeout)
		}
	})

	t.Run("a caller deadline beyond the bound is tightened", func(t *testing.T) {
		caller, cancel := context.WithTimeout(context.Background(), 3*sessionEventOpTimeout)
		defer cancel()

		deadline, start, ok := deadlineOf(t, caller)
		if !ok {
			t.Fatal("the connect ran with no deadline")
		}
		if window := deadline.Sub(start); window < sessionEventOpTimeout || window > sessionEventOpTimeout+dialSlack {
			t.Errorf("connect deadline %v after the call, want the op bound %v, not merely sooner than the caller's", window, sessionEventOpTimeout)
		}
	})

	t.Run("a caller deadline inside the bound survives", func(t *testing.T) {
		want := time.Now().Add(sessionEventOpTimeout / 10)
		caller, cancel := context.WithDeadline(context.Background(), want)
		defer cancel()

		deadline, _, ok := deadlineOf(t, caller)
		if !ok {
			t.Fatal("the caller's deadline was dropped")
		}
		if !deadline.Equal(want) {
			t.Errorf("connect deadline = %v, want the caller's own %v", deadline, want)
		}
	})

	t.Run("an expired caller deadline stays expired", func(t *testing.T) {
		caller, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		err := dialedUnder(t, caller, func(dctx context.Context) error { return dctx.Err() })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("dialSocket error = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("the constructor installs the production connect", func(t *testing.T) {
		// dialSocket has no fallback: the field is the only dial, and every
		// other arm replaces it. So what production connects through is decided
		// by newClient, and that is what this pins. Identity is the claim, not
		// the shape of what comes back: a substitute dial can imitate any error
		// the kernel returns for a path with no socket on it, so asserting on
		// one would pin nothing about which function ran.
		c := newClient("gc-test", t.TempDir())
		if c.dialUnix == nil {
			t.Fatal("newClient left the connect nil; dialSocket would panic on it")
		}
		if got, want := reflect.ValueOf(c.dialUnix).Pointer(), reflect.ValueOf(dialUnix).Pointer(); got != want {
			t.Errorf("newClient installed %s, want dialUnix",
				runtime.FuncForPC(got).Name())
		}
	})

	t.Run("canceling during the connect reaches it", func(t *testing.T) {
		caller, cancel := context.WithCancel(context.Background())
		defer cancel()

		entered := make(chan struct{})
		go func() {
			<-entered
			cancel()
		}()

		err := dialedUnder(t, caller, func(dctx context.Context) error {
			close(entered)
			<-dctx.Done()
			return dctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("dialSocket error = %v, want context.Canceled", err)
		}
	})
}
