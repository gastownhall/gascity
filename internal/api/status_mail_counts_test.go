package api

import (
	"testing"
	"time"
)

func TestStatusMailCountsReturnsStaleWhileRefreshing(t *testing.T) {
	first := StatusMailCounts{Total: 1, Unread: 1}
	refreshed := StatusMailCounts{Total: 2, Unread: 2}
	values := make(chan StatusMailCounts, 2)
	values <- first
	values <- refreshed
	calls := make(chan struct{}, 2)
	releaseRefresh := make(chan struct{})

	s := &Server{}
	s.statusMailCountsComputer = func() StatusMailCounts {
		calls <- struct{}{}
		counts := <-values
		if counts.Total == refreshed.Total {
			<-releaseRefresh
		}
		return counts
	}

	base := time.Now()
	if got := s.cachedStatusMailCounts(base); got != first {
		t.Fatalf("initial counts = %+v, want %+v", got, first)
	}
	<-calls

	start := time.Now()
	got := s.cachedStatusMailCounts(base.Add(statusMailCountsCacheTTL + time.Second))
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("expired cache call took %s, want stale return without waiting for refresh", elapsed)
	}
	if got != first {
		t.Fatalf("expired counts = %+v, want stale %+v", got, first)
	}

	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	close(releaseRefresh)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := s.cachedStatusMailCounts(time.Now()); got == refreshed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("refreshed counts not observed before deadline")
}
