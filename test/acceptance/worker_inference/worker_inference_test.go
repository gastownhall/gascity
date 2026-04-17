//go:build acceptance_c

package workerinference_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
	workerpkg "github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/internal/worker/workertest"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var (
	agentsRunningPattern  = regexp.MustCompile(`(?m)^\s*(\d+)/(\d+)\s+agents running\b`)
	createdBeadPattern    = regexp.MustCompile(`(?m)^Created (\S+)\b`)
	createdSessionPattern = regexp.MustCompile(`(?m)^Session (\S+) created\b`)
	codexThreadIDPattern  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

const (
	inferenceProbeTemplate    = "probe"
	inferenceProbeManualID    = "probe-live"
	inferenceProbePromptPath  = "prompts/worker-inference-probe.md"
	inferenceSlingTarget      = inferenceProbeTemplate
	namedSessionModeMetadata  = "configured_named_mode"
	liveBootstrapTimeout      = 90 * time.Second
	liveControlTimeout        = 45 * time.Second
	liveSpawnTimeout          = 5 * time.Minute
	liveSessionStartupTimeout = "3m"
	liveShutdownTimeout       = 60 * time.Second
	liveStopBarrierTimeout    = 90 * time.Second
)

var inferenceDisabledOrders = []string{
	"beads-health",
	"cross-rig-deps",
	"dolt-health",
	"dolt-remotes-patrol",
	"gate-sweep",
	"mol-dog-backup",
	"mol-dog-compactor",
	"mol-dog-doctor",
	"mol-dog-jsonl",
	"mol-dog-phantom-db",
	"mol-dog-reaper",
	"mol-dog-stale-db",
	"orphan-sweep",
	"prune-branches",
	"spawn-storm-detect",
	"wisp-compact",
}

type inferenceRun struct {
	CityDir        string
	WorkBeadID     string
	WorkBead       beadJSON
	SpawnedSession sessionJSON
	OutputPath     string
	OutputContents string
	LastStatus     string
	SessionList    string
	SupervisorLogs string
}

type inferenceSessionRun struct {
	CityDir        string
	Identity       string
	SessionID      string
	SessionAlias   string
	SessionName    string
	SessionKey     string
	OutputPath     string
	OutputContents string
	LastStatus     string
	SessionList    string
	SupervisorLogs string
}

type liveBlockedInteraction struct {
	Kind        string
	Detail      string
	PaneTail    string
	SessionName string
}

func (b *liveBlockedInteraction) err() error {
	if b == nil {
		return nil
	}
	if strings.TrimSpace(b.SessionName) == "" {
		return fmt.Errorf("worker entered blocked interactive state (%s): %s", b.Kind, b.Detail)
	}
	return fmt.Errorf("session %s entered blocked interactive state (%s): %s", b.SessionName, b.Kind, b.Detail)
}

func (b *liveBlockedInteraction) evidence() map[string]string {
	if b == nil {
		return nil
	}
	evidence := map[string]string{
		"blocked_kind":   b.Kind,
		"blocked_detail": b.Detail,
		"pane_tail":      b.PaneTail,
	}
	if strings.TrimSpace(b.SessionName) != "" {
		evidence["blocked_session_name"] = b.SessionName
	}
	return evidence
}

func TestWorkerInferenceSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("WorkerInference: skipping in short mode")
	}

	profileID := workertest.ProfileID(liveSetup.Profile)
	reporter := workertest.NewSuiteReporter(t, "worker-inference", map[string]string{
		"lane":        "live",
		"profile":     string(liveSetup.Profile),
		"provider":    liveSetup.Provider,
		"auth_source": liveSetup.AuthSource,
	})

	if liveSetup.SetupError != "" {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceFreshSpawn, liveSetup.SetupError).WithEvidence(map[string]string{
			"profile":  string(liveSetup.Profile),
			"provider": liveSetup.Provider,
		}))
		t.FailNow()
	}

	harness, err := newLiveWorkerHandleHarness(t)
	if err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceFreshSpawn, err.Error()).WithEvidence(map[string]string{
			"profile":     string(liveSetup.Profile),
			"provider":    liveSetup.Provider,
			"binary_path": liveSetup.BinaryPath,
		}))
		t.FailNow()
	}

	outputRel := fmt.Sprintf("worker-inference-%s.txt", liveSetup.Provider)
	outputText := fmt.Sprintf("%s live inference ok", liveSetup.Provider)
	prompt := fmt.Sprintf("Create a file named %s containing exactly %q and nothing else.", outputRel, outputText)

	startState, startEvidence, err := harness.start()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshSpawn, err.Error(), startEvidence))
		t.FailNow()
	}
	if startState.Phase != workerpkg.PhaseReady {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshSpawn, fmt.Sprintf("worker Start phase = %s, want ready", startState.Phase), startEvidence))
		t.FailNow()
	}
	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshSpawn, "worker handle started a live worker session").WithEvidence(startEvidence))

	_, output, taskEvidence, err := harness.submitAndWaitForFile(prompt, outputRel, workerpkg.DeliveryIntentDefault)
	taskEvidence["expected_output"] = outputText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshTask, err.Error(), taskEvidence))
		t.FailNow()
	}
	if strings.TrimSpace(output) != outputText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshTask, "live worker output did not match the requested content", taskEvidence))
		t.FailNow()
	}
	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshTask, "live worker completed a machine-checkable file-writing task").WithEvidence(taskEvidence))

	snapshot, transcriptEvidence, err := harness.waitForHistory("", "")
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceTranscript, err.Error(), transcriptEvidence))
		t.FailNow()
	}

	transcriptEvidence["transcript_path"] = snapshot.TranscriptStreamID
	transcriptEvidence["entry_count"] = strconv.Itoa(len(snapshot.Entries))
	transcriptEvidence["tail_activity"] = string(snapshot.TailState.Activity)
	transcriptEvidence["logical_conversation_id"] = snapshot.LogicalConversationID
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceTranscript, "live transcript was discovered and normalized after the completed task").WithEvidence(transcriptEvidence))
}

func TestWorkerInferenceWorkspaceTask(t *testing.T) {
	if testing.Short() {
		t.Skip("WorkerInference: skipping in short mode")
	}

	profileID := workertest.ProfileID(liveSetup.Profile)
	reporter := workertest.NewSuiteReporter(t, "worker-inference-workspace", map[string]string{
		"lane":        "live",
		"profile":     string(liveSetup.Profile),
		"provider":    liveSetup.Provider,
		"auth_source": liveSetup.AuthSource,
	})

	if liveSetup.SetupError != "" {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceWorkspaceTask, liveSetup.SetupError).WithEvidence(map[string]string{
			"profile":  string(liveSetup.Profile),
			"provider": liveSetup.Provider,
		}))
		t.FailNow()
	}

	harness, err := newLiveWorkerHandleHarness(t)
	if err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceWorkspaceTask, err.Error()).WithEvidence(map[string]string{
			"profile":     string(liveSetup.Profile),
			"provider":    liveSetup.Provider,
			"binary_path": liveSetup.BinaryPath,
		}))
		t.FailNow()
	}

	inputRel := filepath.Join("inputs", fmt.Sprintf("worker-inference-source-%s.txt", liveSetup.Provider))
	outputRel := fmt.Sprintf("worker-inference-workspace-%s.txt", liveSetup.Provider)
	expected := fmt.Sprintf("workspace-anchor-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	sourceBody := strings.Join([]string{
		"line-one",
		expected,
		"line-three",
	}, "\n") + "\n"
	prompt := fmt.Sprintf(
		"Read the file %q and create a file named %s containing exactly the second line from %q and nothing else.",
		inputRel,
		outputRel,
		inputRel,
	)

	path := filepath.Join(harness.workDir, inputRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceWorkspaceTask, err.Error()).WithEvidence(harness.baseEvidence()))
		t.FailNow()
	}
	if err := os.WriteFile(path, []byte(sourceBody), 0o644); err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceWorkspaceTask, err.Error()).WithEvidence(harness.baseEvidence()))
		t.FailNow()
	}

	startState, spawnEvidence, err := harness.start()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshSpawn, err.Error(), spawnEvidence))
		t.FailNow()
	}
	if startState.Phase != workerpkg.PhaseReady {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshSpawn, fmt.Sprintf("worker Start phase = %s, want ready", startState.Phase), spawnEvidence))
		t.FailNow()
	}

	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshSpawn, "worker handle started a live worker session").WithEvidence(spawnEvidence))

	_, output, taskEvidence, err := harness.submitAndWaitForFile(prompt, outputRel, workerpkg.DeliveryIntentDefault)
	taskEvidence["source_path"] = path
	taskEvidence["expected_output"] = expected
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceWorkspaceTask, err.Error(), taskEvidence))
		t.FailNow()
	}
	if strings.TrimSpace(output) != expected {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceWorkspaceTask, "live worker did not extract the expected workspace content", taskEvidence))
		t.FailNow()
	}
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceWorkspaceTask, "live worker read workspace state and produced the expected machine-checkable output").WithEvidence(taskEvidence))
}

func TestWorkerInferenceContinuationSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("WorkerInference: skipping in short mode")
	}

	profileID := workertest.ProfileID(liveSetup.Profile)
	reporter := workertest.NewSuiteReporter(t, "worker-inference-continuation", map[string]string{
		"lane":        "live",
		"profile":     string(liveSetup.Profile),
		"provider":    liveSetup.Provider,
		"auth_source": liveSetup.AuthSource,
	})

	if liveSetup.SetupError != "" {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceContinuation, liveSetup.SetupError).WithEvidence(map[string]string{
			"profile":  string(liveSetup.Profile),
			"provider": liveSetup.Provider,
		}))
		t.FailNow()
	}

	harness, err := newLiveWorkerHandleHarness(t)
	if err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceContinuation, err.Error()).WithEvidence(map[string]string{
			"profile":     string(liveSetup.Profile),
			"provider":    liveSetup.Provider,
			"binary_path": liveSetup.BinaryPath,
		}))
		t.FailNow()
	}

	anchorText := fmt.Sprintf("continuation-anchor-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	readyRel := fmt.Sprintf("worker-inference-continuation-ready-%s.txt", liveSetup.Provider)
	readyText := "ready"
	firstPrompt := fmt.Sprintf(
		"Create a file named %s containing exactly %q and nothing else. Also remember this exact phrase for a later message: %q. Do not write that remembered phrase to any file right now.",
		readyRel,
		readyText,
		anchorText,
	)

	startState, startEvidence, err := harness.start()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), startEvidence))
		t.FailNow()
	}
	if startState.Phase != workerpkg.PhaseReady {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, fmt.Sprintf("worker Start phase = %s, want ready", startState.Phase), startEvidence))
		t.FailNow()
	}

	_, readyOutput, taskEvidence, err := harness.submitAndWaitForFile(firstPrompt, readyRel, workerpkg.DeliveryIntentDefault)
	taskEvidence["expected_output"] = readyText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), mergeEvidence(startEvidence, taskEvidence)))
		t.FailNow()
	}
	if readyOutput != readyText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, "live worker did not produce the expected bootstrap output", mergeEvidence(startEvidence, taskEvidence)))
		t.FailNow()
	}

	beforeSnapshot, beforeEvidence, err := harness.waitForHistory(firstPrompt, readyText)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence)))
		t.FailNow()
	}

	stopState, stopEvidence, err := harness.stop()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence)))
		t.FailNow()
	}
	if stopState.Phase != workerpkg.PhaseStopped {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, fmt.Sprintf("worker Stop phase = %s, want stopped", stopState.Phase), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence)))
		t.FailNow()
	}

	restartState, restartEvidence, err := harness.start()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence, restartEvidence)))
		t.FailNow()
	}
	if restartState.Phase != workerpkg.PhaseReady {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, fmt.Sprintf("worker restart phase = %s, want ready", restartState.Phase), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence, restartEvidence)))
		t.FailNow()
	}
	if startState.SessionID != "" && restartState.SessionID != "" && startState.SessionID != restartState.SessionID {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, fmt.Sprintf(
			"session id changed across restart: %q -> %q",
			startState.SessionID,
			restartState.SessionID,
		), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence, restartEvidence)))
		t.FailNow()
	}

	recallPrompt := fmt.Sprintf(
		"Without reading files or manually searching history, create a file named %s containing exactly the remembered phrase from our earlier turn and nothing else.",
		fmt.Sprintf("worker-inference-continuation-proof-%s.txt", liveSetup.Provider),
	)
	recallRel := fmt.Sprintf("worker-inference-continuation-proof-%s.txt", liveSetup.Provider)

	_, proofText, proofEvidence, err := harness.submitAndWaitForFile(recallPrompt, recallRel, workerpkg.DeliveryIntentDefault)
	proofEvidence["expected_output"] = anchorText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence, restartEvidence, proofEvidence)))
		t.FailNow()
	}
	if strings.TrimSpace(proofText) != anchorText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, "continued worker did not reproduce the remembered phrase", mergeEvidence(startEvidence, taskEvidence, beforeEvidence, stopEvidence, restartEvidence, proofEvidence)))
		t.FailNow()
	}

	afterSnapshot, continuationEvidence, err := harness.waitForContinuationHistory(beforeSnapshot, recallPrompt)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			stopEvidence,
			restartEvidence,
			proofEvidence,
			continuationEvidence,
		)))
		t.FailNow()
	}

	merged := mergeEvidence(
		startEvidence,
		taskEvidence,
		beforeEvidence,
		stopEvidence,
		restartEvidence,
		proofEvidence,
		continuationEvidence,
		map[string]string{
			"anchor_text":          anchorText,
			"before_transcript":    beforeSnapshot.TranscriptStreamID,
			"after_transcript":     afterSnapshot.TranscriptStreamID,
			"after_entry_count":    strconv.Itoa(len(afterSnapshot.Entries)),
			"after_logical_conv":   afterSnapshot.LogicalConversationID,
			"after_provider_sess":  afterSnapshot.ProviderSessionID,
			"continued_file_value": proofText,
		},
	)
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceContinuation, "restarted live worker resumed the same conversation and recalled prior context").WithEvidence(merged))
}

func TestWorkerInferenceMultiTurnWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("WorkerInference: skipping in short mode")
	}

	profileID := workertest.ProfileID(liveSetup.Profile)
	reporter := workertest.NewSuiteReporter(t, "worker-inference-multi-turn", map[string]string{
		"lane":        "live",
		"profile":     string(liveSetup.Profile),
		"provider":    liveSetup.Provider,
		"auth_source": liveSetup.AuthSource,
		"binary_path": liveSetup.BinaryPath,
	})

	if liveSetup.SetupError != "" {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceMultiTurnWorkflow, liveSetup.SetupError).WithEvidence(map[string]string{
			"profile":  string(liveSetup.Profile),
			"provider": liveSetup.Provider,
		}))
		t.FailNow()
	}

	harness, err := newLiveWorkerHandleHarness(t)
	if err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error()).WithEvidence(map[string]string{
			"profile":     string(liveSetup.Profile),
			"provider":    liveSetup.Provider,
			"binary_path": liveSetup.BinaryPath,
		}))
		t.FailNow()
	}

	anchorText := fmt.Sprintf("multi-turn-anchor-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	readyRel := fmt.Sprintf("worker-inference-multi-turn-ready-%s.txt", liveSetup.Provider)
	readyText := "ready"
	firstPrompt := fmt.Sprintf(
		"Create a file named %s containing exactly %q and nothing else. Also remember this exact phrase for later turns: %q. Do not write that remembered phrase to any file right now.",
		readyRel,
		readyText,
		anchorText,
	)

	startState, startEvidence, err := harness.start()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), startEvidence))
		t.FailNow()
	}
	if startState.Phase != workerpkg.PhaseReady {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, fmt.Sprintf("worker Start phase = %s, want ready", startState.Phase), startEvidence))
		t.FailNow()
	}

	_, firstOutput, taskEvidence, err := harness.submitAndWaitForFile(firstPrompt, readyRel, workerpkg.DeliveryIntentDefault)
	taskEvidence["expected_output"] = readyText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), mergeEvidence(startEvidence, taskEvidence)))
		t.FailNow()
	}
	if firstOutput != readyText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, "first workflow turn did not produce the expected ready output", mergeEvidence(startEvidence, taskEvidence)))
		t.FailNow()
	}

	beforeSnapshot, beforeEvidence, err := harness.waitForHistory(firstPrompt, readyText)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence)))
		t.FailNow()
	}

	secondRel := fmt.Sprintf("worker-inference-multi-turn-memory-%s.txt", liveSetup.Provider)
	secondPrompt := fmt.Sprintf(
		"Without reading files or manually searching history, create a file named %s containing exactly the remembered phrase from our earlier turn and nothing else.",
		secondRel,
	)
	_, secondOutput, secondEvidence, err := harness.submitAndWaitForFile(secondPrompt, secondRel, workerpkg.DeliveryIntentDefault)
	secondEvidence["expected_output"] = anchorText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, secondEvidence)))
		t.FailNow()
	}
	if secondOutput != anchorText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, "second workflow turn did not recall the remembered phrase", mergeEvidence(startEvidence, taskEvidence, beforeEvidence, secondEvidence)))
		t.FailNow()
	}

	secondSnapshot, secondTranscriptEvidence, err := harness.waitForContinuationHistory(beforeSnapshot, secondPrompt)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			secondEvidence,
			secondTranscriptEvidence,
		)))
		t.FailNow()
	}

	summaryRel := fmt.Sprintf("worker-inference-multi-turn-summary-%s.txt", liveSetup.Provider)
	summaryExpected := readyText + "|" + anchorText
	thirdPrompt := fmt.Sprintf(
		"Without reading files or manually searching history, create a file named %s containing exactly the word you wrote to the ready file in our first turn, then a pipe character, then the remembered phrase from that same turn, and nothing else.",
		summaryRel,
	)
	_, thirdOutput, thirdEvidence, err := harness.submitAndWaitForFile(thirdPrompt, summaryRel, workerpkg.DeliveryIntentDefault)
	thirdEvidence["expected_output"] = summaryExpected
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			secondEvidence,
			secondTranscriptEvidence,
			thirdEvidence,
		)))
		t.FailNow()
	}
	if thirdOutput != summaryExpected {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, "final workflow turn did not combine the expected prior-turn context", mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			secondEvidence,
			secondTranscriptEvidence,
			thirdEvidence,
		)))
		t.FailNow()
	}

	thirdSnapshot, thirdTranscriptEvidence, err := harness.waitForContinuationHistory(secondSnapshot, thirdPrompt)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceMultiTurnWorkflow, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			secondEvidence,
			secondTranscriptEvidence,
			thirdEvidence,
			thirdTranscriptEvidence,
		)))
		t.FailNow()
	}

	evidence := mergeEvidence(
		startEvidence,
		taskEvidence,
		beforeEvidence,
		secondEvidence,
		secondTranscriptEvidence,
		thirdEvidence,
		thirdTranscriptEvidence,
		map[string]string{
			"first_transcript":        beforeSnapshot.TranscriptStreamID,
			"second_transcript":       secondSnapshot.TranscriptStreamID,
			"third_transcript":        thirdSnapshot.TranscriptStreamID,
			"first_logical_conv":      beforeSnapshot.LogicalConversationID,
			"second_logical_conv":     secondSnapshot.LogicalConversationID,
			"third_logical_conv":      thirdSnapshot.LogicalConversationID,
			"memory_output_contents":  secondOutput,
			"summary_output_contents": thirdOutput,
		},
	)
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceMultiTurnWorkflow, "live worker completed a multi-turn workflow with machine-checkable context handoff across turns").WithEvidence(evidence))
}

func TestWorkerInferenceInterruptRecoverContinue(t *testing.T) {
	if testing.Short() {
		t.Skip("WorkerInference: skipping in short mode")
	}

	profileID := workertest.ProfileID(liveSetup.Profile)
	reporter := workertest.NewSuiteReporter(t, "worker-inference-interrupt-recover", map[string]string{
		"lane":        "live",
		"profile":     string(liveSetup.Profile),
		"provider":    liveSetup.Provider,
		"auth_source": liveSetup.AuthSource,
		"binary_path": liveSetup.BinaryPath,
	})

	if liveSetup.SetupError != "" {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceInterruptRecoverContinue, liveSetup.SetupError).WithEvidence(map[string]string{
			"profile":  string(liveSetup.Profile),
			"provider": liveSetup.Provider,
		}))
		t.FailNow()
	}

	harness, err := newLiveWorkerHandleHarness(t)
	if err != nil {
		reporter.Record(workertest.EnvironmentError(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error()).WithEvidence(map[string]string{
			"profile":     string(liveSetup.Profile),
			"provider":    liveSetup.Provider,
			"binary_path": liveSetup.BinaryPath,
		}))
		t.FailNow()
	}

	anchorText := fmt.Sprintf("interrupt-anchor-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	readyRel := fmt.Sprintf("worker-inference-interrupt-ready-%s.txt", liveSetup.Provider)
	readyText := "ready"
	firstPrompt := fmt.Sprintf(
		"Create a file named %s containing exactly %q and nothing else. Also remember this exact phrase for later turns: %q. Do not write that remembered phrase to any file right now.",
		readyRel,
		readyText,
		anchorText,
	)

	startState, startEvidence, err := harness.start()
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), startEvidence))
		t.FailNow()
	}
	if startState.Phase != workerpkg.PhaseReady {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, fmt.Sprintf("worker Start phase = %s, want ready", startState.Phase), startEvidence))
		t.FailNow()
	}

	_, firstOutput, taskEvidence, err := harness.submitAndWaitForFile(firstPrompt, readyRel, workerpkg.DeliveryIntentDefault)
	taskEvidence["expected_output"] = readyText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(startEvidence, taskEvidence)))
		t.FailNow()
	}
	if firstOutput != readyText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, "live worker did not produce the expected ready output", mergeEvidence(startEvidence, taskEvidence)))
		t.FailNow()
	}

	beforeSnapshot, beforeEvidence, err := harness.waitForHistory(firstPrompt, readyText)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence)))
		t.FailNow()
	}

	busyDone := fmt.Sprintf("interrupt-first-done-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	busyPrompt := busyTurnPrompt(fmt.Sprintf("interrupt-%s", liveSetup.Provider), 220, busyDone)
	busyState, busyEvidence, err := harness.submit(busyPrompt, workerpkg.DeliveryIntentDefault)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(startEvidence, taskEvidence, beforeEvidence, busyEvidence)))
		t.FailNow()
	}

	busySnapshot, busyTranscriptEvidence, err := harness.waitForHistory(busyPrompt, "")
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
		)))
		t.FailNow()
	}

	busyStartEvidence, err := harness.waitForBusyTurnStart(busyState.SessionName, fmt.Sprintf("interrupt-%s line 1", liveSetup.Provider))
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
		)))
		t.FailNow()
	}

	recoveryText := fmt.Sprintf("interrupt-recovered-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	recoveryRel := fmt.Sprintf("worker-inference-interrupt-recovered-%s.txt", liveSetup.Provider)
	recoveryPrompt := fmt.Sprintf(
		"Create a file named %s containing exactly %q and nothing else.",
		recoveryRel,
		recoveryText,
	)
	_, recoveryOutput, recoveryEvidence, err := harness.submitAndWaitForFile(recoveryPrompt, recoveryRel, workerpkg.DeliveryIntentInterruptNow)
	recoveryEvidence["expected_output"] = recoveryText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
		)))
		t.FailNow()
	}
	if recoveryOutput != recoveryText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, "replacement turn did not produce the expected recovery output", mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
		)))
		t.FailNow()
	}

	recoverySnapshot, recoveryTranscriptEvidence, err := harness.waitForInterruptContinuationHistory(busySnapshot, busyPrompt, recoveryPrompt)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
			recoveryTranscriptEvidence,
		)))
		t.FailNow()
	}
	if historyContainsAfterPrompt(recoverySnapshot, busyPrompt, busyDone) {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, "interrupt_now replacement still allowed the interrupted turn to finish", mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
			recoveryTranscriptEvidence,
			map[string]string{"interrupted_completion_marker": busyDone},
		)))
		t.FailNow()
	}

	continueRel := fmt.Sprintf("worker-inference-interrupt-continue-%s.txt", liveSetup.Provider)
	continuePrompt := fmt.Sprintf(
		"Without reading files or manually searching history, create a file named %s containing exactly the same text you wrote into %s in the immediately prior interrupt-recovery turn, and nothing else.",
		continueRel,
		recoveryRel,
	)
	_, continueOutput, continueEvidence, err := harness.submitAndWaitForFile(continuePrompt, continueRel, workerpkg.DeliveryIntentDefault)
	continueEvidence["expected_output"] = recoveryText
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
			recoveryTranscriptEvidence,
			continueEvidence,
		)))
		t.FailNow()
	}
	if continueOutput != recoveryText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, "post-recovery continuation did not recall the replacement turn output", mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
			recoveryTranscriptEvidence,
			continueEvidence,
		)))
		t.FailNow()
	}

	continueSnapshot, continueTranscriptEvidence, err := harness.waitForContinuationHistory(recoverySnapshot, continuePrompt)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, err.Error(), mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
			recoveryTranscriptEvidence,
			continueEvidence,
			continueTranscriptEvidence,
		)))
		t.FailNow()
	}
	if historyContainsAfterPrompt(continueSnapshot, busyPrompt, busyDone) {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceInterruptRecoverContinue, "interrupted completion marker appeared later in the continued transcript", mergeEvidence(
			startEvidence,
			taskEvidence,
			beforeEvidence,
			busyEvidence,
			busyTranscriptEvidence,
			busyStartEvidence,
			recoveryEvidence,
			recoveryTranscriptEvidence,
			continueEvidence,
			continueTranscriptEvidence,
			map[string]string{"interrupted_completion_marker": busyDone},
		)))
		t.FailNow()
	}

	evidence := mergeEvidence(
		startEvidence,
		taskEvidence,
		beforeEvidence,
		busyEvidence,
		busyTranscriptEvidence,
		busyStartEvidence,
		recoveryEvidence,
		recoveryTranscriptEvidence,
		continueEvidence,
		continueTranscriptEvidence,
		map[string]string{
			"first_transcript":              beforeSnapshot.TranscriptStreamID,
			"interrupted_transcript":        busySnapshot.TranscriptStreamID,
			"recovery_transcript":           recoverySnapshot.TranscriptStreamID,
			"continue_transcript":           continueSnapshot.TranscriptStreamID,
			"first_logical_conv":            beforeSnapshot.LogicalConversationID,
			"interrupted_logical_conv":      busySnapshot.LogicalConversationID,
			"recovery_logical_conv":         recoverySnapshot.LogicalConversationID,
			"continue_logical_conv":         continueSnapshot.LogicalConversationID,
			"interrupted_completion_marker": busyDone,
			"recovery_output_contents":      recoveryOutput,
			"continued_output_contents":     continueOutput,
		},
	)
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceInterruptRecoverContinue, "live worker interrupted an in-flight turn, recovered with a replacement task, and continued the same conversation").WithEvidence(evidence))
}

func submitLiveSession(
	t *testing.T,
	cityDir string,
	identity string,
	expectedSessionName string,
	prompt string,
	intent sessionpkg.SubmitIntent,
) (sessionJSON, map[string]string, error) {
	t.Helper()

	if intent == "" {
		intent = sessionpkg.SubmitIntentDefault
	}

	evidence := map[string]string{
		"city_dir":      cityDir,
		"identity":      identity,
		"session_name":  expectedSessionName,
		"submit_intent": string(intent),
	}
	if blocked, blockErr := detectLiveBlockedInteraction(cityDir, expectedSessionName); blockErr != nil {
		return sessionJSON{}, evidence, fmt.Errorf("checking blocked state before session submit: %w", blockErr)
	} else if blocked != nil {
		return sessionJSON{}, mergeEvidence(evidence, blocked.evidence()), blocked.err()
	}

	client, cityScope, err := liveCityAPIClient(cityDir)
	evidence["api_city_scope"] = cityScope
	if err != nil {
		return sessionJSON{}, evidence, fmt.Errorf("creating city API client: %w", err)
	}

	response, err := client.SubmitSession(identity, prompt, intent)
	evidence["submit_status"] = response.Status
	evidence["submit_id"] = response.ID
	evidence["submit_queued"] = strconv.FormatBool(response.Queued)
	if response.Intent != "" {
		evidence["submit_intent"] = string(response.Intent)
	}
	if err != nil {
		if blocked, blockErr := detectLiveBlockedInteraction(cityDir, expectedSessionName); blockErr == nil && blocked != nil {
			return sessionJSON{}, mergeEvidence(evidence, blocked.evidence(), map[string]string{
				"supervisor_logs": supervisorLogs(cityDir),
			}), blocked.err()
		}
		evidence["supervisor_logs"] = supervisorLogs(cityDir)
		return sessionJSON{}, evidence, fmt.Errorf("session submit failed: %w", err)
	}
	if response.Queued {
		return sessionJSON{}, evidence, fmt.Errorf("session submit queued unexpectedly for intent %s", intent)
	}

	sessionInfo, statusOut, err := waitForSessionRunning(cityDir, identity, expectedSessionName)
	evidence["running_status"] = strings.TrimSpace(statusOut)
	evidence["running_session_id"] = sessionInfo.ID
	evidence["running_session_alias"] = sessionInfo.Alias
	evidence["running_session_name"] = sessionInfo.SessionName
	evidence["running_session_state"] = sessionInfo.State
	evidence["running_session_key"] = sessionInfo.SessionKey
	if err != nil {
		evidence["supervisor_logs"] = supervisorLogs(cityDir)
		return sessionInfo, evidence, fmt.Errorf("session did not reach a running state after submit: %w", err)
	}
	return sessionInfo, evidence, nil
}

func submitLiveSessionTurnAndWaitForFile(
	t *testing.T,
	cityDir string,
	identity string,
	expectedSessionName string,
	prompt string,
	outputPath string,
	intent sessionpkg.SubmitIntent,
) (sessionJSON, string, map[string]string, error) {
	t.Helper()

	sessionInfo, submitEvidence, err := submitLiveSession(t, cityDir, identity, expectedSessionName, prompt, intent)
	evidence := mergeEvidence(submitEvidence, map[string]string{
		"output_path": outputPath,
	})
	if err != nil {
		return sessionInfo, "", evidence, err
	}

	outputText, outputEvidence, err := waitForLiveFileText(cityDir, sessionInfo.SessionName, outputPath, 4*time.Minute)
	evidence = mergeEvidence(evidence, outputEvidence, map[string]string{
		"output_contents": outputText,
	})
	if err != nil {
		evidence["supervisor_logs"] = supervisorLogs(cityDir)
		return sessionInfo, outputText, evidence, err
	}
	return sessionInfo, outputText, evidence, nil
}

func runFreshInitSlingWork(t *testing.T, provider, prompt, outputRel string) (inferenceRun, map[string]string, map[string]string, string, error) {
	return runFreshInitSlingWorkWithSetup(t, provider, prompt, outputRel, nil)
}

func newLiveCity(t *testing.T) *helpers.City {
	t.Helper()

	root, err := acceptanceTempRoot()
	if err != nil {
		t.Fatalf("worker-inference: preparing city temp root: %v", err)
	}
	baseDir, err := os.MkdirTemp(root, "gcwi-live-*")
	if err != nil {
		t.Fatalf("worker-inference: creating city temp dir: %v", err)
	}
	if os.Getenv("GC_ACCEPTANCE_KEEP") != "1" {
		t.Cleanup(func() {
			_ = os.RemoveAll(baseDir)
		})
	}
	cityDir, err := os.MkdirTemp(baseDir, "at-*")
	if err != nil {
		t.Fatalf("worker-inference: creating city dir: %v", err)
	}
	return helpers.NewCityAt(t, liveEnv, cityDir)
}

func installLiveProviderCommandOverride(cityDir, provider, command string, processNames []string) error {
	provider = strings.TrimSpace(provider)
	command = strings.TrimSpace(command)
	if provider == "" || command == "" {
		return nil
	}

	cityPath := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(cityPath)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("[providers.%s]", provider)
	if strings.Contains(string(data), header) {
		return fmt.Errorf("city.toml already defines %s", header)
	}

	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n[providers.%s]\ncommand = %s\npath_check = %s\n", provider, strconv.Quote(command), strconv.Quote(command))
	if len(processNames) > 0 {
		quoted := make([]string, 0, len(processNames))
		for _, name := range processNames {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			quoted = append(quoted, strconv.Quote(name))
		}
		if len(quoted) > 0 {
			fmt.Fprintf(&b, "process_names = [%s]\n", strings.Join(quoted, ", "))
		}
	}
	return os.WriteFile(cityPath, []byte(b.String()), 0o644)
}

func setNamedSessionMode(cityDir, template, mode string) error {
	if strings.TrimSpace(template) == "" || strings.TrimSpace(mode) == "" {
		return nil
	}
	cityPath := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(cityPath)
	if err != nil {
		return err
	}
	updated, content := rewriteNamedSessionMode(string(data), template, mode)
	if !updated {
		return nil
	}
	return os.WriteFile(cityPath, []byte(content), 0o644)
}

func setAgentSuspended(cityDir, name string, suspended bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	cityPath := filepath.Join(cityDir, "city.toml")
	editor := configedit.NewEditor(fsys.OSFS{}, cityPath)
	if suspended {
		return editor.SuspendAgent(name)
	}
	return editor.ResumeAgent(name)
}

func rewriteNamedSessionMode(content, template, mode string) (bool, string) {
	template = strings.TrimSpace(template)
	mode = strings.TrimSpace(mode)
	if template == "" || mode == "" {
		return false, content
	}

	trailingNewline := strings.HasSuffix(content, "\n")
	if trailingNewline {
		content = strings.TrimSuffix(content, "\n")
	}
	lines := strings.Split(content, "\n")
	if len(lines) == 1 && lines[0] == "" {
		return false, content
	}
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "[[named_session]]" {
			continue
		}
		end := i + 1
		for end < len(lines) {
			trimmed := strings.TrimSpace(lines[end])
			if strings.HasPrefix(trimmed, "[[") || (strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[[")) {
				break
			}
			end++
		}
		blockUpdated, block := rewriteNamedSessionModeBlock(lines[i:end], template, mode)
		if !blockUpdated {
			i = end - 1
			continue
		}
		rewritten := append([]string{}, lines[:i]...)
		rewritten = append(rewritten, block...)
		rewritten = append(rewritten, lines[end:]...)
		result := strings.Join(rewritten, "\n")
		if trailingNewline {
			result += "\n"
		}
		return true, result
	}
	if trailingNewline {
		content += "\n"
	}
	return false, content
}

func rewriteNamedSessionModeBlock(block []string, template, mode string) (bool, []string) {
	templateIndex := -1
	modeIndex := -1
	for i, line := range block {
		if value, ok := parseQuotedTOMLKey(line, "template"); ok && strings.TrimSpace(value) == template {
			templateIndex = i
		}
		if _, ok := parseQuotedTOMLKey(line, "mode"); ok {
			modeIndex = i
		}
	}
	if templateIndex < 0 {
		return false, block
	}
	if modeIndex >= 0 {
		if value, ok := parseQuotedTOMLKey(block[modeIndex], "mode"); ok && strings.TrimSpace(value) == mode {
			return false, block
		}
		block[modeIndex] = leadingWhitespace(block[modeIndex]) + "mode = " + strconv.Quote(mode)
		return true, block
	}

	insertAt := templateIndex + 1
	line := leadingWhitespace(block[templateIndex]) + "mode = " + strconv.Quote(mode)
	block = append(block[:insertAt], append([]string{line}, block[insertAt:]...)...)
	return true, block
}

func parseQuotedTOMLKey(line, key string) (string, bool) {
	left, right, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok || strings.TrimSpace(left) != key {
		return "", false
	}
	value, err := strconv.Unquote(strings.TrimSpace(right))
	if err != nil {
		return "", false
	}
	return value, true
}

func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func closeLiveSessionsByTemplate(cityDir, template string) error {
	sessionsOut, err := runGCWithTimeout(liveControlTimeout, liveEnv, cityDir, "session", "list", "--json")
	if err != nil {
		return err
	}
	sessions, err := parseSessionListJSON(sessionsOut)
	if err != nil {
		return err
	}
	store, err := openLiveCityStore(cityDir)
	if err != nil {
		return fmt.Errorf("open city store for stale session cleanup: %w", err)
	}
	for _, session := range sessions {
		if strings.TrimSpace(session.Template) != strings.TrimSpace(template) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(session.State), "closed") {
			continue
		}
		if strings.TrimSpace(session.ID) == "" {
			continue
		}
		if err := store.SetMetadata(session.ID, namedSessionModeMetadata, "on_demand"); err != nil {
			return fmt.Errorf("normalize stale named session mode for %s: %w", session.ID, err)
		}
		closeOut, err := runGCWithTimeout(30*time.Second, liveEnv, cityDir, "session", "close", session.ID)
		if err != nil {
			detail := strings.TrimSpace(closeOut)
			if detail == "" {
				return fmt.Errorf("gc session close %s: %w", session.ID, err)
			}
			return fmt.Errorf("gc session close %s: %w: %s", session.ID, err, detail)
		}
	}
	return nil
}

func openLiveCityStore(cityDir string) (beads.Store, error) {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		return nil, err
	}
	provider := strings.TrimSpace(cfg.Beads.Provider)
	if override := strings.TrimSpace(liveEnv.Get("GC_BEADS")); override != "" {
		provider = override
	}
	if provider == "" {
		provider = "bd"
	}
	if strings.HasPrefix(provider, "exec:") {
		store := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		store.SetEnv(liveBeadStoreEnv(cityDir))
		return store, nil
	}
	switch provider {
	case "file":
		beadsPath := filepath.Join(cityDir, ".gc", "beads.json")
		store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
		if err != nil {
			return nil, err
		}
		store.SetLocker(beads.NewFileFlock(beadsPath + ".lock"))
		return store, nil
	default:
		return beads.NewBdStore(cityDir, beads.ExecCommandRunnerWithEnv(liveBeadStoreEnv(cityDir))), nil
	}
}

func persistLiveSessionKey(cityDir, sessionID, sessionKey string) error {
	sessionID = strings.TrimSpace(sessionID)
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionID == "" || sessionKey == "" {
		return nil
	}
	store, err := openLiveCityStore(cityDir)
	if err != nil {
		return err
	}
	return store.SetMetadata(sessionID, "session_key", sessionKey)
}

func providerResumeSessionKey(provider, providerSessionID string) string {
	providerSessionID = strings.TrimSpace(providerSessionID)
	if providerSessionID == "" {
		return ""
	}
	if strings.Contains(strings.ToLower(provider), "codex") {
		matches := codexThreadIDPattern.FindAllString(providerSessionID, -1)
		if len(matches) > 0 {
			return matches[len(matches)-1]
		}
	}
	return providerSessionID
}

func TestProviderResumeSessionKey(t *testing.T) {
	const codexID = "rollout-2026-04-14T09-54-20-019d8afb-efe8-7280-abf9-5901fd92e0cd"
	if got, want := providerResumeSessionKey("codex/tmux-cli", codexID), "019d8afb-efe8-7280-abf9-5901fd92e0cd"; got != want {
		t.Fatalf("providerResumeSessionKey(codex) = %q, want %q", got, want)
	}
	if got := providerResumeSessionKey("claude/tmux-cli", codexID); got != codexID {
		t.Fatalf("providerResumeSessionKey(claude) = %q, want original", got)
	}
}

func TestContinuationSnapshotErrorAllowsCodexResumeIDAlias(t *testing.T) {
	const (
		transcript = "/tmp/codex/rollout-2026-04-14T09-54-20-019d8afb-efe8-7280-abf9-5901fd92e0cd.jsonl"
		rolloutID  = "rollout-2026-04-14T09-54-20-019d8afb-efe8-7280-abf9-5901fd92e0cd"
		resumeID   = "019d8afb-efe8-7280-abf9-5901fd92e0cd"
		recall     = "recall the earlier phrase"
	)
	before := &workerpkg.HistorySnapshot{
		LogicalConversationID: rolloutID,
		ProviderSessionID:     rolloutID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-1"},
		Entries: []workerpkg.HistoryEntry{
			{ID: "user-1", Actor: workerpkg.ActorUser, Kind: "message", Text: "remember alpha"},
			{ID: "assistant-1", Actor: workerpkg.ActorAssistant, Kind: "message", Text: "remembered"},
		},
	}
	after := &workerpkg.HistorySnapshot{
		LogicalConversationID: resumeID,
		ProviderSessionID:     rolloutID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-2"},
		Entries: []workerpkg.HistoryEntry{
			before.Entries[0],
			before.Entries[1],
			{ID: "user-2", Actor: workerpkg.ActorUser, Kind: "message", Text: recall},
			{ID: "assistant-2", Actor: workerpkg.ActorAssistant, Kind: "message", Text: "alpha"},
		},
	}

	if err := continuationSnapshotError(workerpkg.ProfileCodexTmuxCLI, transcript, before, transcript, after, recall); err != nil {
		t.Fatalf("continuationSnapshotError(codex alias) = %v", err)
	}
	if err := continuationSnapshotError(workerpkg.ProfileClaudeTmuxCLI, transcript, before, transcript, after, recall); err == nil {
		t.Fatalf("continuationSnapshotError(claude alias) succeeded, want strict identity failure")
	}
}

func TestContinuationSnapshotErrorIgnoresClaudeStopHookSummary(t *testing.T) {
	const (
		transcript = "/tmp/claude/session.jsonl"
		sessionID  = "claude-session-1"
		recall     = "recall the earlier phrase"
	)
	before := &workerpkg.HistorySnapshot{
		LogicalConversationID: sessionID,
		ProviderSessionID:     sessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "hook-1"},
		Entries: []workerpkg.HistoryEntry{
			{ID: "user-1", Actor: workerpkg.ActorUser, Kind: "user", Text: "remember alpha"},
			{ID: "assistant-1", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "remembered"},
			{
				ID:    "hook-1",
				Actor: workerpkg.ActorSystem,
				Kind:  "system",
				Provenance: workerpkg.Provenance{
					RawType: "system",
					Raw:     json.RawMessage(`{"type":"system","subtype":"stop_hook_summary"}`),
				},
			},
		},
	}
	after := &workerpkg.HistorySnapshot{
		LogicalConversationID: sessionID,
		ProviderSessionID:     sessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-2"},
		Entries: []workerpkg.HistoryEntry{
			before.Entries[0],
			before.Entries[1],
			{ID: "user-2", Actor: workerpkg.ActorUser, Kind: "user", Text: recall},
			{ID: "assistant-2", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "alpha"},
		},
	}

	if err := continuationSnapshotError(workerpkg.ProfileClaudeTmuxCLI, transcript, before, transcript, after, recall); err != nil {
		t.Fatalf("continuationSnapshotError(claude stop hook summary) = %v", err)
	}
}

func TestContinuationSnapshotErrorAllowsGeminiTranscriptRotation(t *testing.T) {
	const (
		beforeTranscript = "/tmp/gemini/chats/session-2026-04-17T03-12-1ae2114a.json"
		afterTranscript  = "/tmp/gemini/chats/session-2026-04-17T03-15-a0795392.json"
		beforeSessionID  = "1ae2114a-5d40-4d68-90dd-a747fe98484c"
		afterSessionID   = "a0795392-05ea-48c9-81f1-dd4eeb114aab"
		logicalID        = "gc-1"
		recall           = "recall the earlier phrase"
	)
	before := &workerpkg.HistorySnapshot{
		TranscriptStreamID:    beforeTranscript,
		LogicalConversationID: logicalID,
		ProviderSessionID:     beforeSessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-1"},
		Entries: []workerpkg.HistoryEntry{
			{ID: "user-1", Actor: workerpkg.ActorUser, Kind: "message", Text: "remember alpha"},
			{ID: "assistant-1", Actor: workerpkg.ActorAssistant, Kind: "message", Text: "remembered"},
		},
	}
	after := &workerpkg.HistorySnapshot{
		TranscriptStreamID:    afterTranscript,
		LogicalConversationID: logicalID,
		ProviderSessionID:     afterSessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-2"},
		Entries: []workerpkg.HistoryEntry{
			before.Entries[0],
			before.Entries[1],
			{ID: "user-2", Actor: workerpkg.ActorUser, Kind: "message", Text: recall},
			{ID: "assistant-2", Actor: workerpkg.ActorAssistant, Kind: "message", Text: "alpha"},
		},
	}

	if err := continuationSnapshotError(workerpkg.ProfileGeminiTmuxCLI, beforeTranscript, before, afterTranscript, after, recall); err != nil {
		t.Fatalf("continuationSnapshotError(gemini transcript rotation) = %v", err)
	}
}

func TestInterruptContinuationSnapshotErrorAllowsInterruptedTailRewrite(t *testing.T) {
	const (
		transcript        = "/tmp/claude/session.jsonl"
		sessionID         = "claude-session-1"
		interruptedPrompt = "write 220 numbered lines and finish with interrupt-done"
		recoveryPrompt    = "create recovery.txt containing replacement-token"
	)
	before := &workerpkg.HistorySnapshot{
		TranscriptStreamID:    transcript,
		LogicalConversationID: sessionID,
		ProviderSessionID:     sessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-partial"},
		Entries: []workerpkg.HistoryEntry{
			{ID: "user-1", Actor: workerpkg.ActorUser, Kind: "user", Text: "remember alpha"},
			{ID: "assistant-1", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "ready"},
			{ID: "user-2", Actor: workerpkg.ActorUser, Kind: "user", Text: interruptedPrompt},
			{ID: "assistant-partial", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "1\n2\n3"},
		},
	}
	after := &workerpkg.HistorySnapshot{
		TranscriptStreamID:    transcript,
		LogicalConversationID: sessionID,
		ProviderSessionID:     sessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-2"},
		Entries: []workerpkg.HistoryEntry{
			before.Entries[0],
			before.Entries[1],
			before.Entries[2],
			{ID: "user-3", Actor: workerpkg.ActorUser, Kind: "user", Text: recoveryPrompt},
			{ID: "assistant-2", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "replacement-token"},
		},
	}

	if err := continuationSnapshotError(workerpkg.ProfileClaudeTmuxCLI, transcript, before, transcript, after, recoveryPrompt); err == nil {
		t.Fatalf("continuationSnapshotError() succeeded, want strict interrupted-tail preservation failure")
	}
	if err := interruptContinuationSnapshotError(workerpkg.ProfileClaudeTmuxCLI, before, after, interruptedPrompt, recoveryPrompt); err != nil {
		t.Fatalf("interruptContinuationSnapshotError() = %v", err)
	}
}

func TestInterruptContinuationSnapshotErrorAllowsDroppedInterruptedPrompt(t *testing.T) {
	const (
		transcript        = "/tmp/claude/session.jsonl"
		sessionID         = "claude-session-1"
		interruptedPrompt = "write 220 numbered lines and finish with interrupt-done"
		recoveryPrompt    = "create recovery.txt containing replacement-token"
	)
	before := &workerpkg.HistorySnapshot{
		TranscriptStreamID:    transcript,
		LogicalConversationID: sessionID,
		ProviderSessionID:     sessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-partial"},
		Entries: []workerpkg.HistoryEntry{
			{ID: "user-1", Actor: workerpkg.ActorUser, Kind: "user", Text: "remember alpha"},
			{ID: "assistant-1", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "ready"},
			{ID: "user-2", Actor: workerpkg.ActorUser, Kind: "user", Text: interruptedPrompt},
			{ID: "assistant-partial", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "1\n2\n3"},
		},
	}
	after := &workerpkg.HistorySnapshot{
		TranscriptStreamID:    transcript,
		LogicalConversationID: sessionID,
		ProviderSessionID:     sessionID,
		Cursor:                workerpkg.Cursor{AfterEntryID: "assistant-2"},
		Entries: []workerpkg.HistoryEntry{
			before.Entries[0],
			before.Entries[1],
			{ID: "user-3", Actor: workerpkg.ActorUser, Kind: "user", Text: recoveryPrompt},
			{ID: "assistant-2", Actor: workerpkg.ActorAssistant, Kind: "assistant", Text: "replacement-token"},
		},
	}

	if err := interruptContinuationSnapshotError(workerpkg.ProfileClaudeTmuxCLI, before, after, interruptedPrompt, recoveryPrompt); err != nil {
		t.Fatalf("interruptContinuationSnapshotError(dropped interrupted prompt) = %v", err)
	}
}

func liveBeadStoreEnv(cityDir string) map[string]string {
	env := citylayout.CityRuntimeEnvMap(cityDir)
	env["BEADS_DIR"] = filepath.Join(cityDir, ".beads")
	env["GC_RIG"] = ""
	env["GC_RIG_ROOT"] = ""
	if port := liveCurrentDoltPort(cityDir); port != "" {
		env["GC_DOLT_PORT"] = port
		env["BEADS_DOLT_PORT"] = port
	}
	if host := strings.TrimSpace(env["GC_DOLT_HOST"]); host != "" {
		env["BEADS_DOLT_HOST"] = host
	}
	return env
}

func liveCurrentDoltPort(cityDir string) string {
	if data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port")); err == nil {
		if port := strings.TrimSpace(string(data)); port != "" {
			return port
		}
	}
	statePaths, err := filepath.Glob(filepath.Join(cityDir, ".gc", "runtime", "packs", "*", "dolt-state.json"))
	if err != nil {
		return ""
	}
	for _, statePath := range statePaths {
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		state := liveManagedDoltState{}
		if json.Unmarshal(data, &state) != nil {
			continue
		}
		if state.Port > 0 {
			return strconv.Itoa(state.Port)
		}
	}
	return ""
}

func runFreshInitSlingWorkWithSetup(t *testing.T, provider, prompt, outputRel string, setupFn func(cityDir string) error) (inferenceRun, map[string]string, map[string]string, string, error) {
	t.Helper()

	c := newLiveCity(t)
	initArgs := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		initArgs = append(initArgs, "--provider", provider)
	}
	initArgs = append(initArgs, c.Dir)
	initOut, initErr := runGCWithTimeout(liveBootstrapTimeout, liveEnv, "", initArgs...)
	if initErr != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("gc init failed: %w", initErr)
	}
	if setupFn != nil {
		if err := setupFn(c.Dir); err != nil {
			return inferenceRun{}, map[string]string{
				"city_dir":   c.Dir,
				"provider":   provider,
				"output_rel": outputRel,
				"init_out":   strings.TrimSpace(initOut),
			}, nil, "spawn", fmt.Errorf("preparing live worker workspace: %w", err)
		}
	}
	if err := seedLiveProviderState(c.Dir); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("seeding live provider state: %w", err)
	}
	if err := installInferenceProbeAgent(c.Dir, true); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("installing worker inference probe agent: %w", err)
	}
	if err := installLiveProviderCommandOverride(c.Dir, liveSetup.Provider, liveSetup.BinaryPath, liveSetup.ProcessNames); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("installing live provider command override: %w", err)
	}
	if err := setNamedSessionMode(c.Dir, inferenceSlingTarget, "on_demand"); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":     c.Dir,
			"binary_path":  liveSetup.BinaryPath,
			"provider":     provider,
			"sling_target": inferenceSlingTarget,
			"output_rel":   outputRel,
			"init_out":     strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("setting %s named session to on_demand: %w", inferenceSlingTarget, err)
	}
	if err := setAgentSuspended(c.Dir, "mayor", true); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":     c.Dir,
			"binary_path":  liveSetup.BinaryPath,
			"provider":     provider,
			"sling_target": inferenceSlingTarget,
			"output_rel":   outputRel,
			"init_out":     strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("suspending default mayor session: %w", err)
	}
	_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
	_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, c.Dir, "stop", c.Dir)
	_, _ = waitForManagedDoltStopped(c.Dir, liveStopBarrierTimeout)
	if err := closeLiveSessionsByTemplate(c.Dir, "mayor"); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":     c.Dir,
			"binary_path":  liveSetup.BinaryPath,
			"provider":     provider,
			"sling_target": inferenceSlingTarget,
			"output_rel":   outputRel,
			"init_out":     strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("closing stale mayor sessions before live start: %w", err)
	}
	if err := closeLiveSessionsByTemplate(c.Dir, inferenceSlingTarget); err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":     c.Dir,
			"binary_path":  liveSetup.BinaryPath,
			"provider":     provider,
			"sling_target": inferenceSlingTarget,
			"output_rel":   outputRel,
			"init_out":     strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("closing stale %s sessions before live start: %w", inferenceSlingTarget, err)
	}

	startOut, startErr := runGCWithTimeout(liveBootstrapTimeout, liveEnv, c.Dir, "start", c.Dir)
	startTimedOut := isRunTimeout(startErr)
	t.Cleanup(func() {
		_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, c.Dir, "stop", c.Dir)
		_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
		_, _ = waitForManagedDoltStopped(c.Dir, liveStopBarrierTimeout)
	})

	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, c.Dir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		detail := beadStoreNotReadyDetail("bead store did not become ready after gc start", startErr)
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"start_err":  strings.TrimSpace(errorString(startErr)),
			"output_rel": outputRel,
		}, nil, "spawn", errors.New(detail)
	}

	out, err := runGCWithTimeout(liveControlTimeout, liveEnv, c.Dir, "sling", provider, prompt)
	workBeadID := parseCreatedBeadID(out)
	slingTimedOut := isRunTimeout(err)
	if err != nil && !(slingTimedOut && workBeadID != "") {
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"sling_out":  strings.TrimSpace(out),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("gc sling failed: %w", err)
	}
	if workBeadID == "" {
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"sling_out":  strings.TrimSpace(out),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("gc sling output did not include a created bead id")
	}

	workBead, err := showBeadJSON(c.Dir, workBeadID)
	if err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":     c.Dir,
			"provider":     provider,
			"init_out":     strings.TrimSpace(initOut),
			"start_out":    strings.TrimSpace(startOut),
			"work_bead_id": workBeadID,
			"sling_out":    strings.TrimSpace(out),
			"output_rel":   outputRel,
		}, nil, "spawn", err
	}

	var (
		lastStatus        string
		lastSessionJSON   string
		sessionListOut    string
		supervisorLogsOut string
		spawnedSession    sessionJSON
		blocked           *liveBlockedInteraction
	)

	spawned := pollForCondition(liveSpawnTimeout, 5*time.Second, func() bool {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "status")
		lastStatus = strings.TrimSpace(statusOut)
		if statusErr != nil {
			lastStatus = strings.TrimSpace(statusOut + "\nERR: " + statusErr.Error())
		}

		sessionsOut, sessionsErr := runGCWithTimeout(liveControlTimeout, liveEnv, c.Dir, "session", "list", "--json")
		lastSessionJSON = strings.TrimSpace(sessionsOut)
		if sessionsErr != nil {
			lastSessionJSON = strings.TrimSpace(sessionsOut + "\nERR: " + sessionsErr.Error())
			return false
		}

		sessions, err := parseSessionListJSON(sessionsOut)
		if err != nil {
			lastSessionJSON = strings.TrimSpace(sessionsOut + "\nERR: " + err.Error())
			return false
		}

		detected, ok, detectErr := selectInferenceSpawnedSession(sessions, inferenceSlingTarget, func(name string) (bool, error) {
			return tmuxSessionLive(c.Dir, name)
		})
		if detectErr != nil {
			lastSessionJSON = strings.TrimSpace(lastSessionJSON + "\nTMUX_ERR: " + detectErr.Error())
			return false
		}
		if ok {
			spawnedSession = detected
			return true
		}
		return false
	})

	if !spawned {
		sessionListOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "session", "list")
		supervisorLogsOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "supervisor", "logs")
		return inferenceRun{}, map[string]string{
			"city_dir":        c.Dir,
			"provider":        provider,
			"init_out":        strings.TrimSpace(initOut),
			"start_out":       strings.TrimSpace(startOut),
			"work_bead_id":    workBeadID,
			"status":          lastStatus,
			"session_json":    lastSessionJSON,
			"session_list":    strings.TrimSpace(sessionListOut),
			"supervisor_logs": strings.TrimSpace(supervisorLogsOut),
			"work_status":     workBead.Status,
			"routed_to":       metaString(workBead.Metadata, "gc.routed_to"),
		}, nil, "spawn", fmt.Errorf("fresh city never spawned a running %s worker after gc sling", provider)
	}

	outputPath := filepath.Join(c.Dir, outputRel)
	hookNudgeDelivery := freshWorkerNudgeDelivery(provider)
	hookNudgeOut, hookNudgeErr := runGCWithTimeout(
		liveControlTimeout,
		liveEnv,
		c.Dir,
		"session",
		"nudge",
		"--delivery",
		hookNudgeDelivery,
		spawnedSession.SessionName,
		"Check your hook for work assignments, complete the assigned work, and close the work bead.",
	)
	var lastWorkBead beadJSON
	completed := false
	if hookNudgeErr == nil {
		completed = pollForCondition(6*time.Minute, 10*time.Second, func() bool {
			bead, beadErr := showBeadJSON(c.Dir, workBeadID)
			if beadErr == nil {
				lastWorkBead = bead
			}
			data, readErr := os.ReadFile(outputPath)
			if readErr == nil {
				output := strings.TrimSpace(string(data))
				if output != "" && beadErr == nil && bead.Status == "closed" {
					return true
				}
			}
			detected, err := detectLiveBlockedInteraction(c.Dir, spawnedSession.SessionName)
			if err != nil {
				lastStatus = strings.TrimSpace(lastStatus + "\nTMUX_ERR: " + err.Error())
				return false
			}
			if detected != nil {
				blocked = detected
				return true
			}
			return false
		})
	}

	sessionListOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "session", "list")
	supervisorLogsOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "supervisor", "logs")
	outputContents, outputErr := os.ReadFile(outputPath)
	outputDiag := string(outputContents)
	if outputErr != nil {
		outputDiag = outputErr.Error()
	}

	run := inferenceRun{
		CityDir:        c.Dir,
		WorkBeadID:     workBeadID,
		WorkBead:       lastWorkBead,
		SpawnedSession: spawnedSession,
		OutputPath:     outputPath,
		OutputContents: strings.TrimSpace(string(outputContents)),
		LastStatus:     lastStatus,
		SessionList:    strings.TrimSpace(sessionListOut),
		SupervisorLogs: strings.TrimSpace(supervisorLogsOut),
	}
	spawnEvidence := map[string]string{
		"city_dir":      c.Dir,
		"provider":      provider,
		"init_out":      strings.TrimSpace(initOut),
		"start_out":     strings.TrimSpace(startOut),
		"work_bead_id":  workBeadID,
		"sling_out":     strings.TrimSpace(out),
		"session_id":    spawnedSession.ID,
		"session_name":  spawnedSession.SessionName,
		"session_state": spawnedSession.State,
		"session_key":   spawnedSession.SessionKey,
	}
	if slingTimedOut {
		spawnEvidence["sling_timed_out"] = "true"
	}
	if startTimedOut {
		spawnEvidence["start_timed_out"] = "true"
	}
	taskEvidence := map[string]string{
		"city_dir":        c.Dir,
		"provider":        provider,
		"init_out":        strings.TrimSpace(initOut),
		"start_out":       strings.TrimSpace(startOut),
		"work_bead_id":    workBeadID,
		"output_path":     outputPath,
		"output_contents": strings.TrimSpace(outputDiag),
		"session_name":    spawnedSession.SessionName,
		"session_state":   spawnedSession.State,
		"nudge_delivery":  hookNudgeDelivery,
	}
	if trimmed := strings.TrimSpace(hookNudgeOut); trimmed != "" {
		taskEvidence["hook_nudge_out"] = trimmed
	}
	if hookNudgeErr != nil {
		taskEvidence["hook_nudge_err"] = strings.TrimSpace(errorString(hookNudgeErr))
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", fmt.Errorf("nudging %s to check hook: %w", spawnedSession.SessionName, hookNudgeErr)
	}

	if blocked != nil {
		taskEvidence = mergeEvidence(taskEvidence, blocked.evidence())
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", blocked.err()
	}

	if !completed {
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", fmt.Errorf("live %s worker did not complete the routed task within 6m", provider)
	}

	return run, spawnEvidence, taskEvidence, "", nil
}

func freshWorkerNudgeDelivery(provider string) string {
	if strings.TrimSpace(provider) == "claude" {
		return "immediate"
	}
	return "wait-idle"
}

func runFreshManualSessionTurn(t *testing.T, provider, templateName, alias, prompt, outputRel string) (inferenceSessionRun, map[string]string, map[string]string, string, error) {
	t.Helper()

	c := newLiveCity(t)
	initArgs := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		initArgs = append(initArgs, "--provider", provider)
	}
	initArgs = append(initArgs, c.Dir)
	initOut, initErr := runGCWithTimeout(liveBootstrapTimeout, liveEnv, "", initArgs...)
	if initErr != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"template":   templateName,
			"alias":      alias,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("gc init failed: %w", initErr)
	}
	if err := seedLiveProviderState(c.Dir); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"template":   templateName,
			"alias":      alias,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("seeding live provider state: %w", err)
	}
	if err := installInferenceProbeAgent(c.Dir); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"template":   templateName,
			"alias":      alias,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("installing worker inference probe agent: %w", err)
	}
	if err := installLiveProviderCommandOverride(c.Dir, liveSetup.Provider, liveSetup.BinaryPath, liveSetup.ProcessNames); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("installing live provider command override: %w", err)
	}
	if err := setAgentSuspended(c.Dir, "mayor", true); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("suspending default mayor session: %w", err)
	}
	_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
	_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, c.Dir, "stop", c.Dir)
	_, _ = waitForManagedDoltStopped(c.Dir, liveStopBarrierTimeout)
	if err := closeLiveSessionsByTemplate(c.Dir, "mayor"); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("closing stale mayor sessions before live start: %w", err)
	}
	if err := closeLiveSessionsByTemplate(c.Dir, templateName); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"binary_path": liveSetup.BinaryPath,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"output_rel":  outputRel,
			"init_out":    strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("closing stale %s sessions before manual session start: %w", templateName, err)
	}

	newOut, newErr := runGCWithTimeout(90*time.Second, liveEnv, c.Dir, "session", "new", templateName, "--alias", alias, "--no-attach")
	sessionID := parseCreatedSessionID(newOut)
	if newErr != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"init_out":    strings.TrimSpace(initOut),
			"session_out": strings.TrimSpace(newOut),
			"output_rel":  outputRel,
		}, nil, "spawn", fmt.Errorf("gc session new failed: %w", newErr)
	}
	if sessionID == "" {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"init_out":    strings.TrimSpace(initOut),
			"session_out": strings.TrimSpace(newOut),
			"output_rel":  outputRel,
		}, nil, "spawn", fmt.Errorf("gc session new output did not include a session id")
	}

	startOut, startErr := runGCWithTimeout(liveBootstrapTimeout, liveEnv, c.Dir, "start", c.Dir)
	startTimedOut := isRunTimeout(startErr)
	t.Cleanup(func() {
		_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, c.Dir, "stop", c.Dir)
		_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
		_, _ = waitForManagedDoltStopped(c.Dir, liveStopBarrierTimeout)
	})

	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, c.Dir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		detail := beadStoreNotReadyDetail("bead store did not become ready after gc start", startErr)
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"session_id":  sessionID,
			"init_out":    strings.TrimSpace(initOut),
			"session_out": strings.TrimSpace(newOut),
			"start_out":   strings.TrimSpace(startOut),
			"start_err":   strings.TrimSpace(errorString(startErr)),
			"output_rel":  outputRel,
		}, nil, "spawn", errors.New(detail)
	}

	sessionInfo, statusOut, err := waitForSessionRunning(c.Dir, sessionID, "")
	if err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"session_id":  sessionID,
			"init_out":    strings.TrimSpace(initOut),
			"start_out":   strings.TrimSpace(startOut),
			"session_out": strings.TrimSpace(newOut),
			"status":      strings.TrimSpace(statusOut),
			"output_rel":  outputRel,
		}, nil, "spawn", err
	}

	client, cityScope, err := liveCityAPIClient(c.Dir)
	if err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"session_id":  sessionID,
			"session_out": strings.TrimSpace(newOut),
			"start_out":   strings.TrimSpace(startOut),
			"status":      strings.TrimSpace(statusOut),
			"output_rel":  outputRel,
		}, nil, "spawn", fmt.Errorf("creating city API client: %w", err)
	}
	outputPath := filepath.Join(c.Dir, outputRel)
	sessionInfo, statusOut, err = sendSessionMessageWhenReady(c.Dir, sessionID, sessionInfo.SessionName, client, prompt)
	if err != nil {
		evidence := map[string]string{
			"city_dir":         c.Dir,
			"provider":         provider,
			"template":         templateName,
			"alias":            alias,
			"session_id":       sessionID,
			"session_name":     sessionInfo.SessionName,
			"session_key":      sessionInfo.SessionKey,
			"api_city_scope":   cityScope,
			"message_delivery": "session_api",
			"session_out":      strings.TrimSpace(newOut),
			"start_out":        strings.TrimSpace(startOut),
			"status":           strings.TrimSpace(statusOut),
			"output_path":      outputPath,
		}
		return inferenceSessionRun{
			CityDir:      c.Dir,
			SessionID:    sessionID,
			SessionAlias: alias,
			SessionName:  sessionInfo.SessionName,
			SessionKey:   sessionInfo.SessionKey,
			OutputPath:   outputPath,
			LastStatus:   strings.TrimSpace(statusOut),
		}, evidence, evidence, "task", fmt.Errorf("initial session API message failed: %w", err)
	}

	sessionInfo, statusOut, err = waitForSessionRunning(c.Dir, sessionID, sessionInfo.SessionName)
	if err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":         c.Dir,
			"provider":         provider,
			"template":         templateName,
			"alias":            alias,
			"session_id":       sessionID,
			"session_name":     sessionInfo.SessionName,
			"session_key":      sessionInfo.SessionKey,
			"api_city_scope":   cityScope,
			"message_delivery": "session_api",
			"session_out":      strings.TrimSpace(newOut),
			"start_out":        strings.TrimSpace(startOut),
			"status":           strings.TrimSpace(statusOut),
			"output_path":      outputPath,
		}, nil, "task", err
	}
	bootstrapPath := ""
	bootstrapEntryCount := "0"

	var (
		lastStatus     string
		sessionListOut string
		supervisorLogs string
		outputContents string
		blocked        *liveBlockedInteraction
	)
	completed := pollForCondition(6*time.Minute, 10*time.Second, func() bool {
		statusNow, _ := runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "status")
		lastStatus = strings.TrimSpace(statusNow)
		data, readErr := os.ReadFile(outputPath)
		if readErr == nil {
			outputContents = strings.TrimSpace(string(data))
			if outputContents != "" {
				return true
			}
		}
		detected, err := detectLiveBlockedInteraction(c.Dir, sessionInfo.SessionName)
		if err != nil {
			lastStatus = strings.TrimSpace(lastStatus + "\nTMUX_ERR: " + err.Error())
			return false
		}
		if detected != nil {
			blocked = detected
			return true
		}
		return false
	})

	sessionListOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "session", "list")
	supervisorLogs, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "supervisor", "logs")
	if completed {
		adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
		if path, snapshot, _, transcriptErr := waitForTranscript(adapter, liveSetup.Profile, c.Dir, sessionInfo.SessionName, sessionInfo.SessionKey, prompt, outputContents); transcriptErr == nil {
			bootstrapPath = path
			bootstrapEntryCount = strconv.Itoa(len(snapshot.Entries))
		}
	}
	run := inferenceSessionRun{
		CityDir:        c.Dir,
		SessionID:      sessionID,
		SessionAlias:   alias,
		SessionName:    sessionInfo.SessionName,
		OutputPath:     outputPath,
		OutputContents: outputContents,
		LastStatus:     lastStatus,
		SessionList:    strings.TrimSpace(sessionListOut),
		SupervisorLogs: strings.TrimSpace(supervisorLogs),
	}
	spawnEvidence := map[string]string{
		"city_dir":              c.Dir,
		"provider":              provider,
		"template":              templateName,
		"alias":                 alias,
		"session_id":            sessionID,
		"session_name":          sessionInfo.SessionName,
		"session_key":           sessionInfo.SessionKey,
		"gc_session_id":         sessionInfo.SessionKey,
		"bootstrap_transcript":  bootstrapPath,
		"bootstrap_entry_count": bootstrapEntryCount,
		"init_out":              strings.TrimSpace(initOut),
		"start_out":             strings.TrimSpace(startOut),
		"session_out":           strings.TrimSpace(newOut),
	}
	if startTimedOut {
		spawnEvidence["start_timed_out"] = "true"
	}
	taskEvidence := map[string]string{
		"city_dir":              c.Dir,
		"provider":              provider,
		"template":              templateName,
		"alias":                 alias,
		"session_id":            sessionID,
		"session_name":          sessionInfo.SessionName,
		"session_key":           sessionInfo.SessionKey,
		"gc_session_id":         sessionInfo.SessionKey,
		"bootstrap_transcript":  bootstrapPath,
		"bootstrap_entry_count": bootstrapEntryCount,
		"message_delivery":      "session_api",
		"output_path":           outputPath,
		"output_contents":       outputContents,
	}
	if blocked != nil {
		taskEvidence = mergeEvidence(taskEvidence, blocked.evidence())
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", blocked.err()
	}

	if !completed {
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", fmt.Errorf("live %s worker did not complete the manual session task within 6m", provider)
	}

	return run, spawnEvidence, taskEvidence, "", nil
}

func runFreshNamedSessionTurn(t *testing.T, provider, identity, prompt, outputRel string) (inferenceSessionRun, map[string]string, map[string]string, string, error) {
	t.Helper()

	c := newLiveCity(t)
	initArgs := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		initArgs = append(initArgs, "--provider", provider)
	}
	initArgs = append(initArgs, c.Dir)
	initOut, initErr := runGCWithTimeout(liveBootstrapTimeout, liveEnv, "", initArgs...)
	if initErr != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("gc init failed: %w", initErr)
	}
	if err := seedLiveProviderState(c.Dir); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("seeding live provider state: %w", err)
	}
	if err := installInferenceProbeAgent(c.Dir); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("installing worker inference probe agent: %w", err)
	}
	if err := setAgentSuspended(c.Dir, "mayor", true); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("suspending default mayor session: %w", err)
	}
	_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
	_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, c.Dir, "stop", c.Dir)
	_, _ = waitForManagedDoltStopped(c.Dir, liveStopBarrierTimeout)
	if err := closeLiveSessionsByTemplate(c.Dir, "mayor"); err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"output_rel": outputRel,
			"init_out":   strings.TrimSpace(initOut),
		}, nil, "spawn", fmt.Errorf("closing stale mayor sessions before live start: %w", err)
	}

	startOut, startErr := runGCWithTimeout(liveBootstrapTimeout, liveEnv, c.Dir, "start", c.Dir)
	startTimedOut := isRunTimeout(startErr)
	t.Cleanup(func() {
		_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, c.Dir, "stop", c.Dir)
		_, _ = runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
		_, _ = waitForManagedDoltStopped(c.Dir, liveStopBarrierTimeout)
	})

	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, c.Dir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		detail := beadStoreNotReadyDetail("bead store did not become ready after gc start", startErr)
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"start_err":  strings.TrimSpace(errorString(startErr)),
			"output_rel": outputRel,
		}, nil, "spawn", errors.New(detail)
	}

	sessionInfo, statusOut, err := waitForSessionRunning(c.Dir, identity, "")
	if err != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"status":     strings.TrimSpace(statusOut),
			"output_rel": outputRel,
		}, nil, "spawn", err
	}

	adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
	bootstrapPath, bootstrapSnapshot, bootstrapEvidence, err := waitForTranscript(adapter, liveSetup.Profile, c.Dir, sessionInfo.SessionName, sessionInfo.SessionKey, "", "")
	if err != nil {
		evidence := map[string]string{
			"city_dir":      c.Dir,
			"provider":      provider,
			"identity":      identity,
			"session_id":    sessionInfo.ID,
			"session_alias": sessionInfo.Alias,
			"session_name":  sessionInfo.SessionName,
			"session_key":   sessionInfo.SessionKey,
			"gc_session_id": sessionInfo.SessionKey,
			"init_out":      strings.TrimSpace(initOut),
			"start_out":     strings.TrimSpace(startOut),
			"status":        strings.TrimSpace(statusOut),
			"output_rel":    outputRel,
		}
		return inferenceSessionRun{}, mergeEvidence(evidence, bootstrapEvidence), nil, "spawn", fmt.Errorf("session transcript never became ready before first nudge: %w", err)
	}
	if blocked, blockErr := detectLiveBlockedInteraction(c.Dir, sessionInfo.SessionName); blockErr != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":              c.Dir,
			"provider":              provider,
			"identity":              identity,
			"session_id":            sessionInfo.ID,
			"session_alias":         sessionInfo.Alias,
			"session_name":          sessionInfo.SessionName,
			"session_key":           sessionInfo.SessionKey,
			"gc_session_id":         sessionInfo.SessionKey,
			"bootstrap_transcript":  bootstrapPath,
			"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
			"init_out":              strings.TrimSpace(initOut),
			"start_out":             strings.TrimSpace(startOut),
			"status":                strings.TrimSpace(statusOut),
			"output_rel":            outputRel,
		}, nil, "task", fmt.Errorf("checking blocked state before first nudge: %w", blockErr)
	} else if blocked != nil {
		evidence := map[string]string{
			"city_dir":              c.Dir,
			"provider":              provider,
			"identity":              identity,
			"session_id":            sessionInfo.ID,
			"session_alias":         sessionInfo.Alias,
			"session_name":          sessionInfo.SessionName,
			"session_key":           sessionInfo.SessionKey,
			"gc_session_id":         sessionInfo.SessionKey,
			"bootstrap_transcript":  bootstrapPath,
			"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
			"init_out":              strings.TrimSpace(initOut),
			"start_out":             strings.TrimSpace(startOut),
			"status":                strings.TrimSpace(statusOut),
			"output_rel":            outputRel,
		}
		return inferenceSessionRun{}, mergeEvidence(evidence, blocked.evidence()), nil, "task", blocked.err()
	}

	nudgeOut, err := runGCWithTimeout(liveControlTimeout, liveEnv, c.Dir, "session", "nudge", "--delivery", "immediate", identity, prompt)
	nudgeTimedOut := isRunTimeout(err)
	outputPath := filepath.Join(c.Dir, outputRel)
	if err != nil && !nudgeTimedOut {
		return inferenceSessionRun{
				CityDir:      c.Dir,
				Identity:     identity,
				SessionID:    sessionInfo.ID,
				SessionAlias: sessionInfo.Alias,
				SessionName:  sessionInfo.SessionName,
				SessionKey:   sessionInfo.SessionKey,
				OutputPath:   outputPath,
				LastStatus:   strings.TrimSpace(statusOut),
			}, map[string]string{
				"city_dir":              c.Dir,
				"provider":              provider,
				"identity":              identity,
				"session_id":            sessionInfo.ID,
				"session_name":          sessionInfo.SessionName,
				"session_key":           sessionInfo.SessionKey,
				"gc_session_id":         sessionInfo.SessionKey,
				"bootstrap_transcript":  bootstrapPath,
				"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
				"init_out":              strings.TrimSpace(initOut),
				"start_out":             strings.TrimSpace(startOut),
				"status":                strings.TrimSpace(statusOut),
				"output_rel":            outputRel,
			}, map[string]string{
				"city_dir":              c.Dir,
				"provider":              provider,
				"identity":              identity,
				"session_id":            sessionInfo.ID,
				"session_name":          sessionInfo.SessionName,
				"session_key":           sessionInfo.SessionKey,
				"gc_session_id":         sessionInfo.SessionKey,
				"bootstrap_transcript":  bootstrapPath,
				"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
				"nudge_out":             strings.TrimSpace(nudgeOut),
				"output_path":           outputPath,
			}, "task", fmt.Errorf("gc session nudge failed: %w", err)
	}
	if nudgeTimedOut {
		if blocked, blockErr := detectLiveBlockedInteraction(c.Dir, sessionInfo.SessionName); blockErr == nil && blocked != nil {
			evidence := map[string]string{
				"city_dir":              c.Dir,
				"provider":              provider,
				"identity":              identity,
				"session_id":            sessionInfo.ID,
				"session_alias":         sessionInfo.Alias,
				"session_name":          sessionInfo.SessionName,
				"session_key":           sessionInfo.SessionKey,
				"gc_session_id":         sessionInfo.SessionKey,
				"bootstrap_transcript":  bootstrapPath,
				"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
				"nudge_out":             strings.TrimSpace(nudgeOut),
				"output_path":           outputPath,
			}
			return inferenceSessionRun{}, nil, mergeEvidence(evidence, blocked.evidence()), "task", blocked.err()
		}
	}

	var (
		lastStatus     string
		sessionListOut string
		supervisorLogs string
		outputContents string
		blocked        *liveBlockedInteraction
	)
	completed := pollForCondition(6*time.Minute, 10*time.Second, func() bool {
		statusNow, _ := runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "status")
		lastStatus = strings.TrimSpace(statusNow)
		data, readErr := os.ReadFile(outputPath)
		if readErr == nil {
			outputContents = strings.TrimSpace(string(data))
			if outputContents != "" {
				return true
			}
		}
		detected, err := detectLiveBlockedInteraction(c.Dir, sessionInfo.SessionName)
		if err != nil {
			lastStatus = strings.TrimSpace(lastStatus + "\nTMUX_ERR: " + err.Error())
			return false
		}
		if detected != nil {
			blocked = detected
			return true
		}
		return false
	})

	sessionListOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "session", "list")
	supervisorLogs, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "supervisor", "logs")
	run := inferenceSessionRun{
		CityDir:        c.Dir,
		Identity:       identity,
		SessionID:      sessionInfo.ID,
		SessionAlias:   sessionInfo.Alias,
		SessionName:    sessionInfo.SessionName,
		SessionKey:     sessionInfo.SessionKey,
		OutputPath:     outputPath,
		OutputContents: outputContents,
		LastStatus:     lastStatus,
		SessionList:    strings.TrimSpace(sessionListOut),
		SupervisorLogs: strings.TrimSpace(supervisorLogs),
	}
	spawnEvidence := map[string]string{
		"city_dir":              c.Dir,
		"provider":              provider,
		"identity":              identity,
		"session_id":            sessionInfo.ID,
		"session_alias":         sessionInfo.Alias,
		"session_name":          sessionInfo.SessionName,
		"session_key":           sessionInfo.SessionKey,
		"gc_session_id":         sessionInfo.SessionKey,
		"bootstrap_transcript":  bootstrapPath,
		"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
		"init_out":              strings.TrimSpace(initOut),
		"start_out":             strings.TrimSpace(startOut),
	}
	if startTimedOut {
		spawnEvidence["start_timed_out"] = "true"
	}
	taskEvidence := map[string]string{
		"city_dir":              c.Dir,
		"provider":              provider,
		"identity":              identity,
		"session_id":            sessionInfo.ID,
		"session_alias":         sessionInfo.Alias,
		"session_name":          sessionInfo.SessionName,
		"session_key":           sessionInfo.SessionKey,
		"gc_session_id":         sessionInfo.SessionKey,
		"bootstrap_transcript":  bootstrapPath,
		"bootstrap_entry_count": strconv.Itoa(len(bootstrapSnapshot.Entries)),
		"output_path":           outputPath,
		"output_contents":       outputContents,
		"nudge_out":             strings.TrimSpace(nudgeOut),
	}
	if nudgeTimedOut {
		taskEvidence["nudge_timed_out"] = "true"
	}
	if blocked != nil {
		taskEvidence = mergeEvidence(taskEvidence, blocked.evidence())
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", blocked.err()
	}

	if !completed {
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", fmt.Errorf("live %s worker did not complete the named session task within 6m", provider)
	}

	return run, spawnEvidence, taskEvidence, "", nil
}

func seedLiveProviderState(cityDir string) error {
	gcHome := strings.TrimSpace(liveEnv.Get("GC_HOME"))
	if gcHome == "" {
		return fmt.Errorf("GC_HOME is empty")
	}
	switch liveSetup.Profile {
	case workerpkg.ProfileClaudeTmuxCLI:
		for _, path := range []string{
			filepath.Join(gcHome, ".claude.json"),
			filepath.Join(gcHome, ".claude", ".claude.json"),
		} {
			if err := seedClaudeProjectOnboarding(path, cityDir); err != nil {
				return err
			}
		}
	case workerpkg.ProfileCodexTmuxCLI:
		if err := seedCodexProjectTrust(filepath.Join(gcHome, ".codex", "config.toml"), cityDir); err != nil {
			return err
		}
	case workerpkg.ProfileGeminiTmuxCLI:
		if err := seedGeminiFolderTrust(filepath.Join(gcHome, ".gemini", "trustedFolders.json"), cityDir); err != nil {
			return err
		}
	default:
		return nil
	}
	return nil
}

func installInferenceProbeAgent(cityDir string, includeNamedSessionArgs ...bool) error {
	includeNamedSession := true
	if len(includeNamedSessionArgs) > 0 {
		includeNamedSession = includeNamedSessionArgs[0]
	}
	promptPath := filepath.Join(cityDir, inferenceProbePromptPath)
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		return err
	}
	prompt := strings.TrimSpace(`
You are a worker-inference probe session for Gas City tests.

Follow the user's requests directly.
Use the workspace tools when needed.
After startup, do not inspect files, run commands, or do any other work until the user gives you a task.
When a later message asks you to recall prior turn context, use conversation memory rather than searching files or external history unless the user explicitly asks for that.
`)
	if err := os.WriteFile(promptPath, []byte(prompt+"\n"), 0o644); err != nil {
		return err
	}

	maxActiveSessions := 1
	if !includeNamedSession {
		maxActiveSessions = 0
	}

	cityPath := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(cityPath)
	if err != nil {
		return err
	}
	data, hooksChanged, err := ensureInferenceProbeProviderHooks(data)
	if err != nil {
		return err
	}
	var additions []string
	if !strings.Contains(string(data), "\nname = \""+inferenceProbeTemplate+"\"") {
		additions = append(additions, fmt.Sprintf(`

[[agent]]
name = %q
prompt_template = %q
max_active_sessions = %d
`, inferenceProbeTemplate, inferenceProbePromptPath, maxActiveSessions))
	}
	if includeNamedSession && !strings.Contains(string(data), "\n[[named_session]]\ntemplate = \""+inferenceProbeTemplate+"\"") {
		additions = append(additions, fmt.Sprintf(`

[[named_session]]
template = %q
mode = "always"
`, inferenceProbeTemplate))
	}
	if !strings.Contains(string(data), "\n[session]\n") {
		additions = append(additions, fmt.Sprintf(`

[session]
startup_timeout = %q
`, liveSessionStartupTimeout))
	}
	if !strings.Contains(string(data), "\n[orders]\n") {
		additions = append(additions, fmt.Sprintf(`

[orders]
skip = [%s]
`, quotedOrderList(inferenceDisabledOrders)))
	}
	if len(additions) == 0 && !hooksChanged {
		return nil
	}
	return os.WriteFile(cityPath, append(data, []byte(strings.Join(additions, ""))...), 0o644)
}

func ensureInferenceProbeProviderHooks(data []byte) ([]byte, bool, error) {
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(cfg.Workspace.Provider) != "gemini" {
		return data, false, nil
	}
	if stringListContains(cfg.Workspace.InstallAgentHooks, "gemini") {
		return data, false, nil
	}
	if len(cfg.Workspace.InstallAgentHooks) > 0 {
		return nil, false, fmt.Errorf("workspace install_agent_hooks must include gemini for live Gemini worker inference tests")
	}
	updated, err := insertWorkspaceSetting(data, `install_agent_hooks = ["gemini"]`)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func insertWorkspaceSetting(data []byte, setting string) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	workspaceIndex := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "[workspace]" {
			workspaceIndex = i
			break
		}
	}
	if workspaceIndex < 0 {
		return nil, fmt.Errorf("city.toml missing [workspace]")
	}

	insertAt := len(lines)
	for i := workspaceIndex + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "[") {
			insertAt = i
			break
		}
	}
	lines = append(lines[:insertAt], append([]string{setting}, lines[insertAt:]...)...)
	return []byte(strings.Join(lines, "\n")), nil
}

func stringListContains(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func restartLiveCity(cityDir, expectedSessionName string) (string, string, error) {
	stopOut, err := stopLiveCityForRestart(cityDir, expectedSessionName)
	if err != nil {
		return stopOut, "", err
	}
	startOut, err := startLiveCityAfterRestart(cityDir)
	if err != nil {
		return stopOut, startOut, err
	}
	return stopOut, startOut, nil
}

func stopLiveCityForRestart(cityDir, expectedSessionName string) (string, error) {
	stopOut, stopErr := runGCWithTimeout(liveShutdownTimeout, liveEnv, cityDir, "stop", cityDir)
	if stopErr != nil {
		return stopOut, fmt.Errorf("gc stop failed before restart: %w", stopErr)
	}
	supervisorStopOut, supervisorStopErr := runGCWithTimeout(liveShutdownTimeout, liveEnv, "", "supervisor", "stop")
	stopOut = strings.TrimSpace(strings.TrimSpace(stopOut) + "\n" + strings.TrimSpace(supervisorStopOut))
	if supervisorStopErr != nil {
		return stopOut, fmt.Errorf("gc supervisor stop failed before restart: %w", supervisorStopErr)
	}
	stopBarrierOut, stopBarrierErr := waitForManagedDoltStopped(cityDir, liveStopBarrierTimeout)
	stopOut = strings.TrimSpace(strings.TrimSpace(stopOut) + "\n" + strings.TrimSpace(stopBarrierOut))
	if stopBarrierErr != nil {
		return stopOut, stopBarrierErr
	}
	if err := waitForTmuxSessionStopped(expectedSessionName, liveStopBarrierTimeout, 2*time.Second, func(name string) (bool, error) {
		return tmuxSessionLive(cityDir, name)
	}); err != nil {
		return stopOut, err
	}
	return stopOut, nil
}

func startLiveCityAfterRestart(cityDir string) (string, error) {
	startOut, err := runGCWithTimeout(liveBootstrapTimeout, liveEnv, cityDir, "start", cityDir)
	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, cityDir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		return startOut, errors.New(beadStoreNotReadyDetail("bead store did not become ready after restart", err))
	}
	return startOut, nil
}

func beadStoreNotReadyDetail(prefix string, startErr error) string {
	if startErr == nil {
		return prefix
	}
	if isRunTimeout(startErr) {
		return fmt.Sprintf("%s timed out: %v", prefix, startErr)
	}
	return fmt.Sprintf("%s after initial gc start error: %v", prefix, startErr)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type liveManagedDoltState struct {
	Running bool `json:"running"`
	PID     int  `json:"pid"`
	Port    int  `json:"port"`
}

func waitForManagedDoltStopped(cityDir string, timeout time.Duration) (string, error) {
	statePath := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	portPath := filepath.Join(cityDir, ".beads", "dolt-server.port")

	var lastDetail string
	stopped := pollForCondition(timeout, 500*time.Millisecond, func() bool {
		state := liveManagedDoltState{}
		stateRaw := ""
		stateKnown := false
		if data, err := os.ReadFile(statePath); err == nil {
			stateRaw = strings.TrimSpace(string(data))
			if json.Unmarshal(data, &state) == nil {
				stateKnown = true
			} else {
				lastDetail = fmt.Sprintf("unparseable dolt state: %s", stateRaw)
				return false
			}
		} else if !os.IsNotExist(err) {
			lastDetail = fmt.Sprintf("reading dolt state: %v", err)
			return false
		}

		port := 0
		if state.Port > 0 {
			port = state.Port
		} else if data, err := os.ReadFile(portPath); err == nil {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
				port = parsed
			}
		}

		reachable := false
		if port > 0 {
			reachable = liveTCPPortReachable(port)
		}

		lastDetail = fmt.Sprintf("state=%s reachable=%t", stateRaw, reachable)
		switch {
		case stateKnown && state.Running:
			return false
		case reachable:
			return false
		default:
			return true
		}
	})
	if !stopped {
		if strings.TrimSpace(lastDetail) == "" {
			lastDetail = "managed Dolt never reached a stopped state"
		}
		return lastDetail, fmt.Errorf("managed Dolt did not stop before restart: %s", lastDetail)
	}
	if strings.TrimSpace(lastDetail) == "" {
		lastDetail = "managed Dolt stopped"
	}
	return lastDetail, nil
}

func liveTCPPortReachable(port int) bool {
	if port <= 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitForSessionRunning(cityDir, identity, expectedSessionName string) (sessionJSON, string, error) {
	var (
		lastStatus  string
		liveSession sessionJSON
	)
	ready := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		var err error
		liveSession, lastStatus, err = sessionStateSnapshot(cityDir, identity, expectedSessionName, true)
		return err == nil
	})
	if !ready {
		return liveSession, lastStatus, fmt.Errorf("session %s did not reach a running state within the timeout", identity)
	}
	return liveSession, lastStatus, nil
}

func sessionStateSnapshot(cityDir, identity, expectedSessionName string, requireRunning bool) (sessionJSON, string, error) {
	sessionsOut, sessionsErr := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "session", "list", "--json")
	lastSessions := strings.TrimSpace(sessionsOut)
	if sessionsErr != nil {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "status")
		diag := strings.TrimSpace(statusOut + "\nSESSIONS:\n" + sessionsOut + "\nERR: " + sessionsErr.Error())
		if statusErr != nil {
			diag = strings.TrimSpace(diag + "\nSTATUS_ERR: " + statusErr.Error())
		}
		return sessionJSON{}, diag, sessionsErr
	}
	sessions, err := parseSessionListJSON(sessionsOut)
	if err != nil {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "status")
		diag := strings.TrimSpace(statusOut + "\nSESSIONS:\n" + sessionsOut + "\nERR: " + err.Error())
		if statusErr != nil {
			diag = strings.TrimSpace(diag + "\nSTATUS_ERR: " + statusErr.Error())
		}
		return sessionJSON{}, diag, err
	}
	liveSession, _ := selectSessionMatch(sessions, identity, expectedSessionName, requireRunning)
	diag := ""
	if lastSessions != "" {
		diag = strings.TrimSpace("SESSIONS:\n" + lastSessions)
	}
	if liveSession.ID == "" {
		diag = prependStatusDiagnostic(cityDir, diag)
		return liveSession, diag, fmt.Errorf("session %s not present", identity)
	}
	if expectedSessionName != "" && liveSession.SessionName != expectedSessionName {
		diag = prependStatusDiagnostic(cityDir, diag)
		return liveSession, diag, fmt.Errorf("session %s present with unexpected runtime name %q", identity, liveSession.SessionName)
	}
	if strings.TrimSpace(liveSession.SessionName) == "" {
		diag = prependStatusDiagnostic(cityDir, diag)
		return liveSession, diag, fmt.Errorf("session %s missing runtime name", identity)
	}
	if !requireRunning {
		if strings.EqualFold(strings.TrimSpace(liveSession.State), "closed") {
			diag = prependStatusDiagnostic(cityDir, diag)
			return liveSession, diag, fmt.Errorf("session %s is closed", identity)
		}
		return liveSession, diag, nil
	}
	if !sessionStateCountsAsRunning(liveSession.State) {
		return liveSession, diag, fmt.Errorf("session %s not running yet", identity)
	}
	return liveSession, diag, nil
}

func prependStatusDiagnostic(cityDir, diag string) string {
	statusOut, statusErr := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "status")
	statusDiag := strings.TrimSpace(statusOut)
	if statusErr != nil {
		statusDiag = strings.TrimSpace(statusDiag + "\nSTATUS_ERR: " + statusErr.Error())
	}
	return strings.TrimSpace(statusDiag + "\n" + diag)
}

func liveCityAPIClient(cityDir string) (*api.Client, string, error) {
	gcHome := strings.TrimSpace(liveEnv.Get("GC_HOME"))
	if gcHome == "" {
		return nil, "", fmt.Errorf("GC_HOME is empty")
	}
	supervisorCfg, err := supervisor.LoadConfig(filepath.Join(gcHome, "supervisor.toml"))
	if err != nil {
		return nil, "", fmt.Errorf("load supervisor config: %w", err)
	}
	cityCfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		return nil, "", fmt.Errorf("load city config: %w", err)
	}
	cityName := strings.TrimSpace(cityCfg.Workspace.Name)
	if cityName == "" {
		cityName = filepath.Base(cityDir)
	}
	bind := supervisorCfg.Supervisor.BindOrDefault()
	switch bind {
	case "0.0.0.0":
		bind = "127.0.0.1"
	case "::", "[::]":
		bind = "::1"
	}
	baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(supervisorCfg.Supervisor.PortOrDefault())))
	return api.NewCityScopedClient(baseURL, cityName), cityName, nil
}

func sendSessionMessageWhenReady(cityDir, identity, expectedSessionName string, client *api.Client, message string) (sessionJSON, string, error) {
	deadline := time.Now().Add(4 * time.Minute)
	var (
		lastErr    error
		lastStatus string
		info       sessionJSON
	)
	for {
		snapshot, statusOut, snapshotErr := sessionStateSnapshot(cityDir, identity, expectedSessionName, false)
		if strings.TrimSpace(statusOut) != "" {
			lastStatus = strings.TrimSpace(statusOut)
		}
		if snapshot.ID != "" {
			info = snapshot
			if expectedSessionName == "" {
				expectedSessionName = snapshot.SessionName
			}
		}
		switch {
		case snapshotErr == nil:
			sendErr := client.SendSessionMessage(identity, message)
			if sendErr == nil {
				return info, lastStatus, nil
			}
			lastErr = sendErr
			if !api.IsConnError(sendErr) {
				return info, lastStatus, sendErr
			}
		case info.ID != "" && strings.EqualFold(strings.TrimSpace(info.State), "closed"):
			return info, lastStatus, fmt.Errorf("session %s closed before the first message could be delivered", identity)
		default:
			lastErr = snapshotErr
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("session %s did not accept the first message before timeout", identity)
	}
	return info, lastStatus, lastErr
}

func selectSessionMatch(sessions []sessionJSON, identity, expectedSessionName string, requireRunning bool) (sessionJSON, bool) {
	identity = strings.TrimSpace(identity)
	expectedSessionName = strings.TrimSpace(expectedSessionName)
	bestIdx := -1
	var bestScore sessionMatchScore
	for i, session := range sessions {
		score, ok := scoreSessionMatch(session, identity, expectedSessionName)
		if !ok {
			continue
		}
		if requireRunning && sessionStateCountsAsRunning(session.State) {
			score.Running = 1
		}
		if !strings.EqualFold(strings.TrimSpace(session.State), "closed") {
			score.Open = 1
		}
		if strings.TrimSpace(session.SessionName) != "" {
			score.HasSessionName = 1
		}
		if ts, ok := parseSessionLastActive(session.LastActive); ok {
			score.HasLastActive = 1
			score.LastActiveUnix = ts.UnixNano()
		}
		if bestIdx == -1 || score.betterThan(bestScore) {
			bestIdx = i
			bestScore = score
		}
	}
	if bestIdx == -1 {
		return sessionJSON{}, false
	}
	return sessions[bestIdx], true
}

type sessionMatchScore struct {
	ExpectedName   int
	ID             int
	SessionName    int
	Alias          int
	Running        int
	Open           int
	HasSessionName int
	HasLastActive  int
	LastActiveUnix int64
}

func (s sessionMatchScore) betterThan(other sessionMatchScore) bool {
	switch {
	case s.ExpectedName != other.ExpectedName:
		return s.ExpectedName > other.ExpectedName
	case s.ID != other.ID:
		return s.ID > other.ID
	case s.SessionName != other.SessionName:
		return s.SessionName > other.SessionName
	case s.Alias != other.Alias:
		return s.Alias > other.Alias
	case s.Running != other.Running:
		return s.Running > other.Running
	case s.Open != other.Open:
		return s.Open > other.Open
	case s.HasLastActive != other.HasLastActive:
		return s.HasLastActive > other.HasLastActive
	case s.LastActiveUnix != other.LastActiveUnix:
		return s.LastActiveUnix > other.LastActiveUnix
	case s.HasSessionName != other.HasSessionName:
		return s.HasSessionName > other.HasSessionName
	default:
		return false
	}
}

func scoreSessionMatch(session sessionJSON, identity, expectedSessionName string) (sessionMatchScore, bool) {
	var score sessionMatchScore
	if expectedSessionName != "" && strings.TrimSpace(session.SessionName) == expectedSessionName {
		score.ExpectedName = 1
	}
	if identity == "" {
		return score, score.ExpectedName == 1
	}
	switch {
	case strings.TrimSpace(session.ID) == identity:
		score.ID = 1
	case strings.TrimSpace(session.SessionName) == identity:
		score.SessionName = 1
	case strings.TrimSpace(session.Alias) == identity:
		score.Alias = 1
	default:
		if score.ExpectedName == 0 {
			return sessionMatchScore{}, false
		}
	}
	return score, true
}

func parseSessionLastActive(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

func sessionStateCountsAsRunning(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "active", "awake":
		return true
	default:
		return false
	}
}

func selectInferenceSpawnedSession(sessions []sessionJSON, fallbackSessionName string, isSessionLive func(string) (bool, error)) (sessionJSON, bool, error) {
	for _, session := range sessions {
		if session.Template != inferenceSlingTarget {
			continue
		}
		if strings.TrimSpace(session.SessionName) == "" {
			continue
		}
		live, err := isSessionLive(session.SessionName)
		if err != nil {
			return sessionJSON{}, false, err
		}
		if live || sessionStateCountsAsRunning(session.State) {
			if live && !sessionStateCountsAsRunning(session.State) {
				session.State = "active"
			}
			return session, true, nil
		}
	}
	fallbackSessionName = strings.TrimSpace(fallbackSessionName)
	if fallbackSessionName == "" {
		return sessionJSON{}, false, nil
	}
	live, err := isSessionLive(fallbackSessionName)
	if err != nil {
		return sessionJSON{}, false, err
	}
	if !live {
		return sessionJSON{}, false, nil
	}
	return sessionJSON{
		Template:    inferenceSlingTarget,
		Alias:       fallbackSessionName,
		State:       "active",
		SessionName: fallbackSessionName,
	}, true, nil
}

func waitForTmuxSessionStopped(sessionName string, timeout, interval time.Duration, isSessionLive func(string) (bool, error)) error {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return nil
	}
	if interval <= 0 {
		interval = time.Second
	}
	var (
		lastErr  error
		lastLive bool
	)
	stopped := pollForCondition(timeout, interval, func() bool {
		live, err := isSessionLive(sessionName)
		if err != nil {
			lastErr = err
			return false
		}
		lastErr = nil
		lastLive = live
		return !live
	})
	if stopped {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	if lastLive {
		return fmt.Errorf("tmux session %q still running after gc stop", sessionName)
	}
	return nil
}

func waitForTranscript(adapter workerpkg.SessionLogAdapter, profile workerpkg.Profile, workDir, sessionName, gcSessionID, prompt, outputText string) (string, *workerpkg.HistorySnapshot, map[string]string, error) {
	evidence := map[string]string{
		"work_dir":      workDir,
		"profile":       string(profile),
		"gc_session_id": gcSessionID,
	}
	if strings.TrimSpace(sessionName) != "" {
		evidence["session_name"] = sessionName
	}
	wantPrompt := strings.TrimSpace(prompt)
	wantOutput := strings.TrimSpace(outputText)
	var (
		transcriptPath string
		snapshot       *workerpkg.HistorySnapshot
		lastErr        error
		blocked        *liveBlockedInteraction
	)
	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		candidates := transcriptCandidatePaths(adapter, profile, workDir, gcSessionID)
		if len(candidates) == 0 {
			lastErr = fmt.Errorf("no transcript discovered for %s under %s", profile, workDir)
		}
		for _, candidatePath := range candidates {
			candidateSnapshot, err := adapter.LoadHistory(workerpkg.LoadRequest{
				Provider:       string(profile),
				TranscriptPath: candidatePath,
				GCSessionID:    gcSessionID,
			})
			if err != nil {
				lastErr = err
				continue
			}
			if len(candidateSnapshot.Entries) == 0 {
				lastErr = fmt.Errorf("normalized transcript for %s is empty", profile)
				continue
			}
			transcriptPath = candidatePath
			snapshot = candidateSnapshot
			if wantPrompt == "" && wantOutput == "" {
				return true
			}
			if historyContainsExpectedEvidence(candidateSnapshot, wantPrompt, wantOutput) {
				return true
			}
			lastErr = fmt.Errorf("live transcript for %s did not contain the expected task evidence", profile)
		}
		detected, err := detectLiveBlockedInteraction(workDir, sessionName)
		if err != nil {
			lastErr = err
			return false
		}
		if detected != nil {
			blocked = detected
			lastErr = detected.err()
			return true
		}
		return false
	})
	evidence["transcript_path"] = transcriptPath
	if blocked != nil {
		return transcriptPath, snapshot, mergeEvidence(evidence, blocked.evidence()), blocked.err()
	}
	if found {
		return transcriptPath, snapshot, evidence, nil
	}
	if lastErr != nil {
		return transcriptPath, snapshot, evidence, lastErr
	}
	return transcriptPath, snapshot, evidence, fmt.Errorf("live transcript for %s did not contain the expected task evidence", profile)
}

func transcriptCandidatePaths(adapter workerpkg.SessionLogAdapter, profile workerpkg.Profile, workDir, gcSessionID string) []string {
	var candidates []string
	if discovered := strings.TrimSpace(adapter.DiscoverTranscript(string(profile), workDir, gcSessionID)); discovered != "" {
		candidates = append(candidates, discovered)
	}
	if profile == workerpkg.ProfileGeminiTmuxCLI {
		candidates = append(candidates, geminiTranscriptCandidatePaths(adapter.SearchPaths, workDir)...)
	}
	return uniqueNonEmptyPaths(candidates)
}

func geminiTranscriptCandidatePaths(searchPaths []string, workDir string) []string {
	type candidate struct {
		path    string
		modTime time.Time
	}
	var candidates []candidate
	for _, projectDir := range geminiProjectCandidateDirs(searchPaths, workDir) {
		entries, err := os.ReadDir(filepath.Join(projectDir, "chats"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".json") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			candidates = append(candidates, candidate{
				path:    filepath.Join(projectDir, "chats", name),
				modTime: info.ModTime(),
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.path)
	}
	return paths
}

func geminiProjectCandidateDirs(searchPaths []string, workDir string) []string {
	var candidates []string
	for _, root := range searchPaths {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if geminiProjectRoot(root) == workDir {
			candidates = append(candidates, root)
		}
		if projectDir := geminiProjectDirFromProjects(root, workDir); projectDir != "" {
			candidates = append(candidates, projectDir)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if geminiProjectRoot(dir) == workDir {
				candidates = append(candidates, dir)
			}
		}
	}
	return uniqueNonEmptyPaths(candidates)
}

func geminiProjectDirFromProjects(root, workDir string) string {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(root), "projects.json"))
	if err != nil {
		return ""
	}
	var projects struct {
		Projects map[string]string `json:"projects"`
	}
	if err := json.Unmarshal(data, &projects); err != nil {
		return ""
	}
	dirName := strings.TrimSpace(projects.Projects[workDir])
	if dirName == "" {
		return ""
	}
	return filepath.Join(root, dirName)
}

func geminiProjectRoot(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".project_root"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func uniqueNonEmptyPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}

func waitForTranscriptPath(adapter workerpkg.SessionLogAdapter, profile workerpkg.Profile, transcriptPath, gcSessionID string) (string, *workerpkg.HistorySnapshot, map[string]string, error) {
	transcriptPath = strings.TrimSpace(transcriptPath)
	evidence := map[string]string{
		"profile":         string(profile),
		"gc_session_id":   gcSessionID,
		"transcript_path": transcriptPath,
	}
	if transcriptPath == "" {
		return "", nil, evidence, fmt.Errorf("transcript path is empty for %s", profile)
	}
	var (
		snapshot *workerpkg.HistorySnapshot
		lastErr  error
	)
	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		snapshot, lastErr = adapter.LoadHistory(workerpkg.LoadRequest{
			Provider:       string(profile),
			TranscriptPath: transcriptPath,
			GCSessionID:    gcSessionID,
		})
		if lastErr != nil {
			return false
		}
		if snapshot == nil || len(snapshot.Entries) == 0 {
			lastErr = fmt.Errorf("normalized transcript for %s is empty", profile)
			return false
		}
		return true
	})
	if found {
		return transcriptPath, snapshot, evidence, nil
	}
	if lastErr != nil {
		return transcriptPath, snapshot, evidence, lastErr
	}
	return transcriptPath, snapshot, evidence, fmt.Errorf("transcript path %q for %s never became ready", transcriptPath, profile)
}

func waitForContinuationTranscript(
	adapter workerpkg.SessionLogAdapter,
	profile workerpkg.Profile,
	workDir string,
	sessionName string,
	gcSessionID string,
	beforeTranscriptPath string,
	beforeSnapshot *workerpkg.HistorySnapshot,
	recallPrompt string,
) (string, *workerpkg.HistorySnapshot, map[string]string, error) {
	evidence := map[string]string{
		"work_dir":             workDir,
		"profile":              string(profile),
		"gc_session_id":        gcSessionID,
		"before_transcript":    beforeTranscriptPath,
		"before_entry_count":   strconv.Itoa(len(beforeSnapshot.Entries)),
		"before_logical_conv":  beforeSnapshot.LogicalConversationID,
		"before_provider_sess": beforeSnapshot.ProviderSessionID,
	}
	if strings.TrimSpace(sessionName) != "" {
		evidence["session_name"] = sessionName
	}
	var (
		transcriptPath string
		snapshot       *workerpkg.HistorySnapshot
		lastErr        error
		blocked        *liveBlockedInteraction
	)

	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		transcriptPath = strings.TrimSpace(beforeTranscriptPath)
		if transcriptPath == "" {
			transcriptPath = adapter.DiscoverTranscript(string(profile), workDir, gcSessionID)
		}
		if strings.TrimSpace(transcriptPath) != "" {
			snapshot, lastErr = adapter.LoadHistory(workerpkg.LoadRequest{
				Provider:       string(profile),
				TranscriptPath: transcriptPath,
				GCSessionID:    gcSessionID,
			})
			if lastErr == nil && snapshot != nil {
				lastErr = continuationSnapshotError(profile, beforeTranscriptPath, beforeSnapshot, transcriptPath, snapshot, recallPrompt)
				if lastErr == nil {
					return true
				}
			}
		} else {
			lastErr = fmt.Errorf("no transcript discovered for %s under %s after restart", profile, workDir)
		}
		detected, err := detectLiveBlockedInteraction(workDir, sessionName)
		if err != nil {
			lastErr = err
			return false
		}
		if detected != nil {
			blocked = detected
			lastErr = detected.err()
			return true
		}
		return false
	})

	evidence["after_transcript"] = transcriptPath
	if blocked != nil {
		return transcriptPath, snapshot, mergeEvidence(evidence, blocked.evidence()), blocked.err()
	}
	if found {
		return transcriptPath, snapshot, evidence, nil
	}
	if lastErr != nil {
		return transcriptPath, snapshot, evidence, lastErr
	}
	return transcriptPath, snapshot, evidence, fmt.Errorf("continuation transcript for %s did not show the restarted recall turn", profile)
}

func historyContains(snapshot *workerpkg.HistorySnapshot, needle string) bool {
	needle = strings.TrimSpace(needle)
	if snapshot == nil || needle == "" {
		return false
	}
	for _, entry := range snapshot.Entries {
		if strings.Contains(entry.Text, needle) {
			return true
		}
		for _, block := range entry.Blocks {
			if strings.Contains(block.Text, needle) {
				return true
			}
		}
	}
	return false
}

func historyContainsAfterPrompt(snapshot *workerpkg.HistorySnapshot, prompt, needle string) bool {
	needle = strings.TrimSpace(needle)
	if snapshot == nil || needle == "" {
		return false
	}
	promptIndex := findEntryTextIndex(snapshot.Entries, 0, prompt)
	if promptIndex < 0 {
		return false
	}
	return entriesContainText(snapshot.Entries, promptIndex+1, needle)
}

func entriesContainText(entries []workerpkg.HistoryEntry, start int, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	if start < 0 {
		start = 0
	}
	for idx := start; idx < len(entries); idx++ {
		entry := entries[idx]
		if strings.Contains(entry.Text, needle) {
			return true
		}
		for _, block := range entry.Blocks {
			if strings.Contains(block.Text, needle) {
				return true
			}
		}
	}
	return false
}

func historyContainsExpectedEvidence(snapshot *workerpkg.HistorySnapshot, prompt, outputText string) bool {
	prompt = strings.TrimSpace(prompt)
	if prompt != "" {
		return historyContains(snapshot, prompt)
	}
	return historyContains(snapshot, outputText)
}

func continuationSnapshotError(
	profile workerpkg.Profile,
	beforeTranscriptPath string,
	before *workerpkg.HistorySnapshot,
	afterTranscriptPath string,
	after *workerpkg.HistorySnapshot,
	recallPrompt string,
) error {
	if before == nil || after == nil {
		return fmt.Errorf("missing normalized history snapshot")
	}
	if requiresStableTranscriptPath(profile) && beforeTranscriptPath != afterTranscriptPath {
		return fmt.Errorf("transcript path changed from %q to %q", beforeTranscriptPath, afterTranscriptPath)
	}
	if strings.TrimSpace(before.LogicalConversationID) == "" || strings.TrimSpace(after.LogicalConversationID) == "" {
		return fmt.Errorf("logical conversation identity is empty")
	}
	if !sameContinuationIdentity(profile, before.LogicalConversationID, after.LogicalConversationID) {
		return fmt.Errorf("logical conversation changed from %q to %q", before.LogicalConversationID, after.LogicalConversationID)
	}
	if requiresStableProviderSession(profile) && before.ProviderSessionID != "" && after.ProviderSessionID != "" && !sameContinuationIdentity(profile, before.ProviderSessionID, after.ProviderSessionID) {
		return fmt.Errorf("provider session changed from %q to %q", before.ProviderSessionID, after.ProviderSessionID)
	}
	if strings.TrimSpace(before.Cursor.AfterEntryID) == "" || strings.TrimSpace(after.Cursor.AfterEntryID) == "" {
		return fmt.Errorf("continuation cursor is empty")
	}
	if before.Cursor.AfterEntryID == after.Cursor.AfterEntryID {
		return fmt.Errorf("continuation cursor did not advance")
	}
	historyEnd := historySubsequenceEnd(after.Entries, continuationComparableEntries(before.Entries))
	if historyEnd < 0 {
		return fmt.Errorf("continued transcript does not preserve prior normalized history")
	}
	promptIndex := findEntryTextIndex(after.Entries, historyEnd, recallPrompt)
	if promptIndex < 0 {
		return fmt.Errorf("continued transcript missing recall prompt %q", recallPrompt)
	}
	if promptIndex >= len(after.Entries)-1 {
		return fmt.Errorf("continued transcript did not record any response after the recall prompt")
	}
	return nil
}

func interruptContinuationSnapshotError(
	profile workerpkg.Profile,
	before *workerpkg.HistorySnapshot,
	after *workerpkg.HistorySnapshot,
	interruptedPrompt string,
	recoveryPrompt string,
) error {
	if before == nil || after == nil {
		return fmt.Errorf("missing normalized history snapshot")
	}
	if beforePath, afterPath := strings.TrimSpace(before.TranscriptStreamID), strings.TrimSpace(after.TranscriptStreamID); requiresStableTranscriptPath(profile) && beforePath != "" && afterPath != "" && beforePath != afterPath {
		return fmt.Errorf("transcript path changed from %q to %q", beforePath, afterPath)
	}
	if strings.TrimSpace(before.LogicalConversationID) == "" || strings.TrimSpace(after.LogicalConversationID) == "" {
		return fmt.Errorf("logical conversation identity is empty")
	}
	if !sameContinuationIdentity(profile, before.LogicalConversationID, after.LogicalConversationID) {
		return fmt.Errorf("logical conversation changed from %q to %q", before.LogicalConversationID, after.LogicalConversationID)
	}
	if requiresStableProviderSession(profile) && before.ProviderSessionID != "" && after.ProviderSessionID != "" && !sameContinuationIdentity(profile, before.ProviderSessionID, after.ProviderSessionID) {
		return fmt.Errorf("provider session changed from %q to %q", before.ProviderSessionID, after.ProviderSessionID)
	}
	if strings.TrimSpace(before.Cursor.AfterEntryID) == "" || strings.TrimSpace(after.Cursor.AfterEntryID) == "" {
		return fmt.Errorf("continuation cursor is empty")
	}
	if before.Cursor.AfterEntryID == after.Cursor.AfterEntryID {
		return fmt.Errorf("continuation cursor did not advance")
	}
	interruptedPrompt = strings.TrimSpace(interruptedPrompt)
	if interruptedPrompt == "" {
		return fmt.Errorf("interrupted prompt is empty")
	}
	recoveryPrompt = strings.TrimSpace(recoveryPrompt)
	if recoveryPrompt == "" {
		return fmt.Errorf("replacement prompt is empty")
	}
	beforeInterruptedIndex := findEntryTextIndex(before.Entries, 0, interruptedPrompt)
	if beforeInterruptedIndex < 0 {
		return fmt.Errorf("prior normalized history missing interrupted prompt %q", interruptedPrompt)
	}
	stablePrefixEnd := historySubsequenceEnd(after.Entries, continuationComparableEntries(before.Entries[:beforeInterruptedIndex]))
	if stablePrefixEnd < 0 {
		return fmt.Errorf("interrupt recovery transcript does not preserve stable pre-interrupt history")
	}
	afterInterruptedIndex := findEntryTextIndex(after.Entries, stablePrefixEnd, interruptedPrompt)
	searchStart := stablePrefixEnd
	if afterInterruptedIndex >= 0 {
		searchStart = afterInterruptedIndex + 1
	}
	recoveryIndex := findEntryTextIndex(after.Entries, searchStart, recoveryPrompt)
	if recoveryIndex < 0 {
		return fmt.Errorf("interrupt recovery transcript missing replacement prompt %q", recoveryPrompt)
	}
	if recoveryIndex >= len(after.Entries)-1 {
		return fmt.Errorf("interrupt recovery transcript did not record any response after the replacement prompt")
	}
	return nil
}

func continuationComparableEntries(entries []workerpkg.HistoryEntry) []workerpkg.HistoryEntry {
	out := make([]workerpkg.HistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if isTransientContinuationEntry(entry) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isTransientContinuationEntry(entry workerpkg.HistoryEntry) bool {
	if entry.Provenance.RawType != "system" || len(entry.Provenance.Raw) == 0 {
		return false
	}
	var raw struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(entry.Provenance.Raw, &raw); err != nil {
		return false
	}
	return raw.Subtype == "stop_hook_summary"
}

func sameContinuationIdentity(profile workerpkg.Profile, before, after string) bool {
	before = strings.TrimSpace(before)
	after = strings.TrimSpace(after)
	if before == after {
		return true
	}
	return providerResumeSessionKey(string(profile), before) == providerResumeSessionKey(string(profile), after)
}

func requiresStableTranscriptPath(profile workerpkg.Profile) bool {
	return profile != workerpkg.ProfileGeminiTmuxCLI
}

func requiresStableProviderSession(profile workerpkg.Profile) bool {
	return profile != workerpkg.ProfileGeminiTmuxCLI
}

func findEntryTextIndex(entries []workerpkg.HistoryEntry, start int, needle string) int {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return -1
	}
	if start < 0 {
		start = 0
	}
	for idx := start; idx < len(entries); idx++ {
		entry := entries[idx]
		if strings.Contains(entry.Text, needle) {
			return idx
		}
		for _, block := range entry.Blocks {
			if strings.Contains(block.Text, needle) {
				return idx
			}
		}
	}
	return -1
}

func historySubsequenceEnd(after, before []workerpkg.HistoryEntry) int {
	if len(before) == 0 {
		return 0
	}
	match := 0
	for idx, entry := range after {
		if !historyEntriesEquivalent(entry, before[match]) {
			continue
		}
		match++
		if match == len(before) {
			return idx + 1
		}
	}
	return -1
}

func historyEntriesEquivalent(a, b workerpkg.HistoryEntry) bool {
	if strings.TrimSpace(a.ID) != "" && strings.TrimSpace(b.ID) != "" && a.ID == b.ID {
		return true
	}
	return historyEntrySignature(a) == historyEntrySignature(b)
}

func historyEntrySignature(entry workerpkg.HistoryEntry) string {
	parts := []string{
		string(entry.Actor),
		entry.Kind,
		strings.TrimSpace(entry.Text),
	}
	for _, block := range entry.Blocks {
		parts = append(parts,
			string(block.Kind),
			strings.TrimSpace(block.Text),
			strings.TrimSpace(block.ToolUseID),
			strings.TrimSpace(block.Name),
		)
	}
	return strings.Join(parts, "\x1f")
}

func waitForFileText(path string, timeout time.Duration) (string, error) {
	var last string
	found := pollForCondition(timeout, 5*time.Second, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			last = err.Error()
			return false
		}
		last = string(data)
		return strings.TrimSpace(last) != ""
	})
	if !found {
		return last, fmt.Errorf("timed out waiting for file %s", path)
	}
	return strings.TrimSpace(last), nil
}

func waitForLiveFileText(cityDir, sessionName, path string, timeout time.Duration) (string, map[string]string, error) {
	var last string
	var blocked *liveBlockedInteraction
	found := pollForCondition(timeout, 5*time.Second, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			last = err.Error()
		} else {
			last = string(data)
			if strings.TrimSpace(last) != "" {
				return true
			}
		}
		detected, err := detectLiveBlockedInteraction(cityDir, sessionName)
		if err != nil {
			last = strings.TrimSpace(last + "\nTMUX_ERR: " + err.Error())
			return false
		}
		if detected != nil {
			blocked = detected
			return true
		}
		return false
	})
	if blocked != nil {
		return strings.TrimSpace(last), blocked.evidence(), blocked.err()
	}
	if !found {
		return last, nil, fmt.Errorf("timed out waiting for file %s", path)
	}
	return strings.TrimSpace(last), nil, nil
}

func liveFailureResult(profileID workertest.ProfileID, requirement workertest.RequirementCode, detail string, evidence map[string]string) workertest.Result {
	enriched := enrichLiveFailureEvidence(profileID, evidence)
	switch classifyLiveFailure(detail, enriched) {
	case workertest.ResultEnvironmentErr:
		return workertest.EnvironmentError(profileID, requirement, detail).WithEvidence(enriched)
	case workertest.ResultProviderIssue:
		return workertest.ProviderIncident(profileID, requirement, detail).WithEvidence(enriched)
	default:
		return workertest.Fail(profileID, requirement, detail).WithEvidence(enriched)
	}
}

func enrichLiveFailureEvidence(profileID workertest.ProfileID, evidence map[string]string) map[string]string {
	enriched := mergeEvidence(evidence)
	workDir := strings.TrimSpace(enriched["city_dir"])
	if workDir == "" {
		workDir = strings.TrimSpace(enriched["work_dir"])
	}
	if workDir == "" {
		return enriched
	}
	sessionName := firstNonEmpty(
		enriched["running_session_name"],
		enriched["blocked_session_name"],
		enriched["session_name"],
	)
	if paneTail, paneErr := captureTmuxPane(workDir, sessionName, 60); paneErr == nil {
		if strings.TrimSpace(paneTail) != "" {
			enriched["pane_tail"] = paneTail
			if blocked := classifyLivePaneBlocked(paneTail); blocked != nil {
				blocked.SessionName = sessionName
				enriched = mergeEvidence(enriched, blocked.evidence())
			}
		}
	} else if strings.TrimSpace(sessionName) != "" {
		enriched["pane_capture_error"] = paneErr.Error()
	}
	adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
	gcSessionID := strings.TrimSpace(enriched["gc_session_id"])
	if gcSessionID == "" {
		gcSessionID = strings.TrimSpace(enriched["session_key"])
	}
	transcriptPath := strings.TrimSpace(enriched["transcript_path"])
	if transcriptPath == "" {
		transcriptPath = strings.TrimSpace(adapter.DiscoverTranscript(string(profileID), workDir, gcSessionID))
	}
	if transcriptPath == "" {
		return enriched
	}
	enriched["transcript_path"] = transcriptPath
	if tail := readFileTail(transcriptPath, 4096); strings.TrimSpace(tail) != "" {
		enriched["transcript_tail"] = tail
	}
	snapshot, err := adapter.LoadHistory(workerpkg.LoadRequest{
		Provider:       string(profileID),
		TranscriptPath: transcriptPath,
		GCSessionID:    gcSessionID,
	})
	if err != nil || snapshot == nil {
		return enriched
	}
	enriched["logical_conversation_id"] = snapshot.LogicalConversationID
	enriched["provider_session_id"] = snapshot.ProviderSessionID
	if text := historyTailText(snapshot, 6); strings.TrimSpace(text) != "" {
		enriched["normalized_tail"] = text
	}
	return enriched
}

func classifyLiveFailure(detail string, evidence map[string]string) workertest.ResultStatus {
	haystack := strings.ToLower(strings.TrimSpace(detail))
	for _, key := range []string{
		"transcript_tail",
		"normalized_tail",
		"pane_tail",
		"blocked_detail",
		"blocked_kind",
		"proof_contents",
		"output_contents",
		"resume_status",
		"status",
		"session_list",
		"supervisor_logs",
		"nudge_out",
		"restart_start_out",
		"restart_stop_out",
	} {
		if value := strings.TrimSpace(evidence[key]); value != "" {
			haystack += "\n" + strings.ToLower(value)
		}
	}
	if containsAny(haystack,
		"oauth token has expired",
		"please run /login",
		"authentication_error",
		"api error: 401",
		"invalid api key",
		"api key is invalid",
		"unauthorized",
		"not authenticated",
		"authentication failed",
		"login required",
	) {
		return workertest.ResultEnvironmentErr
	}
	if containsAny(haystack,
		"rate limited",
		"rate_limit",
		"too many requests",
		"try again later",
		"temporarily unavailable",
		"service unavailable",
		"overloaded",
	) {
		return workertest.ResultProviderIssue
	}
	return workertest.ResultFail
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

func readFileTail(path string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	return string(data)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func historyTailText(snapshot *workerpkg.HistorySnapshot, limit int) string {
	if snapshot == nil || limit <= 0 || len(snapshot.Entries) == 0 {
		return ""
	}
	start := len(snapshot.Entries) - limit
	if start < 0 {
		start = 0
	}
	var lines []string
	for _, entry := range snapshot.Entries[start:] {
		if text := strings.TrimSpace(entry.Text); text != "" {
			lines = append(lines, text)
		}
		for _, block := range entry.Blocks {
			if text := strings.TrimSpace(block.Text); text != "" {
				lines = append(lines, text)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func mergeEvidence(parts ...map[string]string) map[string]string {
	out := make(map[string]string)
	for _, part := range parts {
		for key, value := range part {
			if strings.TrimSpace(value) == "" {
				continue
			}
			out[key] = value
		}
	}
	return out
}

func runGCWithTimeout(timeout time.Duration, env *helpers.Env, dir string, args ...string) (string, error) {
	gcPath, err := helpers.ResolveGCPath(env)
	if err != nil {
		return "", fmt.Errorf("gc path: %w", err)
	}

	return runExternalWithTimeout(timeout, env, dir, gcPath, args...)
}

func runExternalWithTimeout(timeout time.Duration, env *helpers.Env, dir, name string, args ...string) (string, error) {
	outFile, err := os.CreateTemp("", "gc-worker-command-*.log")
	if err != nil {
		return "", err
	}
	outPath := outFile.Name()
	defer os.Remove(outPath)

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env.List()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		_ = outFile.Close()
		out, _ := os.ReadFile(outPath)
		return string(out), err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	timedOut := false
	timer := time.NewTimer(timeout)
	select {
	case waitErr = <-done:
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
		timedOut = true
		killTimedCommand(cmd)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}

	_ = outFile.Close()
	out, readErr := os.ReadFile(outPath)
	if readErr != nil && len(out) == 0 {
		return "", readErr
	}
	if timedOut {
		return string(out), fmt.Errorf("timed out after %s", timeout)
	}
	return string(out), waitErr
}

func killTimedCommand(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = err
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = err
	}
}

func parseRunningAgents(status string) (int, int, bool) {
	match := agentsRunningPattern.FindStringSubmatch(status)
	if len(match) != 3 {
		return 0, 0, false
	}
	running, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, false
	}
	total, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, 0, false
	}
	return running, total, true
}

func quotedOrderList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		quoted = append(quoted, strconv.Quote(trimmed))
	}
	return strings.Join(quoted, ", ")
}

func busyTurnPrompt(label string, count int, completionMarker string) string {
	if count <= 0 {
		count = 1
	}
	base := fmt.Sprintf(
		"Write exactly %d numbered lines. Each line must begin with %q followed by the line number. Do not use code fences or extra commentary.",
		count,
		label+" line ",
	)
	if completionMarker == "" {
		return base
	}
	return fmt.Sprintf(
		"Write exactly %d numbered lines. Each line must begin with %q followed by the line number. After the numbered lines, write one final line exactly %q and nothing else. Do not use code fences or extra commentary.",
		count,
		label+" line ",
		completionMarker,
	)
}

func isRunTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "timed out after")
}

func parseCreatedBeadID(output string) string {
	match := createdBeadPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func parseCreatedSessionID(output string) string {
	match := createdSessionPattern.FindStringSubmatch(output)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func parseBeadListJSON(t *testing.T, out string) []beadJSON {
	t.Helper()
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if idx := strings.Index(trimmed, "["); idx >= 0 {
		trimmed = trimmed[idx:]
	}
	var beadsOut []beadJSON
	dec := json.NewDecoder(strings.NewReader(trimmed))
	require.NoError(t, dec.Decode(&beadsOut), "unmarshal bead list json")
	return beadsOut
}

func parseSessionListJSON(out string) ([]sessionJSON, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if idx := strings.Index(trimmed, "["); idx >= 0 {
		trimmed = trimmed[idx:]
	}
	var sessions []sessionJSON
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&sessions); err != nil {
		return nil, fmt.Errorf("unmarshal session list json: %w", err)
	}
	return sessions, nil
}

func showBeadJSON(dir, beadID string) (beadJSON, error) {
	out, err := bdCmd(liveEnv, dir, "show", beadID, "--json")
	if err != nil {
		return beadJSON{}, fmt.Errorf("bd show %s: %w\n%s", beadID, err, out)
	}
	var beadsOut []beadJSON
	payload := strings.TrimSpace(out)
	if idx := strings.Index(payload, "["); idx >= 0 {
		payload = payload[idx:]
	}
	dec := json.NewDecoder(strings.NewReader(payload))
	if err := dec.Decode(&beadsOut); err != nil {
		return beadJSON{}, fmt.Errorf("unmarshal bd show %s: %w\n%s", beadID, err, out)
	}
	if len(beadsOut) != 1 {
		return beadJSON{}, fmt.Errorf("bd show %s returned %d records", beadID, len(beadsOut))
	}
	return beadsOut[0], nil
}

type beadJSON struct {
	ID       string         `json:"id"`
	ParentID string         `json:"parent_id"`
	Status   string         `json:"status"`
	Assignee string         `json:"assignee"`
	Title    string         `json:"title"`
	Labels   []string       `json:"labels"`
	Metadata map[string]any `json:"metadata"`
}

type sessionJSON struct {
	Template    string `json:"Template"`
	Provider    string `json:"Provider"`
	ID          string `json:"ID"`
	Alias       string `json:"Alias"`
	State       string `json:"State"`
	SessionName string `json:"SessionName"`
	SessionKey  string `json:"SessionKey"`
	LastActive  string `json:"LastActive"`
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	v, ok := meta[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func bdCmd(env *helpers.Env, dir string, args ...string) (string, error) {
	return bdCmdWithTimeout(10*time.Second, env, dir, args...)
}

func bdCmdWithTimeout(timeout time.Duration, env *helpers.Env, dir string, args ...string) (string, error) {
	bdPath := "bd"
	if path, err := exec.LookPath("bd"); err == nil {
		bdPath = path
	}

	return runExternalWithTimeout(timeout, env, dir, bdPath, args...)
}

func supervisorLogs(cityDir string) string {
	out, err := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "supervisor", "logs")
	if err != nil {
		return strings.TrimSpace(strings.TrimSpace(out) + "\nERR: " + err.Error())
	}
	return strings.TrimSpace(out)
}

func seedClaudeProjectOnboarding(configPath, projectDir string) error {
	projectDir = strings.TrimSpace(projectDir)
	if projectDir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = nil
	}
	var cfg map[string]any
	if len(strings.TrimSpace(string(data))) == 0 {
		cfg = make(map[string]any)
	} else if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	cfg["hasCompletedOnboarding"] = true
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = make(map[string]any)
		cfg["projects"] = projects
	}
	project, _ := projects[projectDir].(map[string]any)
	if project == nil {
		project = map[string]any{
			"allowedTools": []any{},
		}
	}
	project["hasCompletedProjectOnboarding"] = true
	project["hasTrustDialogAccepted"] = true
	if count, ok := project["projectOnboardingSeenCount"].(float64); !ok || count < 1 {
		project["projectOnboardingSeenCount"] = 1
	}
	projects[projectDir] = project
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(encoded, '\n'), 0o600)
}

func seedCodexProjectTrust(configPath, projectDir string) error {
	projectDir = strings.TrimSpace(projectDir)
	if projectDir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	header := fmt.Sprintf("[projects.%s]", strconv.Quote(projectDir))
	if strings.Contains(string(data), header) {
		return nil
	}

	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%s\ntrust_level = %q\n", header, "trusted")
	return os.WriteFile(configPath, []byte(b.String()), 0o600)
}

func seedGeminiFolderTrust(configPath, projectDir string) error {
	projectDir = strings.TrimSpace(projectDir)
	if projectDir == "" {
		return nil
	}
	if realPath, err := filepath.EvalSymlinks(projectDir); err == nil {
		projectDir = realPath
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	trusted := make(map[string]string)
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		if err := json.Unmarshal(data, &trusted); err != nil {
			return err
		}
	}
	trusted[projectDir] = "TRUST_FOLDER"

	encoded, err := json.MarshalIndent(trusted, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, append(encoded, '\n'), 0o600)
}

func tmuxSessionExists(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return false, fmt.Errorf("tmux not found: %w", err)
	}
	cmd := exec.Command(tmuxPath, "has-session", "-t", name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("tmux has-session %q: %w\n%s", name, err, strings.TrimSpace(string(out)))
}

func tmuxSessionExistsOnCitySocket(cityDir, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	socketName := strings.TrimSpace(filepath.Base(cityDir))
	if socketName == "" || socketName == "." || socketName == string(filepath.Separator) {
		return false, fmt.Errorf("derive tmux socket from city dir %q", cityDir)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return false, fmt.Errorf("tmux not found: %w", err)
	}
	cmd := exec.Command(tmuxPath, "-L", socketName, "has-session", "-t", name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("tmux -L %q has-session %q: %w\n%s", socketName, name, err, strings.TrimSpace(string(out)))
}

func tmuxSessionLive(cityDir, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	socketName := strings.TrimSpace(filepath.Base(cityDir))
	if socketName == "" || socketName == "." || socketName == string(filepath.Separator) {
		return false, fmt.Errorf("derive tmux socket from city dir %q", cityDir)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return false, fmt.Errorf("tmux not found: %w", err)
	}
	cmd := exec.Command(tmuxPath, "-L", socketName, "list-panes", "-t", name, "-F", "#{pane_dead}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("tmux -L %q list-panes -t %q: %w\n%s", socketName, name, err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "0" {
			return true, nil
		}
	}
	return false, nil
}

func waitForGeminiProbeStartupReady(cityDir, sessionName string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var (
		lastPane string
		lastErr  error
	)
	for time.Now().Before(deadline) {
		pane, err := captureTmuxPane(cityDir, sessionName, 80)
		if err != nil {
			lastErr = err
		} else {
			lastPane = pane
			if geminiProbePaneReadyForTask(pane) {
				return pane, nil
			}
			lastErr = fmt.Errorf("gemini probe pane not ready for first task")
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("gemini probe pane did not become ready for first task")
	}
	return lastPane, lastErr
}

func geminiProbePaneReadyForTask(pane string) bool {
	lower := strings.ToLower(pane)
	if !strings.Contains(lower, "type your message") {
		return false
	}
	if strings.Contains(lower, "waiting for authentication") {
		return false
	}
	return strings.Contains(lower, "first task") ||
		strings.Contains(lower, "how can i help") ||
		strings.Contains(lower, "ready")
}

func captureTmuxPane(cityDir, name string, lines int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	socketName := strings.TrimSpace(filepath.Base(cityDir))
	if socketName == "" || socketName == "." || socketName == string(filepath.Separator) {
		return "", fmt.Errorf("derive tmux socket from city dir %q", cityDir)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return "", fmt.Errorf("tmux not found: %w", err)
	}
	if lines <= 0 {
		lines = 40
	}
	cmd := exec.Command(tmuxPath, "-L", socketName, "capture-pane", "-p", "-t", name, "-S", fmt.Sprintf("-%d", lines))
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			text := strings.ToLower(strings.TrimSpace(string(out)))
			if strings.Contains(text, "can't find") || strings.Contains(text, "no server") {
				return "", nil
			}
		}
		return "", fmt.Errorf("tmux -L %q capture-pane -t %q: %w\n%s", socketName, name, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func detectLiveBlockedInteraction(cityDir, sessionName string) (*liveBlockedInteraction, error) {
	sessionNames, err := listTmuxSessionsOnCitySocket(cityDir)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(sessionNames)+1)
	if trimmed := strings.TrimSpace(sessionName); trimmed != "" {
		candidates = append(candidates, trimmed)
	}
	candidates = append(candidates, sessionNames...)
	return detectLiveBlockedInteractionForSessions(cityDir, candidates)
}

func isIgnorableTmuxProbeError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no server") ||
		strings.Contains(text, "failed to connect") ||
		strings.Contains(text, "error connecting") ||
		strings.Contains(text, "server exited unexpectedly") ||
		strings.Contains(text, "can't find pane") ||
		strings.Contains(text, "can't find session")
}

func detectLiveBlockedInteractionForSessions(cityDir string, candidates []string) (*liveBlockedInteraction, error) {
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		paneTail, err := captureTmuxPane(cityDir, trimmed, 60)
		if err != nil {
			if isIgnorableTmuxProbeError(err) {
				continue
			}
			return nil, err
		}
		if strings.TrimSpace(paneTail) == "" {
			continue
		}
		blocked := classifyLivePaneBlocked(paneTail)
		if blocked == nil {
			continue
		}
		blocked.SessionName = trimmed
		return blocked, nil
	}
	return nil, nil
}

func classifyLivePaneBlocked(paneTail string) *liveBlockedInteraction {
	if strings.TrimSpace(paneTail) == "" {
		return nil
	}
	haystack := strings.ToLower(paneTail)
	switch {
	case containsAny(haystack,
		"oauth token has expired",
		"please run /login",
		"login required",
		"authentication_error",
		"not authenticated",
	):
		return &liveBlockedInteraction{
			Kind:     "authentication",
			Detail:   "worker is blocked on provider authentication",
			PaneTail: paneTail,
		}
	case strings.Contains(haystack, "choose the text style") &&
		(strings.Contains(haystack, "let's get started") || strings.Contains(paneTail, "Let’s get started")):
		return &liveBlockedInteraction{
			Kind:     "first_run_picker",
			Detail:   "worker is blocked on the provider first-run text-style picker",
			PaneTail: paneTail,
		}
	case containsAny(haystack,
		"quick safety check",
		"trust this folder",
		"do you trust the contents of this directory?",
	):
		return &liveBlockedInteraction{
			Kind:     "workspace_trust",
			Detail:   "worker is blocked on a workspace trust dialog",
			PaneTail: paneTail,
		}
	case strings.Contains(haystack, "bypass permissions mode"):
		return &liveBlockedInteraction{
			Kind:     "bypass_permissions_warning",
			Detail:   "worker is blocked on the bypass-permissions warning dialog",
			PaneTail: paneTail,
		}
	case containsAny(haystack,
		"this command requires approval",
		"approve edits?",
	):
		return &liveBlockedInteraction{
			Kind:     "tool_approval",
			Detail:   "worker is blocked on a tool approval prompt",
			PaneTail: paneTail,
		}
	case containsAny(haystack,
		"usage limit",
		"approaching rate limits",
		"usage limit reached",
		"rate limit",
	):
		return &liveBlockedInteraction{
			Kind:     "rate_limit",
			Detail:   "worker is blocked on a provider rate-limit dialog",
			PaneTail: paneTail,
		}
	default:
		return nil
	}
}

func listTmuxSessionsOnCitySocket(cityDir string) ([]string, error) {
	socketName := strings.TrimSpace(filepath.Base(cityDir))
	if socketName == "" || socketName == "." || socketName == string(filepath.Separator) {
		return nil, fmt.Errorf("derive tmux socket from city dir %q", cityDir)
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("tmux not found: %w", err)
	}
	cmd := exec.Command(tmuxPath, "-L", socketName, "list-sessions", "-F", "#{session_name}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			text := strings.ToLower(strings.TrimSpace(string(out)))
			if strings.Contains(text, "no server") || strings.Contains(text, "failed to connect") {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("tmux -L %q list-sessions: %w\n%s", socketName, err, strings.TrimSpace(string(out)))
	}
	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			sessions = append(sessions, trimmed)
		}
	}
	return sessions, nil
}

func pollForCondition(timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(interval)
	}
	return false
}
