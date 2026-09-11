package prwatchdog

import (
	"context"
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
