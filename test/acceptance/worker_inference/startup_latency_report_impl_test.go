//go:build acceptance_c

package workerinference_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	agentStartLatencyDefaultSamples = 30

	agentStartOutcomeCompleted    = "completed"
	agentStartOutcomeIncomplete   = "incomplete"
	agentStartOutcomeError        = "error"
	agentStartOutcomeCanceled     = "canceled"
	agentStartOutcomeNotAttempted = "not_attempted"

	agentStartReadinessPromptPrefix = "prompt_prefix"
	agentStartReadinessFixedDelay   = "fixed_delay"
	agentStartReadinessNone         = "none"

	agentStartMetricTotal                   = "start_to_first_turn_complete"
	agentStartMetricNonInference            = "start_to_prompt_delivered"
	agentStartMetricStartToRuntime          = "start_to_runtime_available"
	agentStartMetricRuntimeToCLIExec        = "runtime_to_cli_process_exec"
	agentStartMetricCLIExecToReady          = "cli_process_exec_to_ready"
	agentStartMetricReadyToPrompt           = "cli_ready_to_prompt_delivered"
	agentStartMetricPromptToFirstOutput     = "prompt_to_first_assistant_output"
	agentStartMetricFirstOutputToCompletion = "first_assistant_output_to_turn_complete"
	agentStartMetricUserPromptSubmitHook    = "user_prompt_submit_hook"
	agentStartMetricControllerTotal         = "controller_start_total"
	agentStartMetricControllerStartCall     = "controller_start_call"
	agentStartMetricControllerZombieRecycle = "controller_zombie_recycle"
	agentStartMetricControllerStateSync     = "controller_state_sync_recovery"
	agentStartMetricControllerPostObserve   = "controller_post_start_observe"
	agentStartMetricControllerCommitRefresh = "controller_commit_refresh"
)

var agentStartLatencyRequiredBinaries = []string{"gc", "provider", "tmux", "bd", "dolt"}

type agentStartLatencyBinary struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

type agentStartLatencyReadiness struct {
	Strategy     string        `json:"strategy"`
	PromptPrefix string        `json:"prompt_prefix,omitempty"`
	Delay        time.Duration `json:"delay_ns,omitempty"`
}

type agentStartLatencyProvenance struct {
	Error             string                     `json:"error,omitempty"`
	Profile           string                     `json:"profile"`
	Provider          string                     `json:"provider"`
	RuntimeProvider   string                     `json:"runtime_provider"`
	ReconcilerMode    string                     `json:"reconciler_mode"`
	GCCommit          string                     `json:"gc_commit"`
	HostOS            string                     `json:"host_os"`
	HostArch          string                     `json:"host_arch"`
	CPUCount          int                        `json:"cpu_count"`
	CityPath          string                     `json:"city_path,omitempty"`
	CityConfigSHA256  string                     `json:"city_config_sha256"`
	AgentConfigSHA256 string                     `json:"agent_config_sha256"`
	TmuxSocket        string                     `json:"tmux_socket,omitempty"`
	AuthSource        string                     `json:"auth_source,omitempty"`
	Readiness         agentStartLatencyReadiness `json:"readiness"`
	Binaries          []agentStartLatencyBinary  `json:"binaries"`
}

type agentStartLatencyTimestamps struct {
	StartInitiatedAt       time.Time  `json:"start_initiated_at"`
	IntentReturnedAt       *time.Time `json:"intent_returned_at,omitempty"`
	RuntimeAvailableAt     *time.Time `json:"runtime_available_at,omitempty"`
	CLIProcessExecAt       *time.Time `json:"cli_process_exec_at,omitempty"`
	CLIReadyAt             *time.Time `json:"cli_ready_at,omitempty"`
	PromptDeliveredAt      *time.Time `json:"prompt_delivered_at,omitempty"`
	FirstAssistantOutputAt *time.Time `json:"first_assistant_output_at,omitempty"`
	FirstTurnCompletedAt   *time.Time `json:"first_turn_completed_at,omitempty"`
	CleanupCompletedAt     *time.Time `json:"cleanup_completed_at,omitempty"`
}

type agentStartLatencyTerminalProof struct {
	ExpectedOutputMatched bool `json:"expected_output_matched"`
	AssistantAfterPrompt  bool `json:"assistant_after_prompt"`
	TranscriptIdle        bool `json:"transcript_idle"`
	NoOpenToolUse         bool `json:"no_open_tool_use"`
	NoPendingInteraction  bool `json:"no_pending_interaction"`
	DurableSessionRetired bool `json:"durable_session_retired"`
	TmuxSessionAbsent     bool `json:"tmux_session_absent"`
}

type agentStartLatencyControllerTiming struct {
	SessionID         string        `json:"session_id,omitempty"`
	Total             time.Duration `json:"total_ns,omitempty"`
	StartCall         time.Duration `json:"start_call_ns,omitempty"`
	ZombieRecycle     time.Duration `json:"zombie_recycle_ns,omitempty"`
	StateSyncRecovery time.Duration `json:"state_sync_recovery_ns,omitempty"`
	PostStartObserve  time.Duration `json:"post_start_observe_ns,omitempty"`
	CommitRefresh     time.Duration `json:"commit_refresh_ns,omitempty"`
}

type agentStartLatencyDurations struct {
	Total                   time.Duration  `json:"total_ns"`
	NonInference            time.Duration  `json:"non_inference_ns"`
	StartToRuntime          time.Duration  `json:"start_to_runtime_ns"`
	RuntimeToCLIExec        time.Duration  `json:"runtime_to_cli_exec_ns"`
	CLIExecToReady          *time.Duration `json:"cli_exec_to_ready_ns,omitempty"`
	ReadyToPrompt           *time.Duration `json:"ready_to_prompt_ns,omitempty"`
	PromptToFirstOutput     time.Duration  `json:"prompt_to_first_output_ns"`
	FirstOutputToCompletion time.Duration  `json:"first_output_to_completion_ns"`
}

type agentStartLatencySample struct {
	Index                     int                               `json:"index"`
	Outcome                   string                            `json:"outcome"`
	RunIdentity               string                            `json:"run_identity,omitempty"`
	SessionID                 string                            `json:"session_id,omitempty"`
	SessionName               string                            `json:"session_name,omitempty"`
	Error                     string                            `json:"error,omitempty"`
	Timestamps                agentStartLatencyTimestamps       `json:"timestamps"`
	Terminal                  agentStartLatencyTerminalProof    `json:"terminal"`
	Controller                agentStartLatencyControllerTiming `json:"controller"`
	UserPromptSubmitHook      *time.Duration                    `json:"user_prompt_submit_hook_ns,omitempty"`
	UserPromptSubmitHookError string                            `json:"user_prompt_submit_hook_error,omitempty"`
	Durations                 *agentStartLatencyDurations       `json:"durations,omitempty"`
}

type agentStartLatencyPercentiles struct {
	Count int           `json:"count"`
	P50   time.Duration `json:"p50_ns"`
	P95   time.Duration `json:"p95_ns"`
	P99   time.Duration `json:"p99_ns"`
	Max   time.Duration `json:"max_ns"`
}

type agentStartLatencyMetricStats struct {
	Name                     string                        `json:"name"`
	ObservedCount            int                           `json:"observed_count"`
	MissingCount             int                           `json:"missing_count"`
	ExcludedFromOptimization bool                          `json:"excluded_from_optimization_kpi"`
	Latency                  *agentStartLatencyPercentiles `json:"latency,omitempty"`
}

type agentStartLatencyOutcomeCounts struct {
	Completed    int `json:"completed"`
	Incomplete   int `json:"incomplete"`
	Error        int `json:"error"`
	Canceled     int `json:"canceled"`
	NotAttempted int `json:"not_attempted"`
}

type agentStartLatencyReport struct {
	SchemaVersion        string                         `json:"schema_version"`
	MeasurementMethod    string                         `json:"measurement_method"`
	Provenance           agentStartLatencyProvenance    `json:"provenance"`
	ExpectedSamples      int                            `json:"expected_samples"`
	Warmup               agentStartLatencySample        `json:"warmup"`
	OK                   bool                           `json:"ok"`
	BaselineEligible     bool                           `json:"baseline_eligible"`
	FatalError           string                         `json:"fatal_error,omitempty"`
	LatencyOutcomeCounts agentStartLatencyOutcomeCounts `json:"outcome_counts"`
	Samples              []agentStartLatencySample      `json:"samples"`
	Metrics              []agentStartLatencyMetricStats `json:"metrics"`
}

type agentStartLatencyRunState struct {
	Requested  int
	Warmup     agentStartLatencySample
	Samples    []agentStartLatencySample
	FatalError string
}

type agentStartLatencyMetricDefinition struct {
	name     string
	excluded bool
	value    func(agentStartLatencySample) *time.Duration
}

func buildAgentStartLatencyReport(provenance agentStartLatencyProvenance, run agentStartLatencyRunState) (agentStartLatencyReport, error) {
	report := agentStartLatencyReport{
		SchemaVersion:     "1",
		MeasurementMethod: "gc_session_new_to_normalized_transcript_first_turn_completion",
		Provenance:        provenance,
		ExpectedSamples:   run.Requested,
		Warmup:            run.Warmup,
		FatalError:        strings.TrimSpace(run.FatalError),
		Samples:           append([]agentStartLatencySample(nil), run.Samples...),
	}
	if run.Requested < 0 {
		return agentStartLatencyReport{}, fmt.Errorf("expected samples must not be negative")
	}
	if len(report.Samples) != run.Requested {
		return agentStartLatencyReport{}, fmt.Errorf("retained samples = %d, want %d", len(report.Samples), run.Requested)
	}
	if err := validateAgentStartLatencyWarmup(provenance, &report.Warmup); err != nil {
		return agentStartLatencyReport{}, fmt.Errorf("warmup: %w", err)
	}

	completed := 0
	for i := range report.Samples {
		sample := &report.Samples[i]
		if sample.Index != i+1 {
			return agentStartLatencyReport{}, fmt.Errorf("sample %d: index must be %d", i, i+1)
		}
		if err := validateAgentStartLatencySample(provenance, sample); err != nil {
			return agentStartLatencyReport{}, fmt.Errorf("sample %d: %w", sample.Index, err)
		}
		switch sample.Outcome {
		case agentStartOutcomeCompleted:
			report.LatencyOutcomeCounts.Completed++
			completed++
		case agentStartOutcomeIncomplete:
			report.LatencyOutcomeCounts.Incomplete++
		case agentStartOutcomeError:
			report.LatencyOutcomeCounts.Error++
		case agentStartOutcomeCanceled:
			report.LatencyOutcomeCounts.Canceled++
		case agentStartOutcomeNotAttempted:
			report.LatencyOutcomeCounts.NotAttempted++
		}
	}

	definitions := agentStartLatencyMetricDefinitions()
	for _, definition := range definitions {
		values := make([]time.Duration, 0, completed)
		for _, sample := range report.Samples {
			if sample.Outcome != agentStartOutcomeCompleted {
				continue
			}
			if value := definition.value(sample); value != nil {
				values = append(values, *value)
			}
		}
		report.Metrics = append(report.Metrics, agentStartLatencyMetricStats{
			Name:                     definition.name,
			ObservedCount:            len(values),
			MissingCount:             completed - len(values),
			ExcludedFromOptimization: definition.excluded,
			Latency:                  agentStartLatencyPercentileStats(values),
		})
	}

	warmupOK := report.Warmup.Outcome == agentStartOutcomeCompleted
	provenanceOK := agentStartLatencyProvenanceValid(provenance)
	report.OK = provenanceOK && report.FatalError == "" && warmupOK && completed == run.Requested
	report.BaselineEligible = report.OK && run.Requested >= agentStartLatencyDefaultSamples
	return report, nil
}

func validateAgentStartLatencyWarmup(provenance agentStartLatencyProvenance, sample *agentStartLatencySample) error {
	if sample.Outcome == agentStartOutcomeCompleted {
		return validateAgentStartLatencySample(provenance, sample)
	}
	if sample.Outcome == "" {
		return fmt.Errorf("outcome is empty")
	}
	return validateAgentStartLatencySample(provenance, sample)
}

func validateAgentStartLatencySample(provenance agentStartLatencyProvenance, sample *agentStartLatencySample) error {
	switch sample.Outcome {
	case agentStartOutcomeCompleted:
		if strings.TrimSpace(sample.Error) != "" {
			return fmt.Errorf("completed outcome carries an error")
		}
		if err := validateCompletedAgentStartLatencySample(provenance, sample); err != nil {
			return err
		}
	case agentStartOutcomeIncomplete, agentStartOutcomeError, agentStartOutcomeCanceled, agentStartOutcomeNotAttempted:
		if strings.TrimSpace(sample.Error) == "" {
			return fmt.Errorf("%s outcome requires an error detail", sample.Outcome)
		}
		if err := validatePartialAgentStartTimestampOrder(sample.Timestamps); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown outcome %q", sample.Outcome)
	}
	return nil
}

func validateCompletedAgentStartLatencySample(provenance agentStartLatencyProvenance, sample *agentStartLatencySample) error {
	if strings.TrimSpace(sample.RunIdentity) == "" {
		return fmt.Errorf("opaque run identity is empty")
	}
	if strings.TrimSpace(sample.SessionID) == "" {
		return fmt.Errorf("opaque session id is empty")
	}
	if strings.TrimSpace(sample.SessionName) == "" {
		return fmt.Errorf("session name is empty")
	}
	timestamps := sample.Timestamps
	if timestamps.StartInitiatedAt.IsZero() || timestamps.IntentReturnedAt == nil || timestamps.RuntimeAvailableAt == nil || timestamps.CLIProcessExecAt == nil || timestamps.PromptDeliveredAt == nil || timestamps.FirstAssistantOutputAt == nil || timestamps.FirstTurnCompletedAt == nil || timestamps.CleanupCompletedAt == nil {
		return fmt.Errorf("completed sample is missing a required timestamp")
	}
	if provenance.Readiness.Strategy == agentStartReadinessPromptPrefix && timestamps.CLIReadyAt == nil {
		return fmt.Errorf("prompt-prefix readiness is missing cli_ready_at")
	}
	if err := validatePartialAgentStartTimestampOrder(timestamps); err != nil {
		return err
	}
	if strings.TrimSpace(sample.Controller.SessionID) != strings.TrimSpace(sample.SessionID) {
		return fmt.Errorf("controller timing identity %q does not match session id %q", sample.Controller.SessionID, sample.SessionID)
	}
	if sample.Controller.Total <= 0 || sample.Controller.StartCall < 0 || sample.Controller.ZombieRecycle < 0 || sample.Controller.StateSyncRecovery < 0 || sample.Controller.PostStartObserve < 0 || sample.Controller.CommitRefresh < 0 {
		return fmt.Errorf("controller timing is invalid")
	}
	proofs := sample.Terminal
	if !proofs.ExpectedOutputMatched || !proofs.AssistantAfterPrompt || !proofs.TranscriptIdle || !proofs.NoOpenToolUse || !proofs.NoPendingInteraction || !proofs.DurableSessionRetired || !proofs.TmuxSessionAbsent {
		return fmt.Errorf("completed sample is missing a terminal proof")
	}
	durations := agentStartLatencyDurations{
		Total:                   timestamps.FirstTurnCompletedAt.Sub(timestamps.StartInitiatedAt),
		NonInference:            timestamps.PromptDeliveredAt.Sub(timestamps.StartInitiatedAt),
		StartToRuntime:          timestamps.RuntimeAvailableAt.Sub(timestamps.StartInitiatedAt),
		RuntimeToCLIExec:        timestamps.CLIProcessExecAt.Sub(*timestamps.RuntimeAvailableAt),
		PromptToFirstOutput:     timestamps.FirstAssistantOutputAt.Sub(*timestamps.PromptDeliveredAt),
		FirstOutputToCompletion: timestamps.FirstTurnCompletedAt.Sub(*timestamps.FirstAssistantOutputAt),
	}
	if timestamps.CLIReadyAt != nil {
		cliReady := timestamps.CLIReadyAt.Sub(*timestamps.CLIProcessExecAt)
		readyToPrompt := timestamps.PromptDeliveredAt.Sub(*timestamps.CLIReadyAt)
		durations.CLIExecToReady = &cliReady
		durations.ReadyToPrompt = &readyToPrompt
	}
	sample.Durations = &durations
	return nil
}

func validatePartialAgentStartTimestampOrder(timestamps agentStartLatencyTimestamps) error {
	chain := []struct {
		name string
		at   *time.Time
	}{
		{"runtime_available_at", timestamps.RuntimeAvailableAt},
		{"cli_process_exec_at", timestamps.CLIProcessExecAt},
		{"cli_ready_at", timestamps.CLIReadyAt},
		{"prompt_delivered_at", timestamps.PromptDeliveredAt},
		{"first_assistant_output_at", timestamps.FirstAssistantOutputAt},
		{"first_turn_completed_at", timestamps.FirstTurnCompletedAt},
		{"cleanup_completed_at", timestamps.CleanupCompletedAt},
	}
	var previous *time.Time
	previousName := "start_initiated_at"
	if !timestamps.StartInitiatedAt.IsZero() {
		start := timestamps.StartInitiatedAt
		previous = &start
	}
	for _, point := range chain {
		if point.at == nil {
			continue
		}
		if point.at.IsZero() {
			return fmt.Errorf("%s is zero", point.name)
		}
		if previous == nil {
			return fmt.Errorf("%s is present without start_initiated_at", point.name)
		}
		if point.at.Before(*previous) {
			return fmt.Errorf("%s precedes %s", point.name, previousName)
		}
		previous = point.at
		previousName = point.name
	}
	if timestamps.IntentReturnedAt != nil {
		if timestamps.StartInitiatedAt.IsZero() {
			return fmt.Errorf("intent_returned_at is present without start_initiated_at")
		}
		if timestamps.IntentReturnedAt.IsZero() || timestamps.IntentReturnedAt.Before(timestamps.StartInitiatedAt) {
			return fmt.Errorf("intent_returned_at precedes start_initiated_at")
		}
	}
	return nil
}

func agentStartLatencyProvenanceValid(provenance agentStartLatencyProvenance) bool {
	readinessOK := false
	switch provenance.Readiness.Strategy {
	case agentStartReadinessPromptPrefix:
		readinessOK = strings.TrimSpace(provenance.Readiness.PromptPrefix) != "" && provenance.Readiness.Delay >= 0
	case agentStartReadinessFixedDelay:
		readinessOK = provenance.Readiness.PromptPrefix == "" && provenance.Readiness.Delay > 0
	case agentStartReadinessNone:
		readinessOK = provenance.Readiness.PromptPrefix == "" && provenance.Readiness.Delay == 0
	}
	return strings.TrimSpace(provenance.Error) == "" &&
		strings.TrimSpace(provenance.Profile) != "" &&
		strings.TrimSpace(provenance.Provider) != "" &&
		provenance.RuntimeProvider == "tmux" &&
		provenance.ReconcilerMode == "require" &&
		strings.TrimSpace(provenance.GCCommit) != "" &&
		strings.TrimSpace(provenance.HostOS) != "" &&
		strings.TrimSpace(provenance.HostArch) != "" &&
		provenance.CPUCount > 0 &&
		strings.TrimSpace(provenance.CityConfigSHA256) != "" &&
		strings.TrimSpace(provenance.AgentConfigSHA256) != "" &&
		readinessOK &&
		slices.EqualFunc(provenance.Binaries, agentStartLatencyRequiredBinaries, func(binary agentStartLatencyBinary, name string) bool {
			return binary.Name == name && strings.TrimSpace(binary.Path) != "" && strings.TrimSpace(binary.SHA256) != "" && strings.TrimSpace(binary.Version) != ""
		})
}

func agentStartLatencyMetricDefinitions() []agentStartLatencyMetricDefinition {
	fromDurations := func(get func(*agentStartLatencyDurations) *time.Duration) func(agentStartLatencySample) *time.Duration {
		return func(sample agentStartLatencySample) *time.Duration {
			if sample.Durations == nil {
				return nil
			}
			return get(sample.Durations)
		}
	}
	durationValue := func(get func(*agentStartLatencyDurations) time.Duration) func(agentStartLatencySample) *time.Duration {
		return fromDurations(func(durations *agentStartLatencyDurations) *time.Duration {
			value := get(durations)
			return &value
		})
	}
	controllerValue := func(get func(agentStartLatencyControllerTiming) time.Duration) func(agentStartLatencySample) *time.Duration {
		return func(sample agentStartLatencySample) *time.Duration {
			value := get(sample.Controller)
			return &value
		}
	}
	userPromptSubmitHook := func(sample agentStartLatencySample) *time.Duration {
		return sample.UserPromptSubmitHook
	}
	return []agentStartLatencyMetricDefinition{
		{name: agentStartMetricTotal, excluded: true, value: durationValue(func(value *agentStartLatencyDurations) time.Duration { return value.Total })},
		{name: agentStartMetricNonInference, value: durationValue(func(value *agentStartLatencyDurations) time.Duration { return value.NonInference })},
		{name: agentStartMetricStartToRuntime, value: durationValue(func(value *agentStartLatencyDurations) time.Duration { return value.StartToRuntime })},
		{name: agentStartMetricRuntimeToCLIExec, value: durationValue(func(value *agentStartLatencyDurations) time.Duration { return value.RuntimeToCLIExec })},
		{name: agentStartMetricCLIExecToReady, value: fromDurations(func(value *agentStartLatencyDurations) *time.Duration { return value.CLIExecToReady })},
		{name: agentStartMetricReadyToPrompt, value: fromDurations(func(value *agentStartLatencyDurations) *time.Duration { return value.ReadyToPrompt })},
		{name: agentStartMetricPromptToFirstOutput, excluded: true, value: durationValue(func(value *agentStartLatencyDurations) time.Duration { return value.PromptToFirstOutput })},
		{name: agentStartMetricFirstOutputToCompletion, excluded: true, value: durationValue(func(value *agentStartLatencyDurations) time.Duration { return value.FirstOutputToCompletion })},
		{name: agentStartMetricUserPromptSubmitHook, value: userPromptSubmitHook},
		{name: agentStartMetricControllerTotal, value: controllerValue(func(value agentStartLatencyControllerTiming) time.Duration { return value.Total })},
		{name: agentStartMetricControllerStartCall, value: controllerValue(func(value agentStartLatencyControllerTiming) time.Duration { return value.StartCall })},
		{name: agentStartMetricControllerZombieRecycle, value: controllerValue(func(value agentStartLatencyControllerTiming) time.Duration { return value.ZombieRecycle })},
		{name: agentStartMetricControllerStateSync, value: controllerValue(func(value agentStartLatencyControllerTiming) time.Duration { return value.StateSyncRecovery })},
		{name: agentStartMetricControllerPostObserve, value: controllerValue(func(value agentStartLatencyControllerTiming) time.Duration { return value.PostStartObserve })},
		{name: agentStartMetricControllerCommitRefresh, value: controllerValue(func(value agentStartLatencyControllerTiming) time.Duration { return value.CommitRefresh })},
	}
}

func agentStartLatencyPercentileStats(values []time.Duration) *agentStartLatencyPercentiles {
	if len(values) == 0 {
		return nil
	}
	values = append([]time.Duration(nil), values...)
	slices.Sort(values)
	rank := func(percent int) time.Duration {
		return values[(len(values)*percent+99)/100-1]
	}
	return &agentStartLatencyPercentiles{
		Count: len(values),
		P50:   rank(50),
		P95:   rank(95),
		P99:   rank(99),
		Max:   values[len(values)-1],
	}
}

func writeAgentStartLatencyReport(path string, provenance agentStartLatencyProvenance, run agentStartLatencyRunState) error {
	report, err := buildAgentStartLatencyReport(provenance, run)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal agent start latency report: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write agent start latency report: %w", err)
	}
	return nil
}
