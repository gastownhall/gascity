package prwatchdog

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Fetcher retrieves the current set of check runs for a head commit.
type Fetcher interface {
	FetchCheckRuns(ctx context.Context, headSHA string) ([]CheckRun, error)
}

// Clock reports the current time, abstracted for testability.
type Clock interface {
	Now() time.Time
}

// Sleeper pauses for a duration, abstracted for testability.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration)
}

// RateLimitError reports that the Checks API refused a request because the
// token's rate limit is spent. Reset is when GitHub says the budget refills.
// Unlike every other fetch error it is not a verdict on the PR: Watch waits
// for Reset and polls again, failing closed only if Reset is past the
// deadline.
type RateLimitError struct {
	Reset time.Time
	Err   error
}

// Error describes the refusal and when the budget resets.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited until %s: %v", e.Reset.UTC().Format(time.RFC3339), e.Err)
}

// Unwrap returns the underlying API error.
func (e *RateLimitError) Unwrap() error { return e.Err }

// PollOptions configures a Watch invocation.
type PollOptions struct {
	HeadSHA                  string
	NeedsMacLabel            bool
	NeedsReviewFormulasLabel bool
	Deadline                 time.Duration
	Interval                 time.Duration
}

// Watch polls fetcher for check runs on opts.HeadSHA, evaluating after each
// poll, until the evaluation is terminal or opts.Deadline elapses.
func Watch(ctx context.Context, fetcher Fetcher, clock Clock, sleeper Sleeper, opts PollOptions) Evaluation {
	start := clock.Now()
	for {
		elapsed := clock.Now().Sub(start)
		runs, err := fetcher.FetchCheckRuns(ctx, opts.HeadSHA)

		var limited *RateLimitError
		if errors.As(err, &limited) {
			wait := max(limited.Reset.Sub(clock.Now()), opts.Interval)
			if elapsed+wait < opts.Deadline {
				sleeper.Sleep(ctx, wait)
				continue
			}
		}

		eval := Evaluate(Input{
			HeadSHA:                  opts.HeadSHA,
			CheckRuns:                runs,
			Elapsed:                  elapsed,
			Deadline:                 opts.Deadline,
			NeedsMacLabel:            opts.NeedsMacLabel,
			NeedsReviewFormulasLabel: opts.NeedsReviewFormulasLabel,
			FetchError:               err,
		})
		if eval.Terminal {
			return eval
		}
		sleeper.Sleep(ctx, opts.Interval)
	}
}
