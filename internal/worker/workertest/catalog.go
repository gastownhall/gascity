package workertest

// RequirementCode is the stable identifier for a conformance requirement.
type RequirementCode string

const (
	RequirementTranscriptDiscovery     RequirementCode = "WC-TX-001"
	RequirementTranscriptNormalization RequirementCode = "WC-TX-002"
	RequirementContinuationContinuity  RequirementCode = "WC-CONT-001"
	RequirementFreshSessionIsolation   RequirementCode = "WC-CONT-002"
	RequirementStartupOutcomeBound     RequirementCode = "WC-BRINGUP-001"
	RequirementInteractionSignal       RequirementCode = "WC-INT-000"
	RequirementInteractionPending      RequirementCode = "WC-INT-001"
	RequirementInteractionRespond      RequirementCode = "WC-INT-002"
	RequirementInteractionReject       RequirementCode = "WC-INT-003"
	RequirementToolEventNormalization  RequirementCode = "WC-TOOL-001"
	RequirementToolEventOpenTail       RequirementCode = "WC-TOOL-002"
)

// Requirement describes one phase-1 worker-core rule.
type Requirement struct {
	Code        RequirementCode
	Group       string
	Description string
}

// Phase1Catalog returns the first worker-core transcript/continuation catalog.
func Phase1Catalog() []Requirement {
	return []Requirement{
		{
			Code:        RequirementTranscriptDiscovery,
			Group:       "transcript",
			Description: "The profile resolves its provider-native transcript fixture path.",
		},
		{
			Code:        RequirementTranscriptNormalization,
			Group:       "transcript",
			Description: "The profile transcript normalizes into the canonical message shape.",
		},
		{
			Code:        RequirementContinuationContinuity,
			Group:       "continuation",
			Description: "The continued transcript preserves prior normalized history and logical conversation identity.",
		},
		{
			Code:        RequirementFreshSessionIsolation,
			Group:       "continuation",
			Description: "A fresh session fixture does not alias the prior logical conversation.",
		},
	}
}

// Phase2Catalog returns the startup/interaction/tool-substrate additions for
// the next deterministic worker-core slice.
func Phase2Catalog() []Requirement {
	return []Requirement{
		{
			Code:        RequirementStartupOutcomeBound,
			Group:       "startup",
			Description: "The worker fake surfaces a bounded startup outcome.",
		},
		{
			Code:        RequirementInteractionSignal,
			Group:       "interaction",
			Description: "The standalone fake worker surfaces a blocked structured interaction signal and state.",
		},
		{
			Code:        RequirementInteractionPending,
			Group:       "interaction",
			Description: "Required structured interactions surface through the runtime interaction seam.",
		},
		{
			Code:        RequirementInteractionRespond,
			Group:       "interaction",
			Description: "Responding to a pending structured interaction clears the pending state.",
		},
		{
			Code:        RequirementInteractionReject,
			Group:       "interaction",
			Description: "A mismatched interaction response is rejected without clearing the pending interaction.",
		},
		{
			Code:        RequirementToolEventNormalization,
			Group:       "tool",
			Description: "Normalized history preserves tool_use/tool_result substrate events.",
		},
		{
			Code:        RequirementToolEventOpenTail,
			Group:       "tool",
			Description: "Open tool_use events remain visible at the normalized transcript tail when unresolved.",
		},
	}
}
