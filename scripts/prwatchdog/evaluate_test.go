package prwatchdog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const testHeadSHA = "abc123head"

func TestEvaluate_CoreStateMachine(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		checkRuns  []CheckRun
		elapsed    time.Duration
		wantPass   bool
		wantTerm   bool
		wantReason string
	}{
		{
			name:      "check absent, before deadline: keep observing",
			checkRuns: nil,
			elapsed:   5 * time.Minute,
			wantTerm:  false,
		},
		{
			name:       "check absent at deadline: fail closed, tests never ran",
			checkRuns:  nil,
			elapsed:    ObservationDeadline,
			wantPass:   false,
			wantTerm:   true,
			wantReason: "tests never ran",
		},
		{
			name: "check queued before deadline: keep observing",
			checkRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusQueued, StartedAt: base},
			},
			elapsed:  10 * time.Minute,
			wantTerm: false,
		},
		{
			name: "check in_progress at deadline: fail closed",
			checkRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusInProgress, StartedAt: base},
			},
			elapsed:    ObservationDeadline,
			wantPass:   false,
			wantTerm:   true,
			wantReason: "tests never ran",
		},
		{
			name: "check completed failure: fail immediately, no need to wait for deadline",
			checkRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base},
			},
			elapsed:    2 * time.Minute,
			wantPass:   false,
			wantTerm:   true,
			wantReason: "CI ran but preflight did not pass",
		},
		{
			name: "check success, CI/required absent before deadline: keep observing",
			checkRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			},
			elapsed:  10 * time.Minute,
			wantTerm: false,
		},
		{
			name: "check success, CI/required absent at deadline: fail closed, incomplete evidence",
			checkRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			},
			elapsed:    ObservationDeadline,
			wantPass:   false,
			wantTerm:   true,
			wantReason: "incomplete comprehensive evidence",
		},
		{
			name: "check success, CI/required success: pass",
			checkRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
				{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			},
			elapsed:  3 * time.Minute,
			wantPass: true,
			wantTerm: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := Evaluate(Input{
				HeadSHA:   testHeadSHA,
				CheckRuns: tt.checkRuns,
				Elapsed:   tt.elapsed,
				Deadline:  ObservationDeadline,
			})
			if eval.Terminal != tt.wantTerm {
				t.Fatalf("Terminal = %v, want %v (eval=%+v)", eval.Terminal, tt.wantTerm, eval)
			}
			if tt.wantTerm && eval.Pass != tt.wantPass {
				t.Fatalf("Pass = %v, want %v (eval=%+v)", eval.Pass, tt.wantPass, eval)
			}
			if tt.wantReason != "" && !strings.Contains(eval.Reason, tt.wantReason) {
				t.Fatalf("Reason = %q, want it to contain %q", eval.Reason, tt.wantReason)
			}
		})
	}
}

func TestEvaluate_CIRequiredEveryConclusionFailsExceptSuccess(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	conclusions := []Conclusion{
		ConclusionFailure,
		ConclusionCancelled,
		ConclusionSkipped,
		ConclusionNeutral,
		ConclusionStale,
		ConclusionTimedOut,
		ConclusionActionRequired,
		Conclusion("some_future_unknown_conclusion"),
	}

	for _, c := range conclusions {
		t.Run(string(c), func(t *testing.T) {
			eval := Evaluate(Input{
				HeadSHA: testHeadSHA,
				CheckRuns: []CheckRun{
					{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
					{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: c, StartedAt: base},
				},
				Elapsed:  1 * time.Minute,
				Deadline: ObservationDeadline,
			})
			if !eval.Terminal {
				t.Fatalf("conclusion %q must be terminal immediately, got Terminal=false", c)
			}
			if eval.Pass {
				t.Fatalf("conclusion %q must not pass, got Pass=true", c)
			}
		})
	}
}

func TestEvaluate_DuplicateCheckRunsNewestWins(t *testing.T) {
	older := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	newer := older.Add(10 * time.Minute)

	eval := Evaluate(Input{
		HeadSHA: testHeadSHA,
		CheckRuns: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: older, ID: 1},
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: newer, ID: 2},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: newer, ID: 3},
		},
		Elapsed:  1 * time.Minute,
		Deadline: ObservationDeadline,
	})

	if !eval.Pass {
		t.Fatalf("expected the newer (successful) Check rerun to take precedence over the older failed attempt, got %+v", eval)
	}
}

func TestEvaluate_DuplicateCheckRunsTieBreakOnHigherID(t *testing.T) {
	same := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)

	eval := Evaluate(Input{
		HeadSHA: testHeadSHA,
		CheckRuns: []CheckRun{
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: same, ID: 1},
			{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: same, ID: 2},
			{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: same, ID: 3},
		},
		Elapsed:  1 * time.Minute,
		Deadline: ObservationDeadline,
	})

	if eval.Pass {
		t.Fatalf("expected the higher-ID attempt (the later failure) to win an exact StartedAt tie, got %+v", eval)
	}
}

func TestEvaluate_IgnoresCheckRunsForOtherHeadSHAs(t *testing.T) {
	const staleSHA = "stale-previous-head"
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	eval := Evaluate(Input{
		HeadSHA: testHeadSHA,
		CheckRuns: []CheckRun{
			// Reports success for a different (superseded) head SHA. Must
			// NOT count toward the current head's evaluation, even though
			// it is present in the list.
			{Name: CheckName, HeadSHA: staleSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			{Name: CIRequiredName, HeadSHA: staleSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		},
		Elapsed:  5 * time.Minute,
		Deadline: ObservationDeadline,
	})

	if eval.Terminal {
		t.Fatalf("check-runs scoped to a different head SHA must be ignored entirely; got a terminal result %+v", eval)
	}
}

func TestEvaluate_APIErrorFailsClosedImmediately(t *testing.T) {
	eval := Evaluate(Input{
		HeadSHA:    testHeadSHA,
		Elapsed:    0,
		Deadline:   ObservationDeadline,
		FetchError: errors.New("simulated GitHub API error"),
	})

	if !eval.Terminal || eval.Pass {
		t.Fatalf("a fetch error must fail closed immediately (Terminal=true, Pass=false), got %+v", eval)
	}
}

// TestEvaluate_MacOptIn asserts the watchdog reads Mac's opt-in state
// directly from the Checks API rather than from a caller-supplied label
// flag (ga-ismqdw.1 criterion C): mac-regression.yml now always posts a
// MacVerdictCheckName check run for every head SHA it observes -- a
// neutral conclusion when its own gate decided no tier applied, and a
// success/failure conclusion once a tier actually ran. This removes the
// prior double bookkeeping where pr-evidence-watchdog.yml computed its own
// NEEDS_MAC_LABEL from the PR label while mac-regression.yml's gate could
// also trigger from a mac-sensitive path hit -- the two could diverge.
func TestEvaluate_MacOptIn(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	coreOK := []CheckRun{
		{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
	}

	t.Run("verdict neutral: passes, mac reported not requested", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: "Mac Regression verdict", HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionNeutral, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline})
		if !eval.Pass || !eval.Terminal {
			t.Fatalf("expected pass, got %+v", eval)
		}
		if eval.Summary.Mac != "not requested (opt-in)" {
			t.Fatalf("Summary.Mac = %q, want %q", eval.Summary.Mac, "not requested (opt-in)")
		}
	})

	t.Run("verdict missing at deadline: fails", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: ObservationDeadline, Deadline: ObservationDeadline})
		if eval.Pass || !eval.Terminal {
			t.Fatalf("expected fail-closed at deadline, got %+v", eval)
		}
	})

	t.Run("verdict not yet posted before deadline: keep observing", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: 5 * time.Minute, Deadline: ObservationDeadline})
		if eval.Terminal {
			t.Fatalf("expected to keep observing while the Mac verdict check run has not appeared, got %+v", eval)
		}
	})

	t.Run("verdict succeeded: passes", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: "Mac Regression verdict", HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline})
		if !eval.Pass || !eval.Terminal {
			t.Fatalf("expected pass, got %+v", eval)
		}
	})

	t.Run("verdict failed: fails even though core CI passed", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: "Mac Regression verdict", HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline})
		if eval.Pass || !eval.Terminal {
			t.Fatalf("expected fail, got %+v", eval)
		}
	})
}

func TestEvaluate_ReviewFormulasOptIn(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	coreOK := []CheckRun{
		{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
	}

	t.Run("not requested: passes without a review-formulas run", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: time.Minute, Deadline: ObservationDeadline, NeedsReviewFormulasLabel: false})
		if !eval.Pass || !eval.Terminal {
			t.Fatalf("expected pass, got %+v", eval)
		}
		if eval.Summary.ReviewFormulas != "not explicitly requested; path routing may still apply" {
			t.Fatalf("Summary.ReviewFormulas = %q, want %q", eval.Summary.ReviewFormulas, "not explicitly requested; path routing may still apply")
		}
	})

	t.Run("requested, missing at deadline: fails", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: ObservationDeadline, Deadline: ObservationDeadline, NeedsReviewFormulasLabel: true})
		if eval.Pass || !eval.Terminal {
			t.Fatalf("expected fail-closed at deadline, got %+v", eval)
		}
	})

	t.Run("requested, not yet concluded before deadline: keep observing", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: 5 * time.Minute, Deadline: ObservationDeadline, NeedsReviewFormulasLabel: true})
		if eval.Terminal {
			t.Fatalf("expected to keep observing while an explicitly requested review-formulas run has not concluded, got %+v", eval)
		}
	})

	t.Run("requested and succeeded: passes", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: ReviewFormulasCheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline, NeedsReviewFormulasLabel: true})
		if !eval.Pass || !eval.Terminal {
			t.Fatalf("expected pass, got %+v", eval)
		}
	})

	t.Run("requested and failed: fails even though core CI passed", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: ReviewFormulasCheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline, NeedsReviewFormulasLabel: true})
		if eval.Pass || !eval.Terminal {
			t.Fatalf("expected fail, got %+v", eval)
		}
	})
}

func TestEvaluate_HumanReadableSummaryStates(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	t.Run("nothing observed at deadline", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, Elapsed: ObservationDeadline, Deadline: ObservationDeadline})
		if eval.Summary.Check != "never ran" {
			t.Fatalf("Summary.Check = %q, want %q", eval.Summary.Check, "never ran")
		}
	})

	t.Run("check succeeded, CI/required missing at deadline", func(t *testing.T) {
		eval := Evaluate(Input{
			HeadSHA: testHeadSHA,
			CheckRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			},
			Elapsed:  ObservationDeadline,
			Deadline: ObservationDeadline,
		})
		if eval.Summary.Check != "success" {
			t.Fatalf("Summary.Check = %q, want %q", eval.Summary.Check, "success")
		}
		if eval.Summary.CIRequired != "incomplete" {
			t.Fatalf("Summary.CIRequired = %q, want %q", eval.Summary.CIRequired, "incomplete")
		}
	})

	t.Run("both succeeded", func(t *testing.T) {
		eval := Evaluate(Input{
			HeadSHA: testHeadSHA,
			CheckRuns: []CheckRun{
				{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
				{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
			},
			Elapsed:  time.Minute,
			Deadline: ObservationDeadline,
		})
		if eval.Summary.CIRequired != "success" {
			t.Fatalf("Summary.CIRequired = %q, want %q", eval.Summary.CIRequired, "success")
		}
	})
}
