// Package prwatchdog evaluates whether a pull request has produced complete,
// passing CI evidence for its current head commit.
package prwatchdog

import (
	"fmt"
	"time"
)

// Status mirrors a GitHub check run's status field.
type Status string

// Status values a GitHub check run can report.
const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Conclusion mirrors a GitHub check run's conclusion field, valid once
// Status is StatusCompleted.
type Conclusion string

// Conclusion values a GitHub check run can report once completed.
const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionFailure        Conclusion = "failure"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionCancelled      Conclusion = "canceled"
	ConclusionSkipped        Conclusion = "skipped"
	ConclusionTimedOut       Conclusion = "timed_out"
	ConclusionActionRequired Conclusion = "action_required"
	ConclusionStale          Conclusion = "stale"
)

const (
	// CheckName is the preflight check run that must succeed before
	// CIRequiredName is considered meaningful.
	CheckName = "Check"
	// CIRequiredName is the comprehensive required CI check run.
	CIRequiredName = "CI / required"
	// MacCheckName is the opt-in macOS regression check run.
	MacCheckName = "Mac regression summary"
	// ReviewFormulasCheckName is the opt-in formula-review check run.
	ReviewFormulasCheckName = "Integration / review-formulas"
	// RequiredCheckName is the name this watchdog publishes as its own
	// required check run.
	RequiredCheckName = "Evidence / critical-path suites"
	// ObservationDeadline bounds how long the watchdog observes a head
	// commit before failing closed.
	ObservationDeadline = 25 * time.Minute
)

// CheckRun is a single GitHub check run observation for a head commit.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     Status
	Conclusion Conclusion
	StartedAt  time.Time
	ID         int64
}

// Input is the observed state for one evaluation of a pull request head.
type Input struct {
	HeadSHA                  string
	CheckRuns                []CheckRun
	Elapsed                  time.Duration
	Deadline                 time.Duration
	NeedsMacLabel            bool
	NeedsReviewFormulasLabel bool
	FetchError               error
}

// Summary is a human-readable rendering of each tracked check's state.
type Summary struct {
	Check          string
	CIRequired     string
	Mac            string
	ReviewFormulas string
}

// Evaluation is the result of evaluating an Input.
type Evaluation struct {
	Pass     bool
	Terminal bool
	Reason   string
	Summary  Summary
}

// checkState classifies the latest observed run for one tracked check name.
type checkState int

const (
	stateAbsent checkState = iota
	stateQueuedOrInProgress
	stateCompletedSuccess
	stateCompletedOther
)

// latestRun returns the most recent CheckRun named name scoped to headSHA,
// preferring the later StartedAt and, on an exact tie, the higher ID (the
// later of two same-instant reruns).
func latestRun(runs []CheckRun, headSHA, name string) (CheckRun, bool) {
	var best CheckRun
	found := false
	for _, r := range runs {
		if r.HeadSHA != headSHA || r.Name != name {
			continue
		}
		if !found || r.StartedAt.After(best.StartedAt) || (r.StartedAt.Equal(best.StartedAt) && r.ID > best.ID) {
			best = r
			found = true
		}
	}
	return best, found
}

func stateOf(runs []CheckRun, headSHA, name string) (checkState, CheckRun) {
	run, found := latestRun(runs, headSHA, name)
	if !found {
		return stateAbsent, CheckRun{}
	}
	if run.Status != StatusCompleted {
		return stateQueuedOrInProgress, run
	}
	if run.Conclusion == ConclusionSuccess {
		return stateCompletedSuccess, run
	}
	return stateCompletedOther, run
}

type verdict int

const (
	verdictWait verdict = iota
	verdictFail
	verdictPass
)

// failKind distinguishes why a gate failed: a check that never concluded in
// time reads very differently from one that concluded and did not succeed.
type failKind int

const (
	failNone failKind = iota
	failNotConcluded
	failNonSuccess
)

// evaluateGate inspects one tracked check name and reports whether the
// watchdog should keep waiting, treat it as a terminal failure, or move on.
// absentWord/inProgressWord are the Summary text used while the check has
// not concluded -- Check's "never ran" and the comprehensive checks'
// "incomplete" use different vocabulary for the same underlying state.
func evaluateGate(runs []CheckRun, headSHA, name, absentWord, inProgressWord string, atDeadline bool) (verdict, string, failKind) {
	state, run := stateOf(runs, headSHA, name)
	switch state {
	case stateAbsent:
		if atDeadline {
			return verdictFail, absentWord, failNotConcluded
		}
		return verdictWait, absentWord, failNone
	case stateQueuedOrInProgress:
		if atDeadline {
			return verdictFail, inProgressWord, failNotConcluded
		}
		return verdictWait, inProgressWord, failNone
	case stateCompletedOther:
		return verdictFail, string(run.Conclusion), failNonSuccess
	default: // stateCompletedSuccess
		return verdictPass, "success", failNone
	}
}

// Evaluate inspects the observed check runs for in.HeadSHA and decides
// whether the watchdog should keep observing, or stop with a pass/fail
// verdict.
func Evaluate(in Input) Evaluation {
	if in.FetchError != nil {
		return Evaluation{Terminal: true, Reason: fmt.Sprintf("fetching CI evidence: %v", in.FetchError)}
	}

	atDeadline := in.Elapsed >= in.Deadline
	var summary Summary

	v, word, fk := evaluateGate(in.CheckRuns, in.HeadSHA, CheckName, "never ran", "in progress", atDeadline)
	summary.Check = word
	switch {
	case v == verdictWait:
		return Evaluation{Summary: summary}
	case v == verdictFail && fk == failNotConcluded:
		return Evaluation{Terminal: true, Reason: "tests never ran", Summary: summary}
	case v == verdictFail:
		return Evaluation{Terminal: true, Reason: "CI ran but preflight did not pass", Summary: summary}
	}

	v, word, fk = evaluateGate(in.CheckRuns, in.HeadSHA, CIRequiredName, "incomplete", "incomplete", atDeadline)
	summary.CIRequired = word
	switch {
	case v == verdictWait:
		return Evaluation{Summary: summary}
	case v == verdictFail && fk == failNotConcluded:
		return Evaluation{Terminal: true, Reason: "incomplete comprehensive evidence", Summary: summary}
	case v == verdictFail:
		return Evaluation{Terminal: true, Reason: fmt.Sprintf("%s concluded %q, not success", CIRequiredName, word), Summary: summary}
	}

	if !in.NeedsMacLabel {
		summary.Mac = "not requested (opt-in)"
	} else {
		v, word, fk = evaluateGate(in.CheckRuns, in.HeadSHA, MacCheckName, "incomplete", "incomplete", atDeadline)
		summary.Mac = word
		switch {
		case v == verdictWait:
			return Evaluation{Summary: summary}
		case v == verdictFail && fk == failNotConcluded:
			return Evaluation{Terminal: true, Reason: "Mac regression requested but incomplete", Summary: summary}
		case v == verdictFail:
			return Evaluation{Terminal: true, Reason: fmt.Sprintf("%s concluded %q, not success", MacCheckName, word), Summary: summary}
		}
	}

	if !in.NeedsReviewFormulasLabel {
		summary.ReviewFormulas = "not explicitly requested; path routing may still apply"
	} else {
		v, word, fk = evaluateGate(in.CheckRuns, in.HeadSHA, ReviewFormulasCheckName, "incomplete", "incomplete", atDeadline)
		summary.ReviewFormulas = word
		switch {
		case v == verdictWait:
			return Evaluation{Summary: summary}
		case v == verdictFail && fk == failNotConcluded:
			return Evaluation{Terminal: true, Reason: "Review formulas requested but incomplete", Summary: summary}
		case v == verdictFail:
			return Evaluation{Terminal: true, Reason: fmt.Sprintf("%s concluded %q, not success", ReviewFormulasCheckName, word), Summary: summary}
		}
	}

	return Evaluation{Terminal: true, Pass: true, Reason: "all required CI evidence succeeded", Summary: summary}
}
