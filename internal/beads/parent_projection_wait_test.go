package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAwaitParentProjectionBacksOffWhileUnconverged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var polls atomic.Int32
		reads := parentProjectionReads{
			get: func(string) (Bead, error) {
				polls.Add(1)
				return Bead{ID: "child", ParentID: "old"}, nil
			},
			matches: func(context.Context, string, string, string, bool) (bool, error) {
				t.Error("matches called while the child still reads under its old parent")
				return false, nil
			},
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := awaitParentProjection(ctx, reads, "child", "old", "new")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("awaitParentProjection = %v, want context.DeadlineExceeded", err)
		}
		// Backing off 50ms -> 500ms polls at 0, 50, 150, 350 and 750ms; a
		// fixed 50ms interval would poll 20 times.
		if got := polls.Load(); got != 5 {
			t.Fatalf("polled %d times in 1s, want 5 with backoff", got)
		}
	})
}

func TestAwaitParentProjectionCanceledOnEntryStartsNoRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads atomic.Int32
		backend := parentProjectionReads{
			get: func(string) (Bead, error) {
				reads.Add(1)
				return Bead{ID: "child", ParentID: "new"}, nil
			},
			matches: func(context.Context, string, string, string, bool) (bool, error) {
				reads.Add(1)
				return true, nil
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := awaitParentProjection(ctx, backend, "child", "old", "new")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("awaitParentProjection = %v, want context.Canceled", err)
		}
		// Let any poll started anyway run to completion before counting.
		synctest.Wait()
		if n := reads.Load(); n != 0 {
			t.Fatalf("started %d backend read(s) for a caller whose context had already ended", n)
		}
	})
}

// A poll abandoned while its first read is stalled must not go on to issue
// the listing reads once that read returns.
func TestAwaitParentProjectionAbandonedPollStopsAfterInFlightRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		releaseGet := make(chan struct{})
		var matchesCalls atomic.Int32
		reads := parentProjectionReads{
			get: func(string) (Bead, error) {
				<-releaseGet
				return Bead{ID: "child", ParentID: "new"}, nil
			},
			matches: func(context.Context, string, string, string, bool) (bool, error) {
				matchesCalls.Add(1)
				return true, nil
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- awaitParentProjection(ctx, reads, "child", "old", "new") }()
		synctest.Wait() // the poll is now blocked inside get
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("awaitParentProjection = %v, want context.Canceled", err)
		}
		close(releaseGet)
		synctest.Wait() // the abandoned poll has run to completion
		if n := matchesCalls.Load(); n != 0 {
			t.Fatalf("abandoned poll issued %d listing check(s) after its caller returned", n)
		}
	})
}

// When ctx ends mid-poll, the timeout must still report the last real check
// failure, not the context error that cut the final poll short.
func TestAwaitParentProjectionKeepsLastCheckErrorWhenCanceledMidPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int32
		reads := parentProjectionReads{
			get: func(string) (Bead, error) {
				return Bead{ID: "child", ParentID: "new"}, nil
			},
			matches: func(ctx context.Context, _, _, _ string, _ bool) (bool, error) {
				if calls.Add(1) == 1 {
					return false, errors.New("listing new parent: invalid connection")
				}
				// The caller gives up between this poll's two listings.
				cancel()
				return false, fmt.Errorf("%w: %w", errParentProjectionCutShort, ctx.Err())
			},
		}
		err := awaitParentProjection(ctx, reads, "child", "old", "new")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("awaitParentProjection = %v, want context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "invalid connection") {
			t.Fatalf("awaitParentProjection = %v, want the earlier listing failure as the last check error", err)
		}
	})
}

// The wait error identifies only why the wait stopped. A backend read that
// earlier failed with its own timeout must not make a canceled wait look like
// a deadline; it stays in the message as the last check error.
func TestAwaitParentProjectionErrorIdentityIsTheCallersContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int32
		reads := parentProjectionReads{
			get: func(string) (Bead, error) {
				return Bead{ID: "child", ParentID: "new"}, nil
			},
			matches: func(ctx context.Context, _, _, _ string, _ bool) (bool, error) {
				if calls.Add(1) == 1 {
					return false, fmt.Errorf("listing new parent: bd list: %w", context.DeadlineExceeded)
				}
				cancel()
				return false, fmt.Errorf("%w: %w", errParentProjectionCutShort, ctx.Err())
			},
		}
		err := awaitParentProjection(ctx, reads, "child", "old", "new")
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("awaitParentProjection = %v, want context.Canceled only, not the backend's earlier DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "listing new parent") {
			t.Fatalf("awaitParentProjection = %v, want the backend failure kept as the last check error", err)
		}
	})
}

// A backend read that fails with a context error of its own is a real failure
// worth reporting; only a check stopped by the caller's ctx is cut short.
func TestPollParentProjectionTellsCutShortFromBackendContextErrors(t *testing.T) {
	onNewParent := func(matchErr error) parentProjectionReads {
		return parentProjectionReads{
			get: func(string) (Bead, error) {
				return Bead{ID: "child", ParentID: "new"}, nil
			},
			matches: func(context.Context, string, string, string, bool) (bool, error) {
				return false, matchErr
			},
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	backendErr := fmt.Errorf("native read: %w", context.DeadlineExceeded)
	poll := pollParentProjection(ctx, onNewParent(backendErr), "child", "old", "new")
	if poll.cutShort || poll.done || !errors.Is(poll.err, backendErr) {
		t.Fatalf("poll with a backend timeout = %+v, want a reportable failure carrying it", poll)
	}

	cut := fmt.Errorf("%w: %w", errParentProjectionCutShort, context.Canceled)
	if poll := pollParentProjection(ctx, onNewParent(cut), "child", "old", "new"); !poll.cutShort {
		t.Fatalf("poll stopped between listings = %+v, want cutShort", poll)
	}
}
