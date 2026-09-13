package reviewquorum

import (
	"fmt"
	"strings"
)

// Finalize synthesizes durable lane outputs into a quorum summary for subject
// relative to baseRef. The identity arguments are required durable summary
// fields. It returns awaiting only for zero lane output, hard failure for
// summary identity drift, lane contract violations, read-only mutations,
// unknown verdict values, or explicit lane failures, transient blocked for
// retryable lane failures, and pass/pass_with_findings otherwise.
func Finalize(subject, baseRef string, outputs []LaneOutput) Summary {
	lanes := cloneLaneOutputs(outputs)
	sortLaneOutputs(lanes)

	summary := Summary{
		Subject:       strings.TrimSpace(subject),
		BaseRef:       strings.TrimSpace(baseRef),
		Verdict:       VerdictAwaitingReviewers,
		Summary:       "awaiting reviewer lane output",
		Findings:      []Finding{},
		Evidence:      []Evidence{},
		FailureClass:  FailureClassNone,
		FailureReason: "",
		Lanes:         lanes,
	}
	var hardFailures []string
	if summary.Subject == "" {
		hardFailures = append(hardFailures, "summary_subject_missing")
	}
	if summary.BaseRef == "" {
		hardFailures = append(hardFailures, "summary_base_ref_missing")
	}
	if len(lanes) == 0 {
		if len(hardFailures) > 0 {
			summary.Verdict = VerdictFail
			summary.FailureClass = FailureClassHard
			summary.FailureReason = strings.Join(hardFailures, "; ")
			summary.Summary = "review quorum failed: " + summary.FailureReason
		}
		return summary
	}

	summary.Verdict = VerdictPass
	summary.Summary = "review quorum passed with no findings"

	findingAccumulators := map[string]Finding{}
	var findingOrder []string
	var laneSummaries []string
	var transientFailures []string
	readOnlyMerged := false
	for _, lane := range lanes {
		laneFailures := laneContractFailures(lane)
		for _, reason := range laneFailures {
			hardFailures = append(hardFailures, formatLaneFailure(lane.LaneID, reason))
		}
		if len(laneFailures) > 0 {
			continue
		}
		mergeLaneFindings(findingAccumulators, &findingOrder, lane)
		summary.Evidence = append(summary.Evidence, cloneEvidence(lane.Evidence)...)
		summary.Usage = addUsage(summary.Usage, lane.Usage)
		if !readOnlyMerged {
			summary.ReadOnlyEnforcement = cloneReadOnlyEnforcement(lane.ReadOnlyEnforcement)
			readOnlyMerged = true
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

func laneContractFailures(lane LaneOutput) []string {
	var failures []string
	if strings.TrimSpace(lane.LaneID) == "" {
		failures = append(failures, "lane_id_missing")
	}
	if strings.TrimSpace(lane.Provider) == "" {
		failures = append(failures, "provider_missing")
	}
	if strings.TrimSpace(lane.Model) == "" {
		failures = append(failures, "model_missing")
	}
	if len(failures) > 0 {
		return failures
	}
	if lane.FindingsCount != len(lane.Findings) {
		failures = append(failures, "findings_count_mismatch")
	}
	switch normalizeToken(lane.Verdict) {
	case VerdictPass, VerdictPassWithFindings:
		switch class := normalizeToken(lane.FailureClass); class {
		case "":
			failures = append(failures, "failure_class_missing")
		case FailureClassNone:
			if strings.TrimSpace(lane.FailureReason) != "" {
				_, reason := ClassifyFailure(lane.FailureClass, lane.FailureReason)
				failures = append(failures, reason)
			}
		case FailureClassTransient, FailureClassHard:
			failures = append(failures, "success_failure_class_not_none")
		default:
			_, reason := ClassifyFailure(lane.FailureClass, lane.FailureReason)
			failures = append(failures, reason)
		}
	}
	switch normalizeToken(lane.Verdict) {
	case VerdictPassWithFindings, VerdictFail:
		if len(lane.Findings) == 0 {
			failures = append(failures, "materialized_findings_missing")
		}
	}
	for _, finding := range lane.Findings {
		if len(finding.Lanes) > 0 {
			failures = append(failures, "finding_lanes_not_allowed")
			break
		}
	}
	return failures
}

func mergeLaneFindings(accumulators map[string]Finding, order *[]string, lane LaneOutput) {
	for _, finding := range lane.Findings {
		key := findingKey(finding)
		merged, ok := accumulators[key]
		if !ok {
			merged = finding
			merged.Lanes = nil
			merged.Evidence = nil
			*order = append(*order, key)
		}
		merged.Lanes = appendUniqueStrings(merged.Lanes, lane.LaneID)
		if len(finding.Evidence) > 0 {
			merged.Evidence = append(merged.Evidence, cloneEvidence(finding.Evidence)...)
		} else if len(merged.Evidence) == 0 {
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

func cloneEvidence(evidence []Evidence) []Evidence {
	if evidence == nil {
		return []Evidence{}
	}
	return append([]Evidence(nil), evidence...)
}

func cloneLaneOutputs(outputs []LaneOutput) []LaneOutput {
	lanes := make([]LaneOutput, len(outputs))
	for i, output := range outputs {
		lanes[i] = cloneLaneOutput(output)
	}
	return lanes
}

func cloneLaneOutput(output LaneOutput) LaneOutput {
	output.Findings = cloneFindings(output.Findings)
	output.Evidence = cloneEvidence(output.Evidence)
	output.Usage = cloneUsage(output.Usage)
	output.ReadOnlyEnforcement = cloneReadOnlyEnforcement(output.ReadOnlyEnforcement)
	output.MutationsDelta = cloneMutationsDelta(output.MutationsDelta)
	return output
}

func cloneFindings(findings []Finding) []Finding {
	if findings == nil {
		return []Finding{}
	}
	cloned := make([]Finding, len(findings))
	for i, finding := range findings {
		finding.Lanes = append([]string(nil), finding.Lanes...)
		finding.Evidence = cloneEvidence(finding.Evidence)
		cloned[i] = finding
	}
	return cloned
}

func cloneUsage(usage *Usage) *Usage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	return &cloned
}

func cloneMutationsDelta(delta MutationsDelta) MutationsDelta {
	delta.Changed = append([]StatusEntry(nil), delta.Changed...)
	return delta
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
	if lane.ReadOnlyEnforcement.Passed && strings.TrimSpace(lane.ReadOnlyEnforcement.BaselineCommand) == "" {
		return "read_only_baseline_command_missing"
	}
	if lane.ReadOnlyEnforcement.Passed && strings.TrimSpace(lane.ReadOnlyEnforcement.AfterCommand) == "" {
		return "read_only_after_command_missing"
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
