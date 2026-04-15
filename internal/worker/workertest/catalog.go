package workertest

// RequirementCode is the stable identifier for a conformance requirement.
type RequirementCode string

const (
	RequirementTranscriptDiscovery                 RequirementCode = "WC-TX-001"
	RequirementTranscriptNormalization             RequirementCode = "WC-TX-002"
	RequirementContinuationContinuity              RequirementCode = "WC-CONT-001"
	RequirementFreshSessionIsolation               RequirementCode = "WC-CONT-002"
	RequirementStartupOutcomeBound                 RequirementCode = "WC-BRINGUP-001"
	RequirementInteractionSignal                   RequirementCode = "WC-INT-000"
	RequirementInteractionPending                  RequirementCode = "WC-INT-001"
	RequirementInteractionRespond                  RequirementCode = "WC-INT-002"
	RequirementInteractionReject                   RequirementCode = "WC-INT-003"
	RequirementInteractionInstanceLocalDedup       RequirementCode = "WC-INT-004"
	RequirementToolEventNormalization              RequirementCode = "WC-TOOL-001"
	RequirementToolEventOpenTail                   RequirementCode = "WC-TOOL-002"
	RequirementStartupCommandMaterialization       RequirementCode = "WC-START-001"
	RequirementStartupRuntimeConfigMaterialization RequirementCode = "WC-START-002"
	RequirementInputInitialMessageFirstStart       RequirementCode = "WC-INPUT-001"
	RequirementInputInitialMessageResume           RequirementCode = "WC-INPUT-002"
	RequirementInputOverrideDefaults               RequirementCode = "WC-INPUT-003"
	RequirementInferenceFreshSpawn                 RequirementCode = "WI-START-001"
	RequirementInferenceFreshTask                  RequirementCode = "WI-TASK-001"
	RequirementInferenceWorkspaceTask              RequirementCode = "WI-TOOL-001"
	RequirementInferenceTranscript                 RequirementCode = "WI-TX-001"
	RequirementInferenceContinuation               RequirementCode = "WI-CONT-001"
)

// Requirement describes one worker conformance rule.
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

// Phase2Catalog returns the startup materialization, input delivery,
// interaction, and tool-substrate additions for the next deterministic
// worker-core slice.
func Phase2Catalog() []Requirement {
	return []Requirement{
		{
			Code:        RequirementStartupOutcomeBound,
			Group:       "startup",
			Description: "The worker fake surfaces a bounded startup outcome.",
		},
		{
			Code:        RequirementStartupCommandMaterialization,
			Group:       "startup_materialization",
			Description: "Provider defaults and resolved launch semantics materialize into the startup command for canonical worker profiles.",
		},
		{
			Code:        RequirementStartupRuntimeConfigMaterialization,
			Group:       "startup_materialization",
			Description: "Resolved workdir, env, and startup hints survive templateParamsToConfig into runtime.Config.",
		},
		{
			Code:        RequirementInputInitialMessageFirstStart,
			Group:       "input_delivery",
			Description: "A configured initial_message is injected into the first start exactly once.",
		},
		{
			Code:        RequirementInputInitialMessageResume,
			Group:       "input_delivery",
			Description: "A resumed session does not replay the initial_message after the first start has been recorded.",
		},
		{
			Code:        RequirementInputOverrideDefaults,
			Group:       "input_delivery",
			Description: "Schema overrides and initial_message handling preserve provider default launch flags while separating first-input delivery from option overrides.",
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
			Code:        RequirementInteractionInstanceLocalDedup,
			Group:       "interaction",
			Description: "Tmux interaction dedup state is instance-local so one worker session does not suppress another.",
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

// Phase3Catalog returns the initial live worker-inference smoke catalog.
func Phase3Catalog() []Requirement {
	return InferenceCatalog()
}

// InferenceCatalog returns the initial live worker-inference smoke catalog.
func InferenceCatalog() []Requirement {
	return []Requirement{
		{
			Code:        RequirementInferenceFreshSpawn,
			Group:       "live_startup",
			Description: "A fresh city sling spawns a live worker session for the canonical profile.",
		},
		{
			Code:        RequirementInferenceFreshTask,
			Group:       "live_task",
			Description: "The live worker completes a simple file-writing task with machine-checkable output.",
		},
		{
			Code:        RequirementInferenceWorkspaceTask,
			Group:       "live_tool_task",
			Description: "The live worker reads workspace state and completes a machine-checkable file-writing task that cannot be solved from the prompt alone.",
		},
		{
			Code:        RequirementInferenceTranscript,
			Group:       "live_transcript",
			Description: "The live worker transcript is discoverable and normalizable after the completed task.",
		},
		{
			Code:        RequirementInferenceContinuation,
			Group:       "live_continuation",
			Description: "A restarted live worker continues the same logical conversation and recalls prior turn context.",
		},
	}
}
