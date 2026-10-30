package lanesubmit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The check refuses a session waiting on a pending interaction, naming it,
// and accepts one the runtime says waits on none.
func TestCheckPendingRefusesOnlyWhatIsPending(t *testing.T) {
	err := CheckPending(context.Background(), "s-1", func(context.Context) (*runtime.PendingInteraction, error) {
		return &runtime.PendingInteraction{RequestID: "approval-1", Kind: "approval", Prompt: "Bash: date"}, nil
	})
	if !errors.Is(err, session.ErrPendingInteraction) || !strings.Contains(err.Error(), "approval-1") {
		t.Fatalf("pending: err = %v, want session.ErrPendingInteraction naming approval-1", err)
	}
	var check *PendingCheckError
	if errors.As(err, &check) {
		t.Fatalf("pending: err = %v is a PendingCheckError; a seen prompt is not a failed look", err)
	}
	if err := CheckPending(context.Background(), "s-1", func(context.Context) (*runtime.PendingInteraction, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("nothing pending: err = %v, want nil", err)
	}
}

// A look that cannot answer refuses the request as a PendingCheckError for
// the session, wrapping why, whatever the cause: an unreachable runtime,
// any other error, a panic, a look past the bound, or a caller whose
// context ends first. Kills a check that accepts a message the runtime
// never said nothing blocks.
func TestCheckPendingRefusesALookThatCannotAnswer(t *testing.T) {
	unreachable := fmt.Errorf("%w: socket gone", runtime.ErrRuntimeUnavailable)
	broke := errors.New("pending look broke")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hang := func(context.Context) (*runtime.PendingInteraction, error) {
		<-release
		return nil, nil
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		timeout time.Duration
		look    PendingLook
		want    error
		detail  string
	}{
		{name: "runtime unavailable", look: func(context.Context) (*runtime.PendingInteraction, error) { return nil, unreachable }, want: runtime.ErrRuntimeUnavailable},
		{name: "another error", look: func(context.Context) (*runtime.PendingInteraction, error) { return nil, broke }, want: broke},
		{name: "panic", look: func(context.Context) (*runtime.PendingInteraction, error) { panic("look blew up") }, detail: "the look panicked: look blew up"},
		{name: "past the bound", timeout: 20 * time.Millisecond, look: hang, want: context.DeadlineExceeded},
		{name: "caller's context ends", ctx: canceled, look: hang, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, timeout := tc.ctx, tc.timeout
			if ctx == nil {
				ctx = context.Background()
			}
			if timeout == 0 {
				timeout = PendingCheckTimeout
			}
			err := checkPending(ctx, "s-1", timeout, tc.look)
			var check *PendingCheckError
			if !errors.As(err, &check) || check.Session != "s-1" {
				t.Fatalf("err = %v, want a PendingCheckError for s-1", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.detail) || !strings.Contains(err.Error(), "could not check session s-1 for a pending interaction") || !strings.HasSuffix(err.Error(), "retry") {
				t.Fatalf("err = %q, want it to say the check could not be made (%q) and to retry", err, tc.detail)
			}
			if errors.Is(err, session.ErrPendingInteraction) {
				t.Fatalf("err = %v is ErrPendingInteraction; nothing was seen pending", err)
			}
		})
	}
}

// The bound is five seconds: CheckPending hands the look a context whose
// deadline is PendingCheckTimeout away, and PendingCheckTimeout is 5s.
// Kills a changed or dropped bound.
func TestCheckPendingIsBoundedAtFiveSeconds(t *testing.T) {
	if PendingCheckTimeout != 5*time.Second {
		t.Fatalf("PendingCheckTimeout = %v, want 5s (the maintainer's ruling, ga-81xriy)", PendingCheckTimeout)
	}
	before := time.Now()
	var deadline time.Time
	var ok bool
	if err := CheckPending(context.Background(), "s-1", func(ctx context.Context) (*runtime.PendingInteraction, error) {
		deadline, ok = ctx.Deadline()
		return nil, nil
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatal("the look's context has no deadline; the check is unbounded")
	}
	after := time.Now()
	if deadline.Before(before.Add(PendingCheckTimeout)) || deadline.After(after.Add(PendingCheckTimeout)) {
		t.Fatalf("the look's deadline is %v after the check began, want %v", deadline.Sub(before), PendingCheckTimeout)
	}
	// A caller's earlier deadline wins.
	early, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	want, _ := early.Deadline()
	if err := CheckPending(early, "s-1", func(ctx context.Context) (*runtime.PendingInteraction, error) {
		deadline, _ = ctx.Deadline()
		return nil, nil
	}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !deadline.Equal(want) {
		t.Fatalf("the look's deadline = %v, want the caller's %v", deadline, want)
	}
}
