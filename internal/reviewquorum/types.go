// Package reviewquorum defines the durable contract for Gas City review
// quorum lanes and synthesis.
package reviewquorum

import (
	"fmt"
	"sort"
	"strings"
)

const (
	ProviderOpenCode = "opencode"

	LaneKimi     = "kimi"
	LaneDeepSeek = "deepseek"

	ModelKimi     = "opencode-go/kimi-k2.6"
	ModelDeepSeek = "opencode-go/deepseek-v4-pro"

	FailureClassTransient = "transient"
	FailureClassHard      = "hard"

	VerdictPass              = "pass"
	VerdictPassWithFindings  = "pass_with_findings"
	VerdictFail              = "fail"
	VerdictBlocked           = "blocked"
	VerdictAwaitingReviewers = "awaiting_reviewers"
)

// LaneConfig describes one reviewer lane in the quorum.
type LaneConfig struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// DefaultLaneConfigs returns the fixed two-lane OpenCode quorum.
func DefaultLaneConfigs() []LaneConfig {
	return []LaneConfig{
		{ID: LaneKimi, Provider: ProviderOpenCode, Model: ModelKimi},
		{ID: LaneDeepSeek, Provider: ProviderOpenCode, Model: ModelDeepSeek},
	}
}

// ValidateDefaultQuorum checks that lanes are exactly the built-in Kimi and
// DeepSeek OpenCode quorum, with no extras, aliases, or reordered duplicates.
func ValidateDefaultQuorum(lanes []LaneConfig) error {
	if err := ValidateLaneConfigs(lanes); err != nil {
		return err
	}
	want := map[string]string{
		LaneKimi:     ModelKimi,
		LaneDeepSeek: ModelDeepSeek,
	}
	if len(lanes) != len(want) {
		return fmt.Errorf("default review quorum must have exactly %d lanes, got %d", len(want), len(lanes))
	}
	for _, lane := range lanes {
		model, ok := want[lane.ID]
		if !ok {
			return fmt.Errorf("default review quorum has unexpected lane %q", lane.ID)
		}
		if lane.Provider != ProviderOpenCode {
			return fmt.Errorf("lane %q provider = %q, want %q", lane.ID, lane.Provider, ProviderOpenCode)
		}
		if lane.Model != model {
			return fmt.Errorf("lane %q model = %q, want %q", lane.ID, lane.Model, model)
		}
	}
	return nil
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
	Provider            string              `json:"provider,omitempty"`
	Model               string              `json:"model,omitempty"`
	Verdict             string              `json:"verdict"`
	Summary             string              `json:"summary"`
	FindingsCount       int                 `json:"findings_count"`
	Findings            []Finding           `json:"findings,omitempty"`
	Evidence            []Evidence          `json:"evidence,omitempty"`
	Usage               Usage               `json:"usage,omitempty"`
	ReadOnlyEnforcement ReadOnlyEnforcement `json:"read_only_enforcement"`
	MutationsDelta      MutationsDelta      `json:"mutations_delta"`
	FailureClass        string              `json:"failure_class,omitempty"`
	FailureReason       string              `json:"failure_reason,omitempty"`
}

// Finding is a normalized reviewer finding.
type Finding struct {
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	File     string `json:"file,omitempty"`
	Start    int    `json:"start,omitempty"`
	End      int    `json:"end,omitempty"`
	Severity string `json:"severity,omitempty"`
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

// ReadOnlyEnforcement records whether review lanes respected the no-mutation
// contract.
type ReadOnlyEnforcement struct {
	Enabled bool     `json:"enabled"`
	Passed  bool     `json:"passed"`
	Notes   []string `json:"notes,omitempty"`
}

// Summary is the durable synthesized review quorum result.
type Summary struct {
	Verdict             string              `json:"verdict"`
	Summary             string              `json:"summary"`
	FindingsCount       int                 `json:"findings_count"`
	Findings            []Finding           `json:"findings,omitempty"`
	Evidence            []Evidence          `json:"evidence,omitempty"`
	Usage               Usage               `json:"usage,omitempty"`
	ReadOnlyEnforcement ReadOnlyEnforcement `json:"read_only_enforcement"`
	MutationsDelta      MutationsDelta      `json:"mutations_delta"`
	FailureClass        string              `json:"failure_class,omitempty"`
	FailureReason       string              `json:"failure_reason,omitempty"`
	Lanes               []LaneOutput        `json:"lanes"`
}

func normalizedFindingsCount(out LaneOutput) int {
	if out.FindingsCount > 0 {
		return out.FindingsCount
	}
	return len(out.Findings)
}

func sortLaneOutputs(outputs []LaneOutput) {
	sort.SliceStable(outputs, func(i, j int) bool {
		return outputs[i].LaneID < outputs[j].LaneID
	})
}
