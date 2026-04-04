package workertest

// RequirementCode is the stable identifier for a conformance requirement.
type RequirementCode string

const (
	RequirementTranscriptDiscovery     RequirementCode = "WC-TX-001"
	RequirementTranscriptNormalization RequirementCode = "WC-TX-002"
	RequirementContinuationContinuity  RequirementCode = "WC-CONT-001"
	RequirementFreshSessionIsolation   RequirementCode = "WC-CONT-002"
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
