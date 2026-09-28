package prwatchdog

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type fakeSleeper struct {
	clock *fakeClock
	calls []time.Duration
}

func (s *fakeSleeper) Sleep(_ context.Context, d time.Duration) {
	s.calls = append(s.calls, d)
	s.clock.now = s.clock.now.Add(d)
}

type fetchResponse struct {
	runs []CheckRun
	err  error
}

type scriptedFetcher struct {
	responses []fetchResponse
	calls     []string
}

func (f *scriptedFetcher) FetchCheckRuns(_ context.Context, headSHA string) ([]CheckRun, error) {
	f.calls = append(f.calls, headSHA)
	idx := len(f.calls) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	r := f.responses[idx]
	return r.runs, r.err
}

func TestWatch_PollsUntilTerminalThenStops(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: nil},
		{runs: []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusInProgress, StartedAt: base}}},
		{runs: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		}},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if !eval.Pass || !eval.Terminal {
		t.Fatalf("expected an eventual pass, got %+v", eval)
	}
	if len(fetcher.calls) != 3 {
		t.Fatalf("expected exactly 3 fetch attempts (stop as soon as terminal), got %d: %v", len(fetcher.calls), fetcher.calls)
	}
	if len(sleeper.calls) != 2 {
		t.Fatalf("expected exactly 2 sleeps between 3 attempts, got %d", len(sleeper.calls))
	}
	for _, headSHA := range fetcher.calls {
		if headSHA != testHeadSHA {
			t.Fatalf("fetcher must always be called with the explicit head SHA, got %q", headSHA)
		}
	}
}

func TestWatch_StopsAtDeadlineWithoutUnboundedPolling(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	// Every attempt reports "still queued" -- this never resolves on its own.
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusQueued, StartedAt: base}}},
	}}

	deadline := 25 * time.Minute
	interval := 5 * time.Minute
	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: deadline,
		Interval: interval,
	})

	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected a fail-closed terminal result at the deadline, got %+v", eval)
	}
	// Bounded: with a 25m deadline and a 5m interval, only a handful of
	// attempts should occur before the deadline check stops the loop --
	// never an unbounded number.
	if len(fetcher.calls) == 0 || len(fetcher.calls) > 10 {
		t.Fatalf("expected a small bounded number of fetch attempts, got %d", len(fetcher.calls))
	}
}

func TestWatch_APIErrorStopsImmediatelyWithoutRetry(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{err: errors.New("simulated rate limit / auth / timeout error")},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected an immediate fail-closed result on API error, got %+v", eval)
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("expected exactly one fetch attempt (no retry on API error), got %d", len(fetcher.calls))
	}
	if len(sleeper.calls) != 0 {
		t.Fatalf("expected no sleep/retry after an API error, got %d sleeps", len(sleeper.calls))
	}
}

func TestWatch_PassesThroughOptInLabels(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{runs: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			// Mac opt-in run omitted deliberately: with NeedsMacLabel true
			// below, this must fail closed at the deadline instead of
			// passing on core evidence alone.
		}},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:       testHeadSHA,
		NeedsMacLabel: true,
		Deadline:      10 * time.Minute,
		Interval:      10 * time.Minute,
	})

	if eval.Pass {
		t.Fatalf("expected fail because the requested Mac run never appeared, got %+v", eval)
	}
}
