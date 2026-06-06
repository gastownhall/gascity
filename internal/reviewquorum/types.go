// Package reviewquorum defines the durable contract for Gas City review
// quorum lanes and synthesis.
package reviewquorum

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// FailureClassNone records a lane outcome with no infrastructure failure.
	FailureClassNone = "none"
	// FailureClassTransient records a retryable infrastructure failure.
	FailureClassTransient = "transient"
	// FailureClassHard records a non-retryable infrastructure failure.
	FailureClassHard = "hard"

	// VerdictPass records a lane approval with no findings.
	VerdictPass = "pass"
	// VerdictPassWithFindings records approval with non-blocking findings.
	VerdictPassWithFindings = "pass_with_findings"
	// VerdictFail records a lane rejection.
	VerdictFail = "fail"
	// VerdictBlocked records a lane that could not complete review.
	VerdictBlocked = "blocked"
	// VerdictAwaitingReviewers records that quorum cannot finalize yet.
	VerdictAwaitingReviewers = "awaiting_reviewers"
)

// LaneConfig describes one reviewer lane in the quorum.
type LaneConfig struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ValidateLaneConfigs checks the generic lane invariants required by the
// durable contract.
func ValidateLaneConfigs(lanes []LaneConfig) error {
	seen := map[string]struct{}{}
	for _, lane := range lanes {
		if lane.ID == "" {
			return fmt.Errorf("lane id is required")
		}
		if lane.ID != strings.ToLower(lane.ID) {
			return fmt.Errorf("lane id %q must be lowercase", lane.ID)
		}
		if strings.TrimSpace(lane.ID) != lane.ID || strings.ContainsAny(lane.ID, " \t\n\r") {
			return fmt.Errorf("lane id %q must not contain whitespace", lane.ID)
		}
		if _, ok := seen[lane.ID]; ok {
			return fmt.Errorf("lane id %q is duplicated", lane.ID)
		}
		seen[lane.ID] = struct{}{}
		if lane.Provider == "" {
			return fmt.Errorf("lane %q provider is required", lane.ID)
		}
		if lane.Model == "" {
			return fmt.Errorf("lane %q model is required", lane.ID)
		}
	}
	return nil
}

// LaneOutput is the durable JSON payload produced by one reviewer lane.
type LaneOutput struct {
	LaneID              string              `json:"lane_id"`
	Provider            string              `json:"provider"`
	Model               string              `json:"model"`
	Verdict             string              `json:"verdict"`
	Summary             string              `json:"summary"`
	FindingsCount       int                 `json:"findings_count"`
	Findings            []Finding           `json:"findings"`
	Evidence            []Evidence          `json:"evidence"`
	Usage               *Usage              `json:"usage"`
	ReadOnlyEnforcement ReadOnlyEnforcement `json:"read_only_enforcement"`
	MutationsDelta      MutationsDelta      `json:"mutations_delta"`
	FailureClass        string              `json:"failure_class"`
	FailureReason       string              `json:"failure_reason"`
}

// Finding is a normalized reviewer finding.
type Finding struct {
	Title    string     `json:"title,omitempty"`
	Body     string     `json:"body,omitempty"`
	File     string     `json:"file,omitempty"`
	Start    int        `json:"start,omitempty"`
	End      int        `json:"end,omitempty"`
	Severity string     `json:"severity,omitempty"`
	Lanes    []string   `json:"lanes,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Evidence captures compact source material used by a lane or summary.
type Evidence struct {
	Kind  string `json:"kind,omitempty"`
	Path  string `json:"path,omitempty"`
	URL   string `json:"url,omitempty"`
	Note  string `json:"note,omitempty"`
	Value string `json:"value,omitempty"`
}

// Usage records provider-reported token/cost data when available.
type Usage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	TotalTokens  int     `json:"total_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// ReadOnlyEnforcement records whether a review lane proved it respected the
// no-mutation contract.
type ReadOnlyEnforcement struct {
	Observed        bool     `json:"observed"`
	Enabled         bool     `json:"enabled"`
	Passed          bool     `json:"passed"`
	BaselineCommand string   `json:"baseline_command"`
	AfterCommand    string   `json:"after_command"`
	Notes           []string `json:"notes,omitempty"`
}

// Summary is the durable synthesized review quorum result.
type Summary struct {
	Subject string `json:"subject"`
	BaseRef string `json:"base_ref"`
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	// FindingsCount is the count of deduplicated synthesized findings.
	FindingsCount       int                 `json:"findings_count"`
	Findings            []Finding           `json:"findings"`
	Evidence            []Evidence          `json:"evidence"`
	Usage               *Usage              `json:"usage"`
	ReadOnlyEnforcement ReadOnlyEnforcement `json:"read_only_enforcement"`
	// MutationsDelta records synthesis-created mutations only; reviewer lane
	// mutation deltas remain in Lanes.
	MutationsDelta MutationsDelta `json:"mutations_delta"`
	FailureClass   string         `json:"failure_class"`
	FailureReason  string         `json:"failure_reason"`
	Lanes          []LaneOutput   `json:"lanes"`
}

func sortLaneOutputs(outputs []LaneOutput) {
	sort.SliceStable(outputs, func(i, j int) bool {
		return outputs[i].LaneID < outputs[j].LaneID
	})
}
