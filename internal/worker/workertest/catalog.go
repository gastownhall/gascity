// Package workertest provides shared worker conformance catalogs, fixtures, and helpers.
package workertest

// RequirementCode is the stable identifier for a conformance requirement.
type RequirementCode string

// revive:disable:exported
const ( //nolint:revive // exported requirement IDs are documented by the catalog helpers.
	// Requirement* are stable worker-conformance requirement identifiers.
	RequirementTranscriptDiscovery                 RequirementCode = "WC-TX-001"
	RequirementTranscriptNormalization             RequirementCode = "WC-TX-002"
	RequirementTranscriptDiagnostics               RequirementCode = "WC-TX-003"
	RequirementContinuationContinuity              RequirementCode = "WC-CONT-001"
	RequirementFreshSessionIsolation               RequirementCode = "WC-CONT-002"
	RequirementStartupOutcomeBound                 RequirementCode = "WC-BRINGUP-001"
	RequirementInteractionSignal                   RequirementCode = "WC-INT-000"
	RequirementInteractionPending                  RequirementCode = "WC-INT-001"
	RequirementInteractionRespond                  RequirementCode = "WC-INT-002"
	RequirementInteractionReject                   RequirementCode = "WC-INT-003"
	RequirementInteractionInstanceLocalDedup       RequirementCode = "WC-INT-004"
	RequirementInteractionDurableHistory           RequirementCode = "WC-INT-005"
	RequirementInteractionLifecycleHistory         RequirementCode = "WC-INT-006"
	RequirementToolEventNormalization              RequirementCode = "WC-TOOL-001"
	RequirementToolEventOpenTail                   RequirementCode = "WC-TOOL-002"
	RequirementRealTransportProof                  RequirementCode = "WC-TRANSPORT-001"
	RequirementStartupCommandMaterialization       RequirementCode = "WC-START-001"
	RequirementStartupRuntimeConfigMaterialization RequirementCode = "WC-START-002"
	RequirementInputInitialMessageFirstStart       RequirementCode = "WC-INPUT-001"
	RequirementInputInitialMessageResume           RequirementCode = "WC-INPUT-002"
	RequirementInputOverrideDefaults               RequirementCode = "WC-INPUT-003"
	RequirementInferenceFreshSpawn                 RequirementCode = "WI-START-001"
	RequirementInferenceFreshTask                  RequirementCode = "WI-TASK-001"
	RequirementInferenceWorkspaceTask              RequirementCode = "WI-TOOL-001"
	RequirementInferenceMultiTurnWorkflow          RequirementCode = "WI-MTURN-001"
	RequirementInferenceTranscript                 RequirementCode = "WI-TX-001"
	RequirementInferenceContinuation               RequirementCode = "WI-CONT-001"
	RequirementInferenceFreshReset                 RequirementCode = "WI-RESET-001"
	RequirementInferenceInterruptRecoverContinue   RequirementCode = "WI-INT-001"
)

// revive:enable:exported

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
// worker-core slice. The authoritative data lives in embedded JSON/YAML
// catalog files so the requirement list stays stable and data-first.
func Phase2Catalog() []Requirement {
	return phase2CatalogRequirements()
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
			Code:        RequirementInferenceMultiTurnWorkflow,
			Group:       "live_multi_turn",
			Description: "The live worker completes a multi-turn workflow in one conversation, using prior turn context to produce machine-checkable output.",
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
		{
			Code:        RequirementInferenceFreshReset,
			Group:       "live_fresh_reset",
			Description: "A fresh-reset live worker preserves the session bead but starts a new logical conversation that does not recall prior turn context.",
		},
		{
			Code:        RequirementInferenceInterruptRecoverContinue,
			Group:       "live_interrupt_recover",
			Description: "The live worker can interrupt an in-flight turn, recover with a replacement task, and continue the same conversation afterward.",
		},
	}
}
