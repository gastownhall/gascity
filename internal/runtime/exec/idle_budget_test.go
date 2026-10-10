package exec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// slowCarrier is a runtime.Carrier whose every capture takes perCapture on
// clock. A capture that would end past the capture context's deadline runs
// until that deadline and is canceled there, as a real exec op killed by its
// context is. Only Peek is used by WaitForIdle.
type slowCarrier struct {
	runtime.Carrier
	clock      *fakeClock
	perCapture time.Duration
	pane       string

	mu       sync.Mutex
	captures int
}

func (c *slowCarrier) Peek(ctx context.Context, _ string, _ int) (string, error) {
	c.mu.Lock()
	c.captures++
	c.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < c.perCapture {
			// The real wait is what remains of the deadline; the fake clock
			// moves to the deadline with it.
			<-ctx.Done()
			c.clock.advance(left)
			return "", ctx.Err()
		}
	}
	c.clock.advance(c.perCapture)
	return c.pane, nil
}

func (c *slowCarrier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captures
}

// hungCarrier never answers a capture until release is closed; when
// honorCtx is set it returns on cancellation instead.
type hungCarrier struct {
	runtime.Carrier
	honorCtx bool
	release  chan struct{}
}

func (c *hungCarrier) Peek(ctx context.Context, _ string, _ int) (string, error) {
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
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
		start := clock.Now()
		c := &slowCarrier{clock: clock, perCapture: 900 * time.Millisecond, pane: idleClaudePane}
		realStart := time.Now()
		err := waitForPaneIdle(context.Background(), c, clock.Now, "s", "❯ ", time.Second)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waitForPaneIdle = %v, want DeadlineExceeded", err)
		}
		if elapsed := clock.Now().Sub(start); elapsed > time.Second {
			t.Fatalf("clock advanced %v, past the 1s deadline", elapsed)
		}
		if wall := time.Since(realStart); wall > time.Second {
			t.Fatalf("wall time %v, past the 1s deadline", wall)
		}
		if got := c.count(); got != 2 {
			t.Fatalf("captures = %d, want 2 (the second canceled at the deadline)", got)
		}
	})

	t.Run("5s budget", func(t *testing.T) {
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
		c := &slowCarrier{clock: clock, perCapture: 900 * time.Millisecond, pane: idleClaudePane}
		if err := waitForPaneIdle(context.Background(), c, clock.Now, "s", "❯ ", execIdleProbeBudget); err != nil {
			t.Fatalf("waitForPaneIdle with the exec budget: %v", err)
		}
		if got := c.count(); got != 2 {
			t.Fatalf("captures = %d, want exactly 2", got)
		}
	})
}

// A capture that never answers is abandoned at the deadline, whether or not
// the carrier honors its context.
// Kills: a WaitForIdle that blocks on an in-flight capture past timeout
// (the IdleWaitProvider contract makes timeout a hard upper bound).
func TestWaitForIdleHungCaptureIsCanceledAtDeadline(t *testing.T) {
	fastIdlePoll(t)
	for _, honorCtx := range []bool{true, false} {
		c := &hungCarrier{honorCtx: honorCtx, release: make(chan struct{})}
		t.Cleanup(func() { close(c.release) })
		const timeout = 100 * time.Millisecond
		start := time.Now()
		err := waitForPaneIdle(context.Background(), c, time.Now, "s", "❯ ", timeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("honorCtx=%v: waitForPaneIdle = %v, want DeadlineExceeded", honorCtx, err)
		}
		// The bound is the deadline itself; the slack only absorbs scheduling.
		if elapsed := time.Since(start); elapsed > timeout+time.Second {
			t.Fatalf("honorCtx=%v: returned after %v, want about %v", honorCtx, elapsed, timeout)
		}
	}
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
