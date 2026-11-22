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

func TestEvaluate_MacOptIn(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	coreOK := []CheckRun{
		{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
		{Name: CIRequiredName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base},
	}

	t.Run("not requested: passes without a Mac run", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: time.Minute, Deadline: ObservationDeadline, NeedsMacLabel: false})
		if !eval.Pass || !eval.Terminal {
			t.Fatalf("expected pass, got %+v", eval)
		}
		if eval.Summary.Mac != "not requested (opt-in)" {
			t.Fatalf("Summary.Mac = %q, want %q", eval.Summary.Mac, "not requested (opt-in)")
		}
	})

	t.Run("requested, missing at deadline: fails", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: ObservationDeadline, Deadline: ObservationDeadline, NeedsMacLabel: true})
		if eval.Pass || !eval.Terminal {
			t.Fatalf("expected fail-closed at deadline, got %+v", eval)
		}
	})

	t.Run("requested, not yet concluded before deadline: keep observing", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: coreOK, Elapsed: 5 * time.Minute, Deadline: ObservationDeadline, NeedsMacLabel: true})
		if eval.Terminal {
			t.Fatalf("expected to keep observing while an explicitly requested Mac run has not concluded, got %+v", eval)
		}
	})

	t.Run("requested and succeeded: passes", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: MacCheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline, NeedsMacLabel: true})
		if !eval.Pass || !eval.Terminal {
			t.Fatalf("expected pass, got %+v", eval)
		}
	})

	t.Run("requested and failed: fails even though core CI passed", func(t *testing.T) {
		runs := append(append([]CheckRun{}, coreOK...), CheckRun{Name: MacCheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionFailure, StartedAt: base})
		eval := Evaluate(Input{HeadSHA: testHeadSHA, CheckRuns: runs, Elapsed: time.Minute, Deadline: ObservationDeadline, NeedsMacLabel: true})
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

// A fork PR's pull_request workflows sit at conclusion=action_required (zero
// jobs) until a maintainer approves them, so Check never appears. That must
// not read as "tests never ran": the contributor cannot act on that, and a
// maintainer reading it blames the contributor.
func TestEvaluate_CheckAbsentAwaitingApprovalNamesMaintainerApproval(t *testing.T) {
	eval := Evaluate(Input{
		HeadSHA:          testHeadSHA,
		Elapsed:          ObservationDeadline,
		Deadline:         ObservationDeadline,
		AwaitingApproval: true,
	})
	if eval.Pass || !eval.Terminal {
		t.Fatalf("expected a terminal failure, got %+v", eval)
	}
	if !strings.Contains(eval.Reason, "maintainer approval") {
		t.Fatalf("Reason = %q, want it to name maintainer approval", eval.Reason)
	}
	if strings.Contains(eval.Reason, "never ran") {
		t.Fatalf("Reason = %q must not say the tests never ran", eval.Reason)
	}
	if eval.Summary.Check != "awaiting maintainer approval" {
		t.Fatalf("Summary.Check = %q, want %q", eval.Summary.Check, "awaiting maintainer approval")
	}
}

// Without evidence of a pending approval the existing message stands, so the
// two causes stay distinguishable.
func TestEvaluate_CheckAbsentWithoutPendingApprovalKeepsTestsNeverRan(t *testing.T) {
	eval := Evaluate(Input{HeadSHA: testHeadSHA, Elapsed: ObservationDeadline, Deadline: ObservationDeadline})
	if eval.Reason != "tests never ran" || eval.Summary.Check != "never ran" {
		t.Fatalf("got Reason %q, Summary.Check %q; want the unchanged tests-never-ran result", eval.Reason, eval.Summary.Check)
	}
}

// A pending approval only explains an ABSENT Check. If Check exists the
// workflow was approved and ran, whatever else is still waiting.
func TestEvaluate_AwaitingApprovalDoesNotRewriteOtherFailures(t *testing.T) {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	t.Run("check still in progress at deadline", func(t *testing.T) {
		eval := Evaluate(Input{
			HeadSHA:          testHeadSHA,
			CheckRuns:        []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusInProgress, StartedAt: base}},
			Elapsed:          ObservationDeadline,
			Deadline:         ObservationDeadline,
			AwaitingApproval: true,
		})
		if eval.Reason != "tests never ran" || eval.Summary.Check != "in progress" {
			t.Fatalf("got Reason %q, Summary.Check %q; want the unchanged in-progress result", eval.Reason, eval.Summary.Check)
		}
	})

	t.Run("check passed, CI / required absent at deadline", func(t *testing.T) {
		eval := Evaluate(Input{
			HeadSHA:          testHeadSHA,
			CheckRuns:        []CheckRun{{Name: CheckName, HeadSHA: testHeadSHA, Status: StatusCompleted, Conclusion: ConclusionSuccess, StartedAt: base}},
			Elapsed:          ObservationDeadline,
			Deadline:         ObservationDeadline,
			AwaitingApproval: true,
		})
		if eval.Reason != "incomplete comprehensive evidence" {
			t.Fatalf("Reason = %q, want the unchanged incomplete-evidence result", eval.Reason)
		}
	})

	t.Run("before the deadline it keeps observing", func(t *testing.T) {
		eval := Evaluate(Input{HeadSHA: testHeadSHA, Elapsed: time.Minute, Deadline: ObservationDeadline, AwaitingApproval: true})
		if eval.Terminal {
			t.Fatalf("expected to keep observing (a maintainer may still approve), got %+v", eval)
		}
	})
}
