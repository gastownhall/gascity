package t3bridge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// shrinkRetryBackoff makes latestSeqWithBackoff's waits negligible for tests and
// restores the production value on cleanup.
func shrinkRetryBackoff(t *testing.T) {
	t.Helper()
	prev := latestSeqRetryInitialBackoff
	latestSeqRetryInitialBackoff = time.Millisecond
	t.Cleanup(func() { latestSeqRetryInitialBackoff = prev })
}

func TestLatestSeqWithBackoffRetriesThenSucceeds(t *testing.T) {
	shrinkRetryBackoff(t)
	calls := 0
	seq, err := latestSeqWithBackoff(context.Background(), func() (uint64, error) {
		calls++
		if calls < 3 {
			return 0, fmt.Errorf("transient hiccup %d", calls)
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("latestSeqWithBackoff: %v", err)
	}
	if seq != 42 {
		t.Fatalf("seq = %d, want 42", seq)
	}
	if calls != 3 {
		t.Fatalf("LatestSeq calls = %d, want 3", calls)
	}
}

func TestLatestSeqWithBackoffHonorsContextCancel(t *testing.T) {
	shrinkRetryBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := latestSeqWithBackoff(ctx, func() (uint64, error) {
		calls++
		return 0, errors.New("always fails")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls == 0 {
		t.Fatal("expected at least one LatestSeq attempt before honoring cancel")
	}
}

func TestLatestSeqWithBackoffGivesUpAfterMaxAttempts(t *testing.T) {
	shrinkRetryBackoff(t)
	calls := 0
	_, err := latestSeqWithBackoff(context.Background(), func() (uint64, error) {
		calls++
		return 0, fmt.Errorf("attempt %d", calls)
	})
	if err == nil {
		t.Fatal("expected an error after exhausting the attempt budget")
	}
	if calls != 5 {
		t.Fatalf("LatestSeq calls = %d, want 5 (maxAttempts)", calls)
	}
}
