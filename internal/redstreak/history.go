package redstreak

import (
	"fmt"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// EvaluateHistory reconstructs the consecutive red/green streak of a
// monitor's aggregate job from its observed run history.
//
// Runs are first filtered to the monitor's trigger event and activation
// boundary, then walked newest-to-oldest counting the consecutive run of
// matching aggregate-job conclusions. The activation boundary is applied
// before the aggregate-job data contract is resolved, so pre-enrollment
// runs can never trip a data-contract error even if they carry no job data.
func EvaluateHistory(monitor config.GitHubRunMonitor, runs []ObservedRun) (Evaluation, error) {
	event := monitor.EventOrDefault()
	activatedAfter, err := time.Parse(time.RFC3339, monitor.ActivatedAfter)
	if err != nil {
		return Evaluation{}, fmt.Errorf("parse activated_after %q: %w", monitor.ActivatedAfter, err)
	}

	filtered := make([]ObservedRun, 0, len(runs))
	for _, run := range runs {
		if run.Event != event {
			continue
		}
		if !run.StartedAt.After(activatedAfter) {
			continue
		}
		filtered = append(filtered, run)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].StartedAt.After(filtered[j].StartedAt)
	})

	eval := Evaluation{EvaluatedRuns: len(filtered)}
	if len(filtered) == 0 {
		return eval, nil
	}
	eval.LatestRunID = filtered[0].ID
	eval.LatestRunURL = filtered[0].URL
	eval.FirstRunID = filtered[len(filtered)-1].ID
	eval.FirstRunURL = filtered[len(filtered)-1].URL

	for idx, run := range filtered {
		conclusion, matches := resolveAggregateConclusion(monitor.AggregateJob, run.Jobs)
		contractBroken := matches != 1
		if idx == 0 && !contractBroken {
			eval.AggregateConclusion = conclusion
		}
		isSuccess := !contractBroken && conclusion == "success"

		if isSuccess {
			if eval.ConsecutiveFailures > 0 {
				break
			}
			eval.ConsecutiveSuccesses++
		} else {
			if eval.ConsecutiveSuccesses > 0 {
				break
			}
			eval.ConsecutiveFailures++
		}

		if contractBroken {
			eval.DataContractError = true
			return eval, &DataContractError{RunID: run.ID, AggregateJob: monitor.AggregateJob, Matches: matches}
		}
	}
	return eval, nil
}

// resolveAggregateConclusion returns the aggregate job's own conclusion and
// how many jobs in the run matched its name. Exactly one match is the valid
// data contract; zero or multiple indicate the aggregate job could not be
// unambiguously resolved for that run.
func resolveAggregateConclusion(name string, jobs []Job) (conclusion string, matches int) {
	for _, job := range jobs {
		if job.Name == name {
			matches++
			conclusion = job.Conclusion
		}
	}
	return conclusion, matches
}
