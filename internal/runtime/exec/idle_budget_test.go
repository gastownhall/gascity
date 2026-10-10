package exec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The in-memory carrier tests run under testing/synctest: time inside the
// bubble only advances when every goroutine is durably blocked, so the
// deadlines below are exact rather than raced against the scheduler.

// slowCarrier is a runtime.Carrier whose every capture takes perCapture,
// or until the capture context ends, as a real exec op killed by its
// context does. Only Peek is used by WaitForIdle.
type slowCarrier struct {
	runtime.Carrier
	perCapture time.Duration
	pane       string

	mu       sync.Mutex
	captures int
}

func (c *slowCarrier) Peek(ctx context.Context, _ string, _ int) (string, error) {
	c.mu.Lock()
	c.captures++
	c.mu.Unlock()
	timer := time.NewTimer(c.perCapture)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return c.pane, nil
	}
}

func (c *slowCarrier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captures
}

// hungCarrier never answers a capture until release is closed; when
// honorCtx is set it returns on cancellation instead. started, when set, is
// closed as the first capture begins.
type hungCarrier struct {
	runtime.Carrier
	honorCtx bool
	release  chan struct{}
	started  chan struct{}
	once     sync.Once
}

func (c *hungCarrier) Peek(ctx context.Context, _ string, _ int) (string, error) {
	if c.started != nil {
		c.once.Do(func() { close(c.started) })
	}
	if c.honorCtx {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.release:
			return "", nil
		}
	}
	<-c.release
	return "", nil
}

// A capture is a full round trip to the box. With the 1s default, two
// 900ms captures cannot both finish: the second is canceled at the deadline
// and the wait returns there, never later. With the 5s exec budget the same
// carrier proves idle after exactly two captures.
// Kills: a deadline that only gates starting a capture (letting the
// in-flight one run on under the 30s op timeout), and an observation that
// finished past the deadline counted as idle.
func TestWaitForIdleSlowCarrierHonorsHardBound(t *testing.T) {
	fastIdlePoll(t)

	t.Run("1s timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			c := &slowCarrier{perCapture: 900 * time.Millisecond, pane: idleClaudePane}
			err := waitForPaneIdle(context.Background(), c, time.Now, "s", "❯ ", time.Second)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waitForPaneIdle = %v, want DeadlineExceeded", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("returned after %v, past the 1s deadline", elapsed)
			}
			if got := c.count(); got != 2 {
				t.Fatalf("captures = %d, want 2 (the second canceled at the deadline)", got)
			}
		})
	})

	t.Run("5s budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			c := &slowCarrier{perCapture: 900 * time.Millisecond, pane: idleClaudePane}
			if err := waitForPaneIdle(context.Background(), c, time.Now, "s", "❯ ", execIdleProbeBudget); err != nil {
				t.Fatalf("waitForPaneIdle with the exec budget: %v", err)
			}
			if got := c.count(); got != 2 {
				t.Fatalf("captures = %d, want exactly 2", got)
			}
		})
	})
}

// A capture that never answers is abandoned at the deadline, whether or not
// the carrier honors its context.
// Kills: a WaitForIdle that blocks on an in-flight capture past timeout
// (the IdleWaitProvider contract makes timeout a hard upper bound).
func TestWaitForIdleHungCaptureIsCanceledAtDeadline(t *testing.T) {
	fastIdlePoll(t)
	for _, honorCtx := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			c := &hungCarrier{honorCtx: honorCtx, release: make(chan struct{})}
			defer close(c.release)
			const timeout = 100 * time.Millisecond
			start := time.Now()
			err := waitForPaneIdle(context.Background(), c, time.Now, "s", "❯ ", timeout)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("honorCtx=%v: waitForPaneIdle = %v, want DeadlineExceeded", honorCtx, err)
			}
			if elapsed := time.Since(start); elapsed != timeout {
				t.Fatalf("honorCtx=%v: returned after %v, want exactly %v", honorCtx, elapsed, timeout)
			}
		})
	}
}

// Canceling the caller's context while a capture is in flight ends the wait
// at once, not at the deadline.
// Kills: a capture context derived from context.Background() instead of the
// caller's (a reconciler shutdown or a canceled nudge would wait out the
// whole timeout).
func TestWaitForIdleCancelDuringCapture(t *testing.T) {
	fastIdlePoll(t)
	synctest.Test(t, func(t *testing.T) {
		c := &hungCarrier{honorCtx: true, release: make(chan struct{}), started: make(chan struct{})}
		defer close(c.release)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-c.started
			cancel()
		}()
		start := time.Now()
		err := waitForPaneIdle(ctx, c, time.Now, "s", "❯ ", 10*time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForPaneIdle = %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("returned after %v, want at once", elapsed)
		}
	})
}

// Only a pack with an idle boundary asks for the larger budget, through the
// raw and the seam-backed provider alike.
// Kills: a budget for packs that cannot run the probe, and a seam that drops
// the budget (the orchestrator would keep the 1s default for every pack).
func TestIdleProbeBudgetFollowsDeclaration(t *testing.T) {
	declared := newIdlePack(t, idleHandshake)
	undeclared := newIdlePack(t, `{"version":0,"capabilities":["report-activity","report-attachment"]}`)
	for name, tt := range map[string]struct {
		script string
		want   time.Duration
	}{
		"declared":   {declared.script, execIdleProbeBudget},
		"undeclared": {undeclared.script, 0},
	} {
		if got := NewProvider(tt.script).IdleProbeBudget(); got != tt.want {
			t.Errorf("%s: raw IdleProbeBudget = %v, want %v", name, got, tt.want)
		}
		bp, ok := NewSeamBacked(tt.script).(runtime.IdleProbeBudgetProvider)
		if !ok {
			t.Fatalf("%s: seam-backed provider does not implement runtime.IdleProbeBudgetProvider", name)
		}
		if got := bp.IdleProbeBudget(); got != tt.want {
			t.Errorf("%s: seam-backed IdleProbeBudget = %v, want %v", name, got, tt.want)
		}
	}
	if execIdleProbeBudget != 5*time.Second {
		t.Errorf("execIdleProbeBudget = %v, want 5s", execIdleProbeBudget)
	}
}
