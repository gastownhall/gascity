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
		Verdict: VerdictAwaitingReviewers,
		Summary: "awaiting reviewer lane output",
		Lanes:   lanes,
	}
	if len(lanes) == 0 {
		return summary
	}

	summary.Verdict = VerdictPass
	summary.Summary = "review quorum passed with no findings"

	var laneSummaries []string
	var hardFailures []string
	var transientFailures []string
	var readOnlyMutated bool
	for i, lane := range lanes {
		count := normalizedFindingsCount(lane)
		summary.FindingsCount += count
		summary.Findings = append(summary.Findings, lane.Findings...)
		summary.Evidence = append(summary.Evidence, lane.Evidence...)
		summary.Usage = addUsage(summary.Usage, lane.Usage)
		summary.MutationsDelta = mergeMutationDeltas(summary.MutationsDelta, lane.MutationsDelta)
		if i == 0 {
			summary.ReadOnlyEnforcement = lane.ReadOnlyEnforcement
		} else {
			summary.ReadOnlyEnforcement = mergeReadOnly(summary.ReadOnlyEnforcement, lane.ReadOnlyEnforcement)
		}
		switch reason := readOnlyContractFailure(lane); reason {
		case "":
		case "read_only_mutation_detected":
			readOnlyMutated = true
		default:
			hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, reason))
		}

		if strings.TrimSpace(lane.Summary) != "" {
			laneSummaries = append(laneSummaries, fmt.Sprintf("%s: %s", lane.LaneID, strings.TrimSpace(lane.Summary)))
		}
		if lane.FailureClass != "" || lane.FailureReason != "" {
			class, reason := ClassifyFailure(lane.FailureClass, lane.FailureReason)
			if class != FailureClassNone {
				if class == FailureClassTransient {
					transientFailures = append(transientFailures, formatLaneFailure(lane.LaneID, reason))
				} else {
					hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, reason))
				}
				continue
			}
		}
		switch normalizeToken(lane.Verdict) {
		case VerdictPass:
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
		default:
			transientFailures = append(transientFailures, formatLaneFailure(lane.LaneID, "unknown_verdict_value"))
		}
	}

	if readOnlyMutated {
		hardFailures = append([]string{"read_only_mutation_detected"}, hardFailures...)
	}

	switch {
	case len(hardFailures) > 0:
		summary.Verdict = VerdictFail
		summary.FailureClass = FailureClassHard
		summary.FailureReason = strings.Join(hardFailures, "; ")
		summary.Summary = "review quorum failed: " + summary.FailureReason
	case len(transientFailures) > 0:
		summary.Verdict = VerdictBlocked
		summary.FailureClass = FailureClassTransient
		summary.FailureReason = strings.Join(transientFailures, "; ")
		summary.Summary = "review quorum blocked with degraded coverage: " + summary.FailureReason
	case summary.FindingsCount > 0:
		summary.Verdict = VerdictPassWithFindings
		summary.Summary = fmt.Sprintf("review quorum found %d finding(s)", summary.FindingsCount)
	case len(laneSummaries) > 0:
		summary.Summary = strings.Join(laneSummaries, "\n")
	}

	return summary
}

func readOnlyContractFailure(lane LaneOutput) string {
	if !lane.ReadOnlyEnforcement.Observed {
		return "read_only_enforcement_missing"
	}
	if !lane.ReadOnlyEnforcement.Enabled {
		return "read_only_enforcement_disabled"
	}
	if len(lane.MutationsDelta.Changed) > 0 {
		return "read_only_mutation_detected"
	}
	if !lane.ReadOnlyEnforcement.Passed {
		return "read_only_mutation_detected"
	}
	return ""
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
	notes := append(append([]string(nil), a.Notes...), b.Notes...)
	return ReadOnlyEnforcement{
		Observed:        a.Observed && b.Observed,
		Enabled:         a.Enabled && b.Enabled,
		Passed:          a.Passed && b.Passed,
		BaselineCommand: firstNonEmpty(a.BaselineCommand, b.BaselineCommand),
		AfterCommand:    firstNonEmpty(a.AfterCommand, b.AfterCommand),
		Notes:           notes,
	}
}

func formatLaneFailure(laneID, reason string) string {
	laneID = normalizeFailureFragment(laneID, "unknown_lane")
	reason = normalizeFailureFragment(reason, "unspecified")
	return "lane=" + laneID + " reason=" + reason
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func normalizeFailureFragment(value, fallback string) string {
	value = normalizeToken(value)
	if value == "" {
		return fallback
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	normalized := strings.Trim(b.String(), "_")
	if normalized == "" {
		return fallback
	}
	return normalized
}
