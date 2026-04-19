package workertest

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	worker "github.com/gastownhall/gascity/internal/worker"
)

func startupOutcomeResult(profile ProfileID, outcome string, delay time.Duration, run fakeStartupRun) Result {
	evidence := map[string]string{
		"state_path":     run.StatePath,
		"event_log_path": run.EventPath,
		"expected_state": outcome,
		"launch_to_wait": run.LaunchToWait.String(),
		"expected_delay": delay.String(),
		"event_count":    fmt.Sprintf("%d", len(run.Events)),
		"elapsed":        run.Elapsed.String(),
	}
	stateData, err := os.ReadFile(run.StatePath)
	if err != nil {
		evidence["read_error"] = err.Error()
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("read %s: %v", run.StatePath, err)).WithEvidence(evidence)
	}
	got := strings.TrimSpace(string(stateData))
	evidence["observed_state"] = got
	if got != outcome {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("startup state = %q, want %q", got, outcome)).WithEvidence(evidence)
	}
	if run.LaunchToWait > fakeStartupLaunchBound {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("%s exceeded launch-to-wait bound: %s > %s", RequirementStartupOutcomeBound, run.LaunchToWait, fakeStartupLaunchBound)).WithEvidence(evidence)
	}
	if len(run.Events) < 3 {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("expected control wait, control observed, and startup events for %s", RequirementStartupOutcomeBound)).WithEvidence(evidence)
	}
	if run.Events[0].Kind != "control_waiting" {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("first event kind = %q, want control_waiting", run.Events[0].Kind)).WithEvidence(evidence)
	}
	if run.Events[1].Kind != "control_observed" {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("second event kind = %q, want control_observed", run.Events[1].Kind)).WithEvidence(evidence)
	}
	event := run.Events[2]
	evidence["observed_transition_kind"] = event.Kind
	evidence["observed_transition_state"] = event.State
	if event.Kind != "state_transition" {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("event kind = %q, want state_transition", event.Kind)).WithEvidence(evidence)
	}
	if event.State != outcome {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("event state = %q, want %q", event.State, outcome)).WithEvidence(evidence)
	}
	if event.Provider != string(profile) {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("event provider = %q, want %q", event.Provider, profile)).WithEvidence(evidence)
	}
	postControl := event.Time.Sub(run.Events[1].Time)
	evidence["post_control_delay"] = postControl.String()
	maxPostControl := delay + fakeStartupPostControlOverhead
	if postControl > maxPostControl {
		return Fail(profile, RequirementStartupOutcomeBound,
			fmt.Sprintf("%s exceeded post-control bound: %s > %s", RequirementStartupOutcomeBound, postControl, maxPostControl)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementStartupOutcomeBound, "standalone fake worker surfaced the bounded startup outcome").WithEvidence(evidence)
}

func interactionSignalResult(profile ProfileID, run fakeStartupRun) Result {
	evidence := map[string]string{
		"state_path":     run.StatePath,
		"event_log_path": run.EventPath,
		"event_count":    fmt.Sprintf("%d", len(run.Events)),
		"elapsed":        run.Elapsed.String(),
	}
	stateData, err := os.ReadFile(run.StatePath)
	if err != nil {
		evidence["read_error"] = err.Error()
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("read %s: %v", run.StatePath, err)).WithEvidence(evidence)
	}
	got := strings.TrimSpace(string(stateData))
	evidence["observed_state"] = got
	if got != "blocked" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("interaction state = %q, want blocked", got)).WithEvidence(evidence)
	}
	if run.Elapsed > fakeInteractionSignalBound {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("blocked interaction signal exceeded bound: %s > %s", run.Elapsed, fakeInteractionSignalBound)).WithEvidence(evidence)
	}
	if len(run.Events) != 2 {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event count = %d, want 2", len(run.Events))).WithEvidence(evidence)
	}
	event := run.Events[1]
	if event.Kind != "interaction" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event kind = %q, want interaction", event.Kind)).WithEvidence(evidence)
	}
	if event.Provider != string(profile) {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event provider = %q, want %q", event.Provider, profile)).WithEvidence(evidence)
	}
	if event.State != "blocked" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("event state = %q, want blocked", event.State)).WithEvidence(evidence)
	}
	if event.Interaction == nil {
		return Fail(profile, RequirementInteractionSignal, "interaction event missing payload").WithEvidence(evidence)
	}
	evidence["interaction_kind"] = event.Interaction.Kind
	evidence["interaction_request_id"] = event.Interaction.RequestID
	if event.Interaction.Kind != "approval" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("interaction kind = %q, want approval", event.Interaction.Kind)).WithEvidence(evidence)
	}
	if event.Interaction.RequestID != "req-1" {
		return Fail(profile, RequirementInteractionSignal,
			fmt.Sprintf("interaction request ID = %q, want req-1", event.Interaction.RequestID)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionSignal, "standalone fake worker surfaced the required interaction signal").WithEvidence(evidence)
}

func pendingInteractionResult(profile ProfileID, got, expected *runtime.PendingInteraction, err error) Result {
	evidence := map[string]string{
		"expected_request_id": expected.RequestID,
		"expected_kind":       expected.Kind,
	}
	if err != nil {
		evidence["error"] = err.Error()
		return Fail(profile, RequirementInteractionPending, fmt.Sprintf("Pending: %v", err)).WithEvidence(evidence)
	}
	if got == nil {
		return Fail(profile, RequirementInteractionPending, "expected pending interaction").WithEvidence(evidence)
	}
	evidence["observed_request_id"] = got.RequestID
	evidence["observed_kind"] = got.Kind
	evidence["observed_profile"] = got.Metadata["profile"]
	if got.RequestID != expected.RequestID {
		return Fail(profile, RequirementInteractionPending,
			fmt.Sprintf("RequestID = %q, want %q", got.RequestID, expected.RequestID)).WithEvidence(evidence)
	}
	if got.Kind != expected.Kind {
		return Fail(profile, RequirementInteractionPending,
			fmt.Sprintf("Kind = %q, want %q", got.Kind, expected.Kind)).WithEvidence(evidence)
	}
	if got.Metadata["profile"] != string(profile) {
		return Fail(profile, RequirementInteractionPending,
			fmt.Sprintf("profile metadata = %q, want %q", got.Metadata["profile"], profile)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionPending, "runtime interaction seam exposed the pending approval request").WithEvidence(evidence)
}

func rejectInteractionResult(profile ProfileID, respondErr error, stillPending *runtime.PendingInteraction, pendingErr error, responseCount int) Result {
	evidence := map[string]string{
		"response_count": fmt.Sprintf("%d", responseCount),
	}
	if respondErr == nil {
		return Fail(profile, RequirementInteractionReject, "Respond should fail for mismatched request id").WithEvidence(evidence)
	}
	evidence["respond_error"] = respondErr.Error()
	if pendingErr != nil {
		evidence["pending_error"] = pendingErr.Error()
		return Fail(profile, RequirementInteractionReject, fmt.Sprintf("Pending after reject: %v", pendingErr)).WithEvidence(evidence)
	}
	if stillPending == nil {
		return Fail(profile, RequirementInteractionReject, "pending interaction cleared after mismatched response").WithEvidence(evidence)
	}
	evidence["remaining_request_id"] = stillPending.RequestID
	if responseCount != 0 {
		return Fail(profile, RequirementInteractionReject,
			fmt.Sprintf("recorded responses = %d, want 0", responseCount)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionReject, "mismatched responses are rejected without clearing the pending interaction").WithEvidence(evidence)
}

func respondInteractionResult(profile ProfileID, respondErr error, got *runtime.PendingInteraction, pendingErr error, responses []runtime.InteractionResponse) Result {
	evidence := map[string]string{
		"response_count": fmt.Sprintf("%d", len(responses)),
	}
	if respondErr != nil {
		evidence["respond_error"] = respondErr.Error()
		return Fail(profile, RequirementInteractionRespond, fmt.Sprintf("Respond: %v", respondErr)).WithEvidence(evidence)
	}
	if pendingErr != nil {
		evidence["pending_error"] = pendingErr.Error()
		return Fail(profile, RequirementInteractionRespond, fmt.Sprintf("Pending after respond: %v", pendingErr)).WithEvidence(evidence)
	}
	if got != nil {
		evidence["remaining_request_id"] = got.RequestID
		return Fail(profile, RequirementInteractionRespond, "pending interaction not cleared after response").WithEvidence(evidence)
	}
	if len(responses) != 1 {
		return Fail(profile, RequirementInteractionRespond,
			fmt.Sprintf("recorded responses = %d, want 1", len(responses))).WithEvidence(evidence)
	}
	evidence["recorded_action"] = responses[0].Action
	if responses[0].Action != "approve" {
		return Fail(profile, RequirementInteractionRespond,
			fmt.Sprintf("recorded action = %q, want approve", responses[0].Action)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionRespond, "responding to a pending interaction clears the request and records the response").WithEvidence(evidence)
}

func interactionDurableHistoryResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot) Result {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
	}
	if history == nil {
		return Fail(profile, RequirementInteractionDurableHistory, "expected history snapshot").WithEvidence(evidence)
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["pending_interaction_ids"] = strings.Join(history.TailState.PendingInteractionIDs, ",")

	interaction, ok := findHistoryInteraction(history, "approval-1")
	if !ok {
		return Fail(profile, RequirementInteractionDurableHistory, "normalized history missing durable interaction record").WithEvidence(evidence)
	}
	evidence["interaction_request_id"] = interaction.RequestID
	evidence["interaction_kind"] = interaction.Kind
	evidence["interaction_state"] = string(interaction.State)
	evidence["interaction_prompt"] = interaction.Prompt
	evidence["interaction_options"] = strings.Join(interaction.Options, ",")

	if interaction.Kind != "approval" {
		return Fail(profile, RequirementInteractionDurableHistory,
			fmt.Sprintf("interaction kind = %q, want approval", interaction.Kind)).WithEvidence(evidence)
	}
	if interaction.State != worker.InteractionStatePending {
		return Fail(profile, RequirementInteractionDurableHistory,
			fmt.Sprintf("interaction state = %q, want %q", interaction.State, worker.InteractionStatePending)).WithEvidence(evidence)
	}
	if interaction.Prompt != "Allow Read?" {
		return Fail(profile, RequirementInteractionDurableHistory,
			fmt.Sprintf("interaction prompt = %q, want Allow Read?", interaction.Prompt)).WithEvidence(evidence)
	}
	if !containsString(history.TailState.PendingInteractionIDs, "approval-1") {
		return Fail(profile, RequirementInteractionDurableHistory,
			"pending interaction not visible in transcript tail state").WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionDurableHistory, "normalized history preserved durable pending interaction state").WithEvidence(evidence)
}

func interactionLifecycleHistoryResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot, finalState worker.InteractionState, wantPending bool) Result {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
		"expected_state":  string(finalState),
		"want_pending":    fmt.Sprintf("%t", wantPending),
	}
	if history == nil {
		return Fail(profile, RequirementInteractionLifecycleHistory, "expected history snapshot").WithEvidence(evidence)
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["pending_interaction_ids"] = strings.Join(history.TailState.PendingInteractionIDs, ",")

	interaction, ok := findLastHistoryInteraction(history, "approval-1")
	if !ok {
		return Fail(profile, RequirementInteractionLifecycleHistory, "normalized history missing final interaction lifecycle record").WithEvidence(evidence)
	}
	evidence["observed_state"] = string(interaction.State)
	evidence["interaction_action"] = interaction.Action
	if interaction.State != finalState {
		return Fail(profile, RequirementInteractionLifecycleHistory,
			fmt.Sprintf("final interaction state = %q, want %q", interaction.State, finalState)).WithEvidence(evidence)
	}
	isPending := containsString(history.TailState.PendingInteractionIDs, "approval-1")
	evidence["observed_pending"] = fmt.Sprintf("%t", isPending)
	if isPending != wantPending {
		return Fail(profile, RequirementInteractionLifecycleHistory,
			fmt.Sprintf("tail pending = %t, want %t", isPending, wantPending)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementInteractionLifecycleHistory, "normalized history applied durable interaction lifecycle state to the transcript tail").WithEvidence(evidence)
}

func findHistoryInteraction(history *worker.HistorySnapshot, requestID string) (*worker.HistoryInteraction, bool) {
	for _, entry := range history.Entries {
		for _, block := range entry.Blocks {
			if block.Kind == worker.BlockKindInteraction && block.Interaction != nil && block.Interaction.RequestID == requestID {
				return block.Interaction, true
			}
		}
	}
	return nil, false
}

func findLastHistoryInteraction(history *worker.HistorySnapshot, requestID string) (*worker.HistoryInteraction, bool) {
	for i := len(history.Entries) - 1; i >= 0; i-- {
		entry := history.Entries[i]
		for j := len(entry.Blocks) - 1; j >= 0; j-- {
			block := entry.Blocks[j]
			if block.Kind == worker.BlockKindInteraction && block.Interaction != nil && block.Interaction.RequestID == requestID {
				return block.Interaction, true
			}
		}
	}
	return nil, false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func toolNormalizationResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot) Result {
	evidence := toolHistoryEvidence(transcriptPath, history)
	switch {
	case len(history.TailState.OpenToolUseIDs) != 0:
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("open tool uses = %v, want none", history.TailState.OpenToolUseIDs)).WithEvidence(evidence)
	case len(history.Entries) < 2:
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("entries = %d, want at least 2", len(history.Entries))).WithEvidence(evidence)
	case !historyHasBlockKind(history, worker.BlockKindToolUse):
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("normalized history missing %q block", worker.BlockKindToolUse)).WithEvidence(evidence)
	case !historyHasBlockKind(history, worker.BlockKindToolResult):
		return Fail(profile, RequirementToolEventNormalization,
			fmt.Sprintf("normalized history missing %q block", worker.BlockKindToolResult)).WithEvidence(evidence)
	default:
		return Pass(profile, RequirementToolEventNormalization, "normalized history preserved tool_use/tool_result substrate events").WithEvidence(evidence)
	}
}

func toolOpenTailResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot) Result {
	evidence := toolHistoryEvidence(transcriptPath, history)
	if !historyHasOpenToolUseEvidence(history) {
		return Fail(profile, RequirementToolEventOpenTail,
			fmt.Sprintf("normalized history does not preserve unresolved tool-use evidence: %+v", history.TailState.OpenToolUseIDs)).WithEvidence(evidence)
	}
	return Pass(profile, RequirementToolEventOpenTail, "normalized history preserved unresolved tool-use evidence at the transcript tail").WithEvidence(evidence)
}

func toolHistoryEvidence(transcriptPath string, history *worker.HistorySnapshot) map[string]string {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
	}
	if history == nil {
		return evidence
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["open_tool_use_count"] = fmt.Sprintf("%d", len(history.TailState.OpenToolUseIDs))
	if len(history.TailState.OpenToolUseIDs) > 0 {
		evidence["open_tool_use_ids"] = strings.Join(history.TailState.OpenToolUseIDs, ",")
	}
	return evidence
}

func historyDiagnosticsResult(profile ProfileID, transcriptPath string, history *worker.HistorySnapshot, loadErr error) Result {
	evidence := historyDiagnosticsEvidence(transcriptPath, history)
	if loadErr != nil {
		evidence["load_error"] = loadErr.Error()
		if profile == ProfileGeminiTmuxCLI {
			return Pass(profile, RequirementTranscriptDiagnostics, "malformed single-file transcript failed closed").WithEvidence(evidence)
		}
		return Fail(profile, RequirementTranscriptDiagnostics, fmt.Sprintf("LoadHistory: %v", loadErr)).WithEvidence(evidence)
	}
	if history == nil {
		return Fail(profile, RequirementTranscriptDiagnostics, "expected history snapshot").WithEvidence(evidence)
	}
	if len(history.Entries) == 0 {
		return Fail(profile, RequirementTranscriptDiagnostics, "degraded history has no readable prefix").WithEvidence(evidence)
	}
	if history.Continuity.Status != worker.ContinuityStatusDegraded {
		return Fail(profile, RequirementTranscriptDiagnostics,
			fmt.Sprintf("continuity status = %q, want %q", history.Continuity.Status, worker.ContinuityStatusDegraded)).WithEvidence(evidence)
	}
	expectedCode := expectedHistoryDiagnosticCode(profile)
	if expectedCode != "" && !historyHasDiagnosticCode(history, expectedCode) {
		return Fail(profile, RequirementTranscriptDiagnostics,
			fmt.Sprintf("diagnostics missing %q", expectedCode)).WithEvidence(evidence)
	}
	if len(history.Diagnostics) == 0 {
		return Fail(profile, RequirementTranscriptDiagnostics, "expected history diagnostics").WithEvidence(evidence)
	}
	return Pass(profile, RequirementTranscriptDiagnostics, "malformed transcript surfaced degraded history diagnostics").WithEvidence(evidence)
}

func historyDiagnosticsEvidence(transcriptPath string, history *worker.HistorySnapshot) map[string]string {
	evidence := map[string]string{
		"transcript_path": transcriptPath,
	}
	if history == nil {
		return evidence
	}
	evidence["entry_count"] = fmt.Sprintf("%d", len(history.Entries))
	evidence["continuity_status"] = string(history.Continuity.Status)
	if history.Continuity.Note != "" {
		evidence["continuity_note"] = history.Continuity.Note
	}
	if len(history.Diagnostics) > 0 {
		evidence["diagnostic_count"] = fmt.Sprintf("%d", len(history.Diagnostics))
		evidence["diagnostic_codes"] = diagnosticCodes(history.Diagnostics)
		for _, diagnostic := range history.Diagnostics {
			if diagnostic.Count > 0 {
				evidence["diagnostic_"+diagnostic.Code+"_count"] = fmt.Sprintf("%d", diagnostic.Count)
			}
		}
	}
	if history.TailState.Degraded {
		evidence["tail_degraded"] = "true"
		evidence["tail_degraded_reason"] = history.TailState.DegradedReason
	}
	return evidence
}

func historyHasDiagnosticCode(history *worker.HistorySnapshot, code string) bool {
	for _, diagnostic := range history.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func expectedHistoryDiagnosticCode(profile ProfileID) string {
	switch profile {
	case ProfileClaudeTmuxCLI:
		return "malformed_tail"
	case ProfileCodexTmuxCLI:
		return "malformed_jsonl"
	default:
		// Gemini stores one JSON document, so malformed/truncated transcript
		// input fails closed in encoding/json before a diagnostic code exists.
		return ""
	}
}
