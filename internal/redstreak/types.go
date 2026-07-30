// Package redstreak evaluates scheduled GitHub Actions workflow history for
// consecutive red (failing) runs of a designated aggregate job.
package redstreak

import (
	"fmt"
	"time"
)

// Run is one observed GitHub Actions workflow run.
type Run struct {
	ID         int64
	Event      string
	Conclusion string
	StartedAt  time.Time
	URL        string
}

// ObservedRun pairs a workflow run with the jobs executed within it.
type ObservedRun struct {
	Run
	Jobs []Job
}

// Job is one job executed within a workflow run.
type Job struct {
	Name       string
	Conclusion string
}

// Evaluation is the computed red-streak state for one monitor as of the
// latest observed run.
type Evaluation struct {
	Monitor              string
	LatestRunID          int64
	LatestRunURL         string
	AggregateConclusion  string
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	EvaluatedRuns        int
	EvaluatedAt          time.Time
	DataContractError    bool
}

// DataContractError reports that a run's aggregate job could not be
// unambiguously identified (missing or duplicated).
type DataContractError struct {
	RunID        int64
	AggregateJob string
	Matches      int
}

func (e *DataContractError) Error() string {
	return fmt.Sprintf("run %d: aggregate job %q matched %d jobs, want exactly 1", e.RunID, e.AggregateJob, e.Matches)
}
