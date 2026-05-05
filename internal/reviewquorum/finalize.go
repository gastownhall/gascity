package reviewquorum

import (
	"fmt"
	"strings"
)

// Finalize synthesizes durable lane outputs into a quorum summary. It returns
// a terminal blocked summary when at least one lane output exists and another
// lane soft-failed transiently; awaiting states are reserved for zero output.
func Finalize(outputs []LaneOutput) Summary {
	lanes := append([]LaneOutput(nil), outputs...)
	sortLaneOutputs(lanes)

	summary := Summary{
		Verdict:             VerdictAwaitingReviewers,
		Summary:             "awaiting reviewer lane output",
		ReadOnlyEnforcement: ReadOnlyEnforcement{Enabled: true, Passed: true},
		Lanes:               lanes,
	}
	if len(lanes) == 0 {
		return summary
	}

	summary.Verdict = VerdictPass
	summary.Summary = "review quorum passed with no findings"

	var laneSummaries []string
	var hardFailures []string
	var transientFailures []string
	for _, lane := range lanes {
		count := normalizedFindingsCount(lane)
		summary.FindingsCount += count
		summary.Findings = append(summary.Findings, lane.Findings...)
		summary.Evidence = append(summary.Evidence, lane.Evidence...)
		summary.Usage = addUsage(summary.Usage, lane.Usage)
		summary.MutationsDelta = mergeMutationDeltas(summary.MutationsDelta, lane.MutationsDelta)
		summary.ReadOnlyEnforcement = mergeReadOnly(summary.ReadOnlyEnforcement, lane.ReadOnlyEnforcement)

		if strings.TrimSpace(lane.Summary) != "" {
			laneSummaries = append(laneSummaries, fmt.Sprintf("%s: %s", lane.LaneID, strings.TrimSpace(lane.Summary)))
		}
		if lane.FailureClass != "" || lane.FailureReason != "" {
			class, reason := ClassifyFailure(lane.FailureClass, lane.FailureReason)
			if class == FailureClassTransient {
				transientFailures = append(transientFailures, formatLaneFailure(lane.LaneID, reason))
			} else {
				hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, reason))
			}
			continue
		}
		switch normalizeToken(lane.Verdict) {
		case VerdictPassWithFindings:
			summary.Verdict = VerdictPassWithFindings
		case VerdictFail:
			hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, "lane_failed"))
		case VerdictBlocked:
			class, reason := ClassifyFailure(lane.FailureClass, lane.FailureReason)
			if class == FailureClassTransient {
				transientFailures = append(transientFailures, formatLaneFailure(lane.LaneID, reason))
			} else {
				hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, reason))
			}
		}
	}

	switch {
	case len(hardFailures) > 0:
		summary.Verdict = VerdictFail
		summary.FailureClass = FailureClassHard
		summary.FailureReason = strings.Join(hardFailures, "; ")
		summary.Summary = "review quorum failed: " + summary.FailureReason
	case summary.FindingsCount > 0:
		summary.Verdict = VerdictPassWithFindings
		summary.Summary = fmt.Sprintf("review quorum found %d finding(s)", summary.FindingsCount)
	case len(transientFailures) > 0:
		summary.Verdict = VerdictBlocked
		summary.FailureClass = FailureClassTransient
		summary.FailureReason = strings.Join(transientFailures, "; ")
		summary.Summary = "review quorum blocked with degraded coverage: " + summary.FailureReason
	case len(laneSummaries) > 0:
		summary.Summary = strings.Join(laneSummaries, "\n")
	}

	if len(summary.MutationsDelta.Changed) > 0 && summary.Verdict == VerdictPass {
		summary.Verdict = VerdictFail
		summary.FailureClass = FailureClassHard
		summary.FailureReason = "read_only_mutation_detected"
		summary.Summary = "review lane mutated the worktree"
	}
	return summary
}

func addUsage(a, b Usage) Usage {
	return Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
		CostUSD:      a.CostUSD + b.CostUSD,
	}
}

func mergeReadOnly(a, b ReadOnlyEnforcement) ReadOnlyEnforcement {
	enabled := a.Enabled || b.Enabled
	passed := a.Passed
	if b.Enabled && !b.Passed {
		passed = false
	}
	notes := append(append([]string(nil), a.Notes...), b.Notes...)
	return ReadOnlyEnforcement{Enabled: enabled, Passed: passed, Notes: notes}
}

func formatLaneFailure(laneID, reason string) string {
	if laneID == "" {
		laneID = "unknown_lane"
	}
	if reason == "" {
		reason = "unspecified"
	}
	return laneID + ":" + reason
}
