package reviewquorum

import (
	"fmt"
	"strings"
)

// Finalize synthesizes durable lane outputs into a quorum summary. It returns
// a terminal blocked summary when at least one lane output exists and another
// lane soft-failed transiently; awaiting states are reserved for zero output.
func Finalize(outputs []LaneOutput) Summary {
	lanes := append([]LaneOutput{}, outputs...)
	sortLaneOutputs(lanes)

	summary := Summary{
		Verdict:       VerdictAwaitingReviewers,
		Summary:       "awaiting reviewer lane output",
		Findings:      []Finding{},
		Evidence:      []Evidence{},
		FailureClass:  FailureClassNone,
		FailureReason: "",
		Lanes:         lanes,
	}
	if len(lanes) == 0 {
		return summary
	}

	summary.Verdict = VerdictPass
	summary.Summary = "review quorum passed with no findings"

	findingAccumulators := map[string]Finding{}
	var findingOrder []string
	var laneSummaries []string
	var hardFailures []string
	var transientFailures []string
	for i, lane := range lanes {
		mergeLaneFindings(findingAccumulators, &findingOrder, lane)
		summary.Evidence = append(summary.Evidence, lane.Evidence...)
		summary.Usage = addUsage(summary.Usage, lane.Usage)
		summary.MutationsDelta = mergeMutationDeltas(summary.MutationsDelta, lane.MutationsDelta)
		if i == 0 {
			summary.ReadOnlyEnforcement = cloneReadOnlyEnforcement(lane.ReadOnlyEnforcement)
		} else {
			summary.ReadOnlyEnforcement = mergeReadOnly(summary.ReadOnlyEnforcement, lane.ReadOnlyEnforcement)
		}
		switch reason := readOnlyContractFailure(lane); reason {
		case "":
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
			hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, "unknown_verdict_value"))
		}
	}

	for _, key := range findingOrder {
		summary.Findings = append(summary.Findings, findingAccumulators[key])
	}
	summary.FindingsCount = len(summary.Findings)

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

func mergeLaneFindings(accumulators map[string]Finding, order *[]string, lane LaneOutput) {
	for _, finding := range lane.Findings {
		key := findingKey(finding)
		merged, ok := accumulators[key]
		if !ok {
			merged = cloneFinding(finding)
			merged.Lanes = nil
			merged.Evidence = nil
			*order = append(*order, key)
		}
		merged.Lanes = appendUniqueStrings(merged.Lanes, lane.LaneID)
		merged.Lanes = appendUniqueStrings(merged.Lanes, finding.Lanes...)
		if len(finding.Evidence) > 0 {
			merged.Evidence = append(merged.Evidence, cloneEvidence(finding.Evidence)...)
		} else {
			merged.Evidence = append(merged.Evidence, cloneEvidence(lane.Evidence)...)
		}
		accumulators[key] = merged
	}
}

func findingKey(finding Finding) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%d",
		normalizeToken(finding.Severity),
		normalizeToken(finding.Title),
		strings.TrimSpace(finding.Body),
		strings.TrimSpace(finding.File),
		finding.Start,
		finding.End,
	)
}

func cloneFinding(finding Finding) Finding {
	finding.Lanes = append([]string(nil), finding.Lanes...)
	finding.Evidence = cloneEvidence(finding.Evidence)
	return finding
}

func cloneEvidence(evidence []Evidence) []Evidence {
	return append([]Evidence(nil), evidence...)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, addition := range additions {
		addition = strings.TrimSpace(addition)
		if addition == "" {
			continue
		}
		if _, ok := seen[addition]; ok {
			continue
		}
		seen[addition] = struct{}{}
		values = append(values, addition)
	}
	return values
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

func addUsage(a, b *Usage) *Usage {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		usage := *b
		return &usage
	}
	if b == nil {
		usage := *a
		return &usage
	}
	return &Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		TotalTokens:  a.TotalTokens + b.TotalTokens,
		CostUSD:      a.CostUSD + b.CostUSD,
	}
}

func cloneReadOnlyEnforcement(value ReadOnlyEnforcement) ReadOnlyEnforcement {
	value.Notes = append([]string(nil), value.Notes...)
	return value
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
