package prwatchdog

import (
	"context"
	"errors"
	"strings"
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

	awaiting      bool
	awaitingErr   error
	awaitingCalls []string
}

func (f *scriptedFetcher) FetchAwaitingApproval(_ context.Context, headSHA string) (bool, error) {
	f.awaitingCalls = append(f.awaitingCalls, headSHA)
	return f.awaiting, f.awaitingErr
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

func TestWatch_RateLimitWaitsForResetThenRetries(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	reset := base.Add(4 * time.Minute)
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{err: &RateLimitError{Reset: reset, Err: errors.New("API rate limit exceeded for installation")}},
		{runs: passingRequiredRuns()},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if !eval.Pass {
		t.Fatalf("expected a pass once the rate limit reset, got %+v", eval)
	}
	if len(fetcher.calls) != 2 {
		t.Fatalf("expected one retry after the rate limit, got %d fetches", len(fetcher.calls))
	}
	if len(sleeper.calls) != 1 || sleeper.calls[0] != 4*time.Minute {
		t.Fatalf("expected one sleep until the reset (4m), got %v", sleeper.calls)
	}
}

func TestWatch_RateLimitShorterThanIntervalWaitsOneInterval(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{err: &RateLimitError{Reset: base.Add(-time.Second), Err: errors.New("rate limited")}},
		{runs: passingRequiredRuns()},
	}}

	Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if len(sleeper.calls) != 1 || sleeper.calls[0] != 30*time.Second {
		t.Fatalf("expected a stale reset to wait one interval, got %v", sleeper.calls)
	}
}

func TestWatch_RateLimitResettingPastDeadlineFailsClosed(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: base}
	sleeper := &fakeSleeper{clock: clock}
	fetcher := &scriptedFetcher{responses: []fetchResponse{
		{err: &RateLimitError{Reset: base.Add(ObservationDeadline + time.Minute), Err: errors.New("API rate limit exceeded for installation")}},
	}}

	eval := Watch(context.Background(), fetcher, clock, sleeper, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: ObservationDeadline,
		Interval: 30 * time.Second,
	})

	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected fail-closed when the reset is past the deadline, got %+v", eval)
	}
	if len(fetcher.calls) != 1 || len(sleeper.calls) != 0 {
		t.Fatalf("expected no wait past the deadline, got %d fetches, sleeps %v", len(fetcher.calls), sleeper.calls)
	}
}

// passingRequiredRuns is the minimal evidence set that evaluates to a pass:
// Check and CI / required both concluded success on the head commit.
func passingRequiredRuns() []CheckRun {
	started := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	return []CheckRun{
		{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: started},
		{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: started},
	}
}

func watchToDeadline(t *testing.T, fetcher *scriptedFetcher) Evaluation {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	return Watch(context.Background(), fetcher, clock, &fakeSleeper{clock: clock}, PollOptions{
		HeadSHA:  testHeadSHA,
		Deadline: 25 * time.Minute,
		Interval: 5 * time.Minute,
	})
}

func TestWatch_CheckNeverRanChecksForPendingApprovalOnce(t *testing.T) {
	fetcher := &scriptedFetcher{responses: []fetchResponse{{runs: nil}}, awaiting: true}

	eval := watchToDeadline(t, fetcher)

	if eval.Pass || !eval.Terminal || !strings.Contains(eval.Reason, "maintainer approval") {
		t.Fatalf("expected a terminal failure naming maintainer approval, got %+v", eval)
	}
	if len(fetcher.awaitingCalls) != 1 || fetcher.awaitingCalls[0] != testHeadSHA {
		t.Fatalf("approval lookup calls = %v, want exactly one for the explicit head SHA (not once per poll)", fetcher.awaitingCalls)
	}
}

func TestWatch_CheckNeverRanWithoutPendingApprovalKeepsTestsNeverRan(t *testing.T) {
	fetcher := &scriptedFetcher{responses: []fetchResponse{{runs: nil}}, awaiting: false}

	eval := watchToDeadline(t, fetcher)

	if eval.Reason != "tests never ran" {
		t.Fatalf("Reason = %q, want the unchanged %q", eval.Reason, "tests never ran")
	}
	if len(fetcher.awaitingCalls) != 1 {
		t.Fatalf("approval lookup calls = %v, want exactly one", fetcher.awaitingCalls)
	}
}

// The lookup failing must not turn into a silent "tests never ran": the
// reason says the approval check could not be made.
func TestWatch_ApprovalLookupErrorIsReportedNotSwallowed(t *testing.T) {
	fetcher := &scriptedFetcher{responses: []fetchResponse{{runs: nil}}, awaitingErr: errors.New("suites unavailable")}

	eval := watchToDeadline(t, fetcher)

	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected a fail-closed terminal result, got %+v", eval)
	}
	if !strings.HasPrefix(eval.Reason, "tests never ran") || !strings.Contains(eval.Reason, "suites unavailable") {
		t.Fatalf("Reason = %q, want tests-never-ran plus the lookup error", eval.Reason)
	}
}

// Only an ABSENT Check is explained by a pending approval, and only a failing
// run needs explaining, so no other path pays for the extra request.
func TestWatch_NoApprovalLookupUnlessCheckNeverRan(t *testing.T) {
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		runs []CheckRun
	}{
		{"passes", passingRequiredRuns()},
		{"check in progress at deadline", []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusInProgress, StartedAt: base}}},
		{"check failed", []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &scriptedFetcher{responses: []fetchResponse{{runs: tc.runs}}, awaiting: true}

			watchToDeadline(t, fetcher)

			if len(fetcher.awaitingCalls) != 0 {
				t.Fatalf("approval lookup calls = %v, want none", fetcher.awaitingCalls)
			}
		})
	}
}
