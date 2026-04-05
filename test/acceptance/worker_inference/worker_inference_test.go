//go:build acceptance_c

package workerinference_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	workerpkg "github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/internal/worker/workertest"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var (
	agentsRunningPattern  = regexp.MustCompile(`(?m)^\s*(\d+)/(\d+)\s+agents running\b`)
	createdBeadPattern    = regexp.MustCompile(`(?m)^Created (\S+)\b`)
	createdSessionPattern = regexp.MustCompile(`(?m)^Session (\S+) created\b`)
)

const (
	inferenceProbeTemplate   = "probe"
	inferenceProbePromptPath = "prompts/worker-inference-probe.md"
)

type inferenceRun struct {
	CityDir            string
	WorkBeadID         string
	WorkBead           beadJSON
	SpawnedSessionBead beadJSON
	OutputPath         string
	OutputContents     string
	LastStatus         string
	SessionList        string
	SupervisorLogs     string
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

	outputRel := fmt.Sprintf("worker-inference-%s.txt", liveSetup.Provider)
	outputText := fmt.Sprintf("%s live inference ok", liveSetup.Provider)
	prompt := fmt.Sprintf("Create a file named %s containing exactly %q and nothing else.", outputRel, outputText)

	run, spawnEvidence, taskEvidence, stage, err := runFreshInitSlingWork(t, liveSetup.Provider, prompt, outputRel)
	if err != nil {
		requirement := workertest.RequirementInferenceFreshSpawn
		evidence := spawnEvidence
		if stage == "task" {
			requirement = workertest.RequirementInferenceFreshTask
			evidence = taskEvidence
		}
		reporter.Record(liveFailureResult(profileID, requirement, err.Error(), evidence))
		t.FailNow()
	}
	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshSpawn, "fresh city sling spawned a live worker session").WithEvidence(spawnEvidence))

	taskEvidence["expected_output"] = outputText
	if strings.TrimSpace(run.OutputContents) != outputText {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceFreshTask, "live worker output did not match the requested content", taskEvidence))
		t.FailNow()
	}
	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshTask, "live worker completed a machine-checkable file-writing task").WithEvidence(taskEvidence))

	adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
	transcriptPath, snapshot, transcriptEvidence, err := waitForTranscript(adapter, liveSetup.Profile, run.CityDir, prompt, outputText)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceTranscript, err.Error(), transcriptEvidence))
		t.FailNow()
	}

	transcriptEvidence["transcript_path"] = transcriptPath
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

	run, spawnEvidence, taskEvidence, stage, err := runFreshInitSlingWorkWithSetup(
		t,
		liveSetup.Provider,
		prompt,
		outputRel,
		func(cityDir string) error {
			path := filepath.Join(cityDir, inputRel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte(sourceBody), 0o644)
		},
	)
	if err != nil {
		requirement := workertest.RequirementInferenceFreshSpawn
		evidence := spawnEvidence
		if stage == "task" {
			requirement = workertest.RequirementInferenceWorkspaceTask
			evidence = taskEvidence
		}
		reporter.Record(liveFailureResult(profileID, requirement, err.Error(), evidence))
		t.FailNow()
	}

	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshSpawn, "fresh city sling spawned a live worker session").WithEvidence(spawnEvidence))
	taskEvidence["source_path"] = filepath.Join(run.CityDir, inputRel)
	taskEvidence["expected_output"] = expected
	if strings.TrimSpace(run.OutputContents) != expected {
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

	anchorText := fmt.Sprintf("continuation-anchor-%s-%d", liveSetup.Provider, time.Now().UTC().UnixNano())
	readyRel := fmt.Sprintf("worker-inference-continuation-ready-%s.txt", liveSetup.Provider)
	readyText := "ready"
	firstPrompt := fmt.Sprintf(
		"Create a file named %s containing exactly %q and nothing else. Also remember this exact phrase for a later message: %q. Do not write that remembered phrase to any file right now.",
		readyRel,
		readyText,
		anchorText,
	)

	run, spawnEvidence, taskEvidence, stage, err := runFreshNamedSessionTurn(t, liveSetup.Provider, inferenceProbeTemplate, firstPrompt, readyRel)
	if err != nil {
		evidence := spawnEvidence
		if stage == "task" {
			evidence = taskEvidence
		}
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), evidence))
		t.FailNow()
	}

	adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
	beforeTranscriptPath, beforeSnapshot, beforeEvidence, err := waitForTranscript(adapter, liveSetup.Profile, run.CityDir, firstPrompt, readyText)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), beforeEvidence))
		t.FailNow()
	}

	restartEvidence := map[string]string{
		"city_dir":            run.CityDir,
		"provider":            liveSetup.Provider,
		"session_bead_id":     run.SessionID,
		"session_alias":       run.SessionAlias,
		"session_name":        run.SessionName,
		"session_key":         run.SessionKey,
		"first_transcript":    beforeTranscriptPath,
		"first_entry_count":   strconv.Itoa(len(beforeSnapshot.Entries)),
		"first_logical_conv":  beforeSnapshot.LogicalConversationID,
		"first_provider_sess": beforeSnapshot.ProviderSessionID,
		"anchor_text":         anchorText,
	}

	stopOut, startOut, err := restartLiveCity(run.CityDir, run.SessionName)
	restartEvidence["restart_stop_out"] = strings.TrimSpace(stopOut)
	restartEvidence["restart_start_out"] = strings.TrimSpace(startOut)
	if err != nil {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), restartEvidence))
		t.FailNow()
	}

	runningSession, statusOut, err := waitForSessionRunning(run.CityDir, run.Identity, run.SessionName)
	restartEvidence["resume_status"] = strings.TrimSpace(statusOut)
	if err != nil {
		restartEvidence["supervisor_logs"] = supervisorLogs(run.CityDir)
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), restartEvidence))
		t.FailNow()
	}
	restartEvidence["running_session_id"] = runningSession.ID
	restartEvidence["running_session_alias"] = runningSession.Alias
	restartEvidence["running_session_name"] = runningSession.SessionName
	restartEvidence["running_session_state"] = runningSession.State
	restartEvidence["running_session_key"] = runningSession.SessionKey
	restartEvidence["running_session_last_active"] = runningSession.LastActive
	if strings.TrimSpace(run.SessionKey) != "" && strings.TrimSpace(runningSession.SessionKey) != "" && run.SessionKey != runningSession.SessionKey {
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, fmt.Sprintf(
			"session key changed across restart: %q -> %q",
			run.SessionKey,
			runningSession.SessionKey,
		), restartEvidence))
		t.FailNow()
	}

	recallPrompt := fmt.Sprintf(
		"Without reading files or manually searching history, create a file named %s containing exactly the remembered phrase from our earlier turn and nothing else.",
		fmt.Sprintf("worker-inference-continuation-proof-%s.txt", liveSetup.Provider),
	)
	recallRel := fmt.Sprintf("worker-inference-continuation-proof-%s.txt", liveSetup.Provider)
	nudgeOut, err := runGCWithTimeout(20*time.Second, liveEnv, run.CityDir, "session", "nudge", run.Identity, recallPrompt)
	restartEvidence["nudge_out"] = strings.TrimSpace(nudgeOut)
	if err != nil {
		restartEvidence["supervisor_logs"] = supervisorLogs(run.CityDir)
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, fmt.Sprintf("gc session nudge failed: %v", err), restartEvidence))
		t.FailNow()
	}

	proofPath := filepath.Join(run.CityDir, recallRel)
	proofText, err := waitForFileText(proofPath, 4*time.Minute)
	restartEvidence["proof_path"] = proofPath
	restartEvidence["proof_contents"] = proofText
	if err != nil {
		restartEvidence["supervisor_logs"] = supervisorLogs(run.CityDir)
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), restartEvidence))
		t.FailNow()
	}
	if strings.TrimSpace(proofText) != anchorText {
		restartEvidence["supervisor_logs"] = supervisorLogs(run.CityDir)
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, "continued worker did not reproduce the remembered phrase", restartEvidence))
		t.FailNow()
	}

	afterTranscriptPath, afterSnapshot, continuationEvidence, err := waitForContinuationTranscript(
		adapter,
		liveSetup.Profile,
		run.CityDir,
		beforeTranscriptPath,
		beforeSnapshot,
		recallPrompt,
		anchorText,
	)
	if err != nil {
		merged := mergeEvidence(restartEvidence, continuationEvidence)
		merged["supervisor_logs"] = supervisorLogs(run.CityDir)
		reporter.Record(liveFailureResult(profileID, workertest.RequirementInferenceContinuation, err.Error(), merged))
		t.FailNow()
	}

	merged := mergeEvidence(restartEvidence, continuationEvidence)
	merged["after_transcript"] = afterTranscriptPath
	merged["after_entry_count"] = strconv.Itoa(len(afterSnapshot.Entries))
	merged["after_logical_conv"] = afterSnapshot.LogicalConversationID
	merged["after_provider_sess"] = afterSnapshot.ProviderSessionID
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceContinuation, "restarted live worker resumed the same conversation and recalled prior context").WithEvidence(merged))
}

func runFreshInitSlingWork(t *testing.T, provider, prompt, outputRel string) (inferenceRun, map[string]string, map[string]string, string, error) {
	return runFreshInitSlingWorkWithSetup(t, provider, prompt, outputRel, nil)
}

func runFreshInitSlingWorkWithSetup(t *testing.T, provider, prompt, outputRel string, setupFn func(cityDir string) error) (inferenceRun, map[string]string, map[string]string, string, error) {
	t.Helper()

	c := helpers.NewCity(t, liveEnv)
	initArgs := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		initArgs = append(initArgs, "--provider", provider)
	}
	initArgs = append(initArgs, c.Dir)
	initOut, initErr := runGCWithTimeout(45*time.Second, liveEnv, "", initArgs...)
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
	_, _ = runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	_, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "stop", c.Dir)
	time.Sleep(2 * time.Second)

	startOut, startErr := runGCWithTimeout(45*time.Second, liveEnv, c.Dir, "start", c.Dir)
	if startErr != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("gc start failed: %w", startErr)
	}
	t.Cleanup(func() {
		_, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "stop", c.Dir)
		_, _ = runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	})

	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, c.Dir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("bead store did not become ready after gc start")
	}

	out, err := runGCWithTimeout(20*time.Second, liveEnv, c.Dir, "sling", provider, prompt)
	if err != nil {
		return inferenceRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"sling_out":  strings.TrimSpace(out),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("gc sling failed: %w", err)
	}

	workBeadID := parseCreatedBeadID(out)
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
		spawnedSession    beadJSON
	)

	spawned := pollForCondition(2*time.Minute, 5*time.Second, func() bool {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "status")
		lastStatus = strings.TrimSpace(statusOut)
		if statusErr != nil {
			lastStatus = strings.TrimSpace(statusOut + "\nERR: " + statusErr.Error())
			return false
		}

		sessionsOut, sessionsErr := bdCmd(liveEnv, c.Dir, "list", "--include-infra", "--label", "gc:session", "--json", "--limit=20")
		lastSessionJSON = strings.TrimSpace(sessionsOut)
		if sessionsErr != nil {
			lastSessionJSON = strings.TrimSpace(sessionsOut + "\nERR: " + sessionsErr.Error())
			return false
		}

		sessionBeads := parseBeadListJSON(t, sessionsOut)
		for _, sessionBead := range sessionBeads {
			if metaString(sessionBead.Metadata, "template") != provider {
				continue
			}
			state := metaString(sessionBead.Metadata, "state")
			if state != "creating" && state != "active" && state != "awake" {
				continue
			}
			if metaString(sessionBead.Metadata, "session_name") == "" {
				continue
			}
			spawnedSession = sessionBead
			running, total, ok := parseRunningAgents(statusOut)
			return ok && total > 0 && running > 0
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
	var lastWorkBead beadJSON
	completed := pollForCondition(6*time.Minute, 10*time.Second, func() bool {
		bead, beadErr := showBeadJSON(c.Dir, workBeadID)
		if beadErr == nil {
			lastWorkBead = bead
		}
		data, readErr := os.ReadFile(outputPath)
		if readErr != nil {
			return false
		}
		if strings.TrimSpace(string(data)) == "" {
			return false
		}
		return beadErr == nil && bead.Status == "closed"
	})

	sessionListOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "session", "list")
	supervisorLogsOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "supervisor", "logs")
	outputContents, outputErr := os.ReadFile(outputPath)
	outputDiag := string(outputContents)
	if outputErr != nil {
		outputDiag = outputErr.Error()
	}

	run := inferenceRun{
		CityDir:            c.Dir,
		WorkBeadID:         workBeadID,
		WorkBead:           lastWorkBead,
		SpawnedSessionBead: spawnedSession,
		OutputPath:         outputPath,
		OutputContents:     strings.TrimSpace(string(outputContents)),
		LastStatus:         lastStatus,
		SessionList:        strings.TrimSpace(sessionListOut),
		SupervisorLogs:     strings.TrimSpace(supervisorLogsOut),
	}
	spawnEvidence := map[string]string{
		"city_dir":        c.Dir,
		"provider":        provider,
		"init_out":        strings.TrimSpace(initOut),
		"start_out":       strings.TrimSpace(startOut),
		"work_bead_id":    workBeadID,
		"session_name":    metaString(spawnedSession.Metadata, "session_name"),
		"session_state":   metaString(spawnedSession.Metadata, "state"),
		"session_command": metaString(spawnedSession.Metadata, "command"),
	}
	taskEvidence := map[string]string{
		"city_dir":        c.Dir,
		"provider":        provider,
		"init_out":        strings.TrimSpace(initOut),
		"start_out":       strings.TrimSpace(startOut),
		"work_bead_id":    workBeadID,
		"output_path":     outputPath,
		"output_contents": strings.TrimSpace(outputDiag),
		"session_name":    metaString(spawnedSession.Metadata, "session_name"),
	}

	if !completed {
		taskEvidence["status"] = lastStatus
		taskEvidence["session_list"] = run.SessionList
		taskEvidence["supervisor_logs"] = run.SupervisorLogs
		return run, spawnEvidence, taskEvidence, "task", fmt.Errorf("live %s worker did not complete the routed task within 6m", provider)
	}

	return run, spawnEvidence, taskEvidence, "", nil
}

func runFreshManualSessionTurn(t *testing.T, provider, templateName, alias, prompt, outputRel string) (inferenceSessionRun, map[string]string, map[string]string, string, error) {
	t.Helper()

	c := helpers.NewCity(t, liveEnv)
	initArgs := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		initArgs = append(initArgs, "--provider", provider)
	}
	initArgs = append(initArgs, c.Dir)
	initOut, initErr := runGCWithTimeout(45*time.Second, liveEnv, "", initArgs...)
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
	_, _ = runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	_, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "stop", c.Dir)
	time.Sleep(2 * time.Second)

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

	startOut, startErr := runGCWithTimeout(45*time.Second, liveEnv, c.Dir, "start", c.Dir)
	if startErr != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"session_id":  sessionID,
			"init_out":    strings.TrimSpace(initOut),
			"session_out": strings.TrimSpace(newOut),
			"start_out":   strings.TrimSpace(startOut),
			"output_rel":  outputRel,
		}, nil, "spawn", fmt.Errorf("gc start failed after session create: %w", startErr)
	}
	t.Cleanup(func() {
		_, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "stop", c.Dir)
		_, _ = runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	})

	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, c.Dir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":    c.Dir,
			"provider":    provider,
			"template":    templateName,
			"alias":       alias,
			"session_id":  sessionID,
			"init_out":    strings.TrimSpace(initOut),
			"session_out": strings.TrimSpace(newOut),
			"start_out":   strings.TrimSpace(startOut),
			"output_rel":  outputRel,
		}, nil, "spawn", fmt.Errorf("bead store did not become ready after gc start")
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

	nudgeOut, err := runGCWithTimeout(20*time.Second, liveEnv, c.Dir, "session", "nudge", sessionID, prompt)
	outputPath := filepath.Join(c.Dir, outputRel)
	if err != nil {
		return inferenceSessionRun{
				CityDir:      c.Dir,
				SessionID:    sessionID,
				SessionAlias: alias,
				SessionName:  sessionInfo.SessionName,
				OutputPath:   outputPath,
				LastStatus:   strings.TrimSpace(statusOut),
			}, map[string]string{
				"city_dir":     c.Dir,
				"provider":     provider,
				"template":     templateName,
				"alias":        alias,
				"session_id":   sessionID,
				"session_name": sessionInfo.SessionName,
				"init_out":     strings.TrimSpace(initOut),
				"start_out":    strings.TrimSpace(startOut),
				"session_out":  strings.TrimSpace(newOut),
				"status":       strings.TrimSpace(statusOut),
				"output_rel":   outputRel,
			}, map[string]string{
				"city_dir":     c.Dir,
				"provider":     provider,
				"template":     templateName,
				"alias":        alias,
				"session_id":   sessionID,
				"session_name": sessionInfo.SessionName,
				"nudge_out":    strings.TrimSpace(nudgeOut),
				"output_path":  outputPath,
			}, "task", fmt.Errorf("gc session nudge failed: %w", err)
	}

	var (
		lastStatus     string
		sessionListOut string
		supervisorLogs string
		outputContents string
	)
	completed := pollForCondition(6*time.Minute, 10*time.Second, func() bool {
		statusNow, _ := runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "status")
		lastStatus = strings.TrimSpace(statusNow)
		data, readErr := os.ReadFile(outputPath)
		if readErr != nil {
			return false
		}
		outputContents = strings.TrimSpace(string(data))
		return outputContents != ""
	})

	sessionListOut, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "session", "list")
	supervisorLogs, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "supervisor", "logs")
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
		"city_dir":     c.Dir,
		"provider":     provider,
		"template":     templateName,
		"alias":        alias,
		"session_id":   sessionID,
		"session_name": sessionInfo.SessionName,
		"init_out":     strings.TrimSpace(initOut),
		"start_out":    strings.TrimSpace(startOut),
		"session_out":  strings.TrimSpace(newOut),
	}
	taskEvidence := map[string]string{
		"city_dir":        c.Dir,
		"provider":        provider,
		"template":        templateName,
		"alias":           alias,
		"session_id":      sessionID,
		"session_name":    sessionInfo.SessionName,
		"output_path":     outputPath,
		"output_contents": outputContents,
		"nudge_out":       strings.TrimSpace(nudgeOut),
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

	c := helpers.NewCity(t, liveEnv)
	initArgs := []string{"init", "--skip-provider-readiness"}
	if provider != "" {
		initArgs = append(initArgs, "--provider", provider)
	}
	initArgs = append(initArgs, c.Dir)
	initOut, initErr := runGCWithTimeout(45*time.Second, liveEnv, "", initArgs...)
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
	_, _ = runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	_, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "stop", c.Dir)
	time.Sleep(2 * time.Second)

	startOut, startErr := runGCWithTimeout(45*time.Second, liveEnv, c.Dir, "start", c.Dir)
	if startErr != nil {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("gc start failed: %w", startErr)
	}
	t.Cleanup(func() {
		_, _ = runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "stop", c.Dir)
		_, _ = runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	})

	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, c.Dir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		return inferenceSessionRun{}, map[string]string{
			"city_dir":   c.Dir,
			"provider":   provider,
			"identity":   identity,
			"init_out":   strings.TrimSpace(initOut),
			"start_out":  strings.TrimSpace(startOut),
			"output_rel": outputRel,
		}, nil, "spawn", fmt.Errorf("bead store did not become ready after gc start")
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

	nudgeOut, err := runGCWithTimeout(20*time.Second, liveEnv, c.Dir, "session", "nudge", identity, prompt)
	outputPath := filepath.Join(c.Dir, outputRel)
	if err != nil {
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
				"city_dir":     c.Dir,
				"provider":     provider,
				"identity":     identity,
				"session_id":   sessionInfo.ID,
				"session_name": sessionInfo.SessionName,
				"session_key":  sessionInfo.SessionKey,
				"init_out":     strings.TrimSpace(initOut),
				"start_out":    strings.TrimSpace(startOut),
				"status":       strings.TrimSpace(statusOut),
				"output_rel":   outputRel,
			}, map[string]string{
				"city_dir":     c.Dir,
				"provider":     provider,
				"identity":     identity,
				"session_id":   sessionInfo.ID,
				"session_name": sessionInfo.SessionName,
				"session_key":  sessionInfo.SessionKey,
				"nudge_out":    strings.TrimSpace(nudgeOut),
				"output_path":  outputPath,
			}, "task", fmt.Errorf("gc session nudge failed: %w", err)
	}

	var (
		lastStatus     string
		sessionListOut string
		supervisorLogs string
		outputContents string
	)
	completed := pollForCondition(6*time.Minute, 10*time.Second, func() bool {
		statusNow, _ := runGCWithTimeout(10*time.Second, liveEnv, c.Dir, "status")
		lastStatus = strings.TrimSpace(statusNow)
		data, readErr := os.ReadFile(outputPath)
		if readErr != nil {
			return false
		}
		outputContents = strings.TrimSpace(string(data))
		return outputContents != ""
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
		"city_dir":      c.Dir,
		"provider":      provider,
		"identity":      identity,
		"session_id":    sessionInfo.ID,
		"session_alias": sessionInfo.Alias,
		"session_name":  sessionInfo.SessionName,
		"session_key":   sessionInfo.SessionKey,
		"init_out":      strings.TrimSpace(initOut),
		"start_out":     strings.TrimSpace(startOut),
	}
	taskEvidence := map[string]string{
		"city_dir":        c.Dir,
		"provider":        provider,
		"identity":        identity,
		"session_id":      sessionInfo.ID,
		"session_alias":   sessionInfo.Alias,
		"session_name":    sessionInfo.SessionName,
		"session_key":     sessionInfo.SessionKey,
		"output_path":     outputPath,
		"output_contents": outputContents,
		"nudge_out":       strings.TrimSpace(nudgeOut),
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
	if liveSetup.Profile != workerpkg.ProfileClaudeTmuxCLI {
		return nil
	}
	gcHome := strings.TrimSpace(liveEnv.Get("GC_HOME"))
	if gcHome == "" {
		return fmt.Errorf("GC_HOME is empty")
	}
	for _, path := range []string{
		filepath.Join(gcHome, ".claude.json"),
		filepath.Join(gcHome, ".claude", ".claude.json"),
	} {
		if err := seedClaudeProjectOnboarding(path, cityDir); err != nil {
			return err
		}
	}
	return nil
}

func installInferenceProbeAgent(cityDir string) error {
	promptPath := filepath.Join(cityDir, inferenceProbePromptPath)
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		return err
	}
	prompt := strings.TrimSpace(`
You are a worker-inference probe session for Gas City tests.

Follow the user's requests directly.
Use the workspace tools when needed.
When a later message asks you to recall prior turn context, use conversation memory rather than searching files or external history unless the user explicitly asks for that.
`)
	if err := os.WriteFile(promptPath, []byte(prompt+"\n"), 0o644); err != nil {
		return err
	}

	cityPath := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(cityPath)
	if err != nil {
		return err
	}
	var additions []string
	if !strings.Contains(string(data), "\nname = \""+inferenceProbeTemplate+"\"") {
		additions = append(additions, fmt.Sprintf(`

[[agent]]
name = %q
prompt_template = %q
max_active_sessions = 1
`, inferenceProbeTemplate, inferenceProbePromptPath))
	}
	if !strings.Contains(string(data), "\n[[named_session]]\ntemplate = \""+inferenceProbeTemplate+"\"") {
		additions = append(additions, fmt.Sprintf(`

[[named_session]]
template = %q
mode = "always"
`, inferenceProbeTemplate))
	}
	if len(additions) == 0 {
		return nil
	}
	return os.WriteFile(cityPath, append(data, []byte(strings.Join(additions, ""))...), 0o644)
}

func restartLiveCity(cityDir, expectedSessionName string) (string, string, error) {
	stopOut, stopErr := runGCWithTimeout(20*time.Second, liveEnv, cityDir, "stop", cityDir)
	if stopErr != nil {
		return stopOut, "", fmt.Errorf("gc stop failed before restart: %w", stopErr)
	}
	supervisorStopOut, supervisorStopErr := runGCWithTimeout(10*time.Second, liveEnv, "", "supervisor", "stop")
	stopOut = strings.TrimSpace(strings.TrimSpace(stopOut) + "\n" + strings.TrimSpace(supervisorStopOut))
	if supervisorStopErr != nil {
		return stopOut, "", fmt.Errorf("gc supervisor stop failed before restart: %w", supervisorStopErr)
	}
	if expectedSessionName != "" {
		running, err := tmuxSessionExists(expectedSessionName)
		if err != nil {
			return stopOut, "", err
		}
		if running {
			return stopOut, "", fmt.Errorf("tmux session %q still running after gc stop", expectedSessionName)
		}
	}
	time.Sleep(2 * time.Second)

	startOut, err := runGCWithTimeout(45*time.Second, liveEnv, cityDir, "start", cityDir)
	if err != nil {
		return stopOut, startOut, fmt.Errorf("gc start failed after restart: %w", err)
	}
	beadReady := pollForCondition(60*time.Second, 2*time.Second, func() bool {
		_, err := bdCmd(liveEnv, cityDir, "list", "--json", "--limit=1")
		return err == nil
	})
	if !beadReady {
		return stopOut, startOut, fmt.Errorf("bead store did not become ready after restart")
	}
	return stopOut, startOut, nil
}

func waitForSessionRunning(cityDir, identity, expectedSessionName string) (sessionJSON, string, error) {
	var (
		lastStatus   string
		lastSessions string
		liveSession  sessionJSON
	)
	ready := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "status")
		lastStatus = strings.TrimSpace(statusOut)
		if statusErr != nil {
			lastStatus = strings.TrimSpace(statusOut + "\nERR: " + statusErr.Error())
			return false
		}
		sessionsOut, sessionsErr := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "session", "list", "--json")
		lastSessions = strings.TrimSpace(sessionsOut)
		if sessionsErr != nil {
			lastSessions = strings.TrimSpace(sessionsOut + "\nERR: " + sessionsErr.Error())
			return false
		}
		sessions, err := parseSessionListJSON(sessionsOut)
		if err != nil {
			lastSessions = strings.TrimSpace(sessionsOut + "\nERR: " + err.Error())
			return false
		}
		liveSession = sessionJSON{}
		for _, session := range sessions {
			if session.ID != identity && session.Alias != identity && session.SessionName != identity {
				continue
			}
			liveSession = session
			break
		}
		if liveSession.ID == "" {
			return false
		}
		if expectedSessionName != "" && liveSession.SessionName != expectedSessionName {
			return false
		}
		if liveSession.State != "active" && liveSession.State != "awake" && liveSession.State != "asleep" {
			return false
		}
		if strings.TrimSpace(liveSession.SessionName) == "" {
			return false
		}
		return true
	})
	if !ready {
		diag := strings.TrimSpace(lastStatus)
		if strings.TrimSpace(lastSessions) != "" {
			diag = strings.TrimSpace(diag + "\nSESSIONS:\n" + lastSessions)
		}
		return liveSession, diag, fmt.Errorf("session %s did not reach a running state within the timeout", identity)
	}
	diag := strings.TrimSpace(lastStatus)
	if strings.TrimSpace(lastSessions) != "" {
		diag = strings.TrimSpace(diag + "\nSESSIONS:\n" + lastSessions)
	}
	return liveSession, diag, nil
}

func waitForTranscript(adapter workerpkg.SessionLogAdapter, profile workerpkg.Profile, workDir, prompt, outputText string) (string, *workerpkg.HistorySnapshot, map[string]string, error) {
	evidence := map[string]string{
		"work_dir": workDir,
		"profile":  string(profile),
	}
	var (
		transcriptPath string
		snapshot       *workerpkg.HistorySnapshot
		lastErr        error
	)
	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		transcriptPath = adapter.DiscoverTranscript(string(profile), workDir, "")
		if strings.TrimSpace(transcriptPath) == "" {
			lastErr = fmt.Errorf("no transcript discovered for %s under %s", profile, workDir)
			return false
		}
		snapshot, lastErr = adapter.LoadHistory(workerpkg.LoadRequest{
			Provider:       string(profile),
			TranscriptPath: transcriptPath,
		})
		if lastErr != nil {
			return false
		}
		if len(snapshot.Entries) == 0 {
			lastErr = fmt.Errorf("normalized transcript for %s is empty", profile)
			return false
		}
		return historyContains(snapshot, prompt) || historyContains(snapshot, outputText)
	})
	evidence["transcript_path"] = transcriptPath
	if found {
		return transcriptPath, snapshot, evidence, nil
	}
	if lastErr != nil {
		return transcriptPath, snapshot, evidence, lastErr
	}
	return transcriptPath, snapshot, evidence, fmt.Errorf("live transcript for %s did not contain the expected task evidence", profile)
}

func waitForContinuationTranscript(
	adapter workerpkg.SessionLogAdapter,
	profile workerpkg.Profile,
	workDir string,
	beforeTranscriptPath string,
	beforeSnapshot *workerpkg.HistorySnapshot,
	recallPrompt string,
	recallResponse string,
) (string, *workerpkg.HistorySnapshot, map[string]string, error) {
	evidence := map[string]string{
		"work_dir":             workDir,
		"profile":              string(profile),
		"before_transcript":    beforeTranscriptPath,
		"before_entry_count":   strconv.Itoa(len(beforeSnapshot.Entries)),
		"before_logical_conv":  beforeSnapshot.LogicalConversationID,
		"before_provider_sess": beforeSnapshot.ProviderSessionID,
	}
	var (
		transcriptPath string
		snapshot       *workerpkg.HistorySnapshot
		lastErr        error
	)

	found := pollForCondition(90*time.Second, 5*time.Second, func() bool {
		transcriptPath = adapter.DiscoverTranscript(string(profile), workDir, "")
		if strings.TrimSpace(transcriptPath) == "" {
			lastErr = fmt.Errorf("no transcript discovered for %s under %s after restart", profile, workDir)
			return false
		}
		snapshot, lastErr = adapter.LoadHistory(workerpkg.LoadRequest{
			Provider:       string(profile),
			TranscriptPath: transcriptPath,
		})
		if lastErr != nil || snapshot == nil {
			return false
		}
		lastErr = continuationSnapshotError(beforeTranscriptPath, beforeSnapshot, transcriptPath, snapshot, recallPrompt, recallResponse)
		return lastErr == nil
	})

	evidence["after_transcript"] = transcriptPath
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

func continuationSnapshotError(
	beforeTranscriptPath string,
	before *workerpkg.HistorySnapshot,
	afterTranscriptPath string,
	after *workerpkg.HistorySnapshot,
	recallPrompt string,
	recallResponse string,
) error {
	if before == nil || after == nil {
		return fmt.Errorf("missing normalized history snapshot")
	}
	if beforeTranscriptPath != afterTranscriptPath {
		return fmt.Errorf("transcript path changed from %q to %q", beforeTranscriptPath, afterTranscriptPath)
	}
	if strings.TrimSpace(before.LogicalConversationID) == "" || strings.TrimSpace(after.LogicalConversationID) == "" {
		return fmt.Errorf("logical conversation identity is empty")
	}
	if before.LogicalConversationID != after.LogicalConversationID {
		return fmt.Errorf("logical conversation changed from %q to %q", before.LogicalConversationID, after.LogicalConversationID)
	}
	if before.ProviderSessionID != "" && after.ProviderSessionID != "" && before.ProviderSessionID != after.ProviderSessionID {
		return fmt.Errorf("provider session changed from %q to %q", before.ProviderSessionID, after.ProviderSessionID)
	}
	if strings.TrimSpace(before.Cursor.AfterEntryID) == "" || strings.TrimSpace(after.Cursor.AfterEntryID) == "" {
		return fmt.Errorf("continuation cursor is empty")
	}
	if before.Cursor.AfterEntryID == after.Cursor.AfterEntryID {
		return fmt.Errorf("continuation cursor did not advance")
	}
	historyEnd := historySubsequenceEnd(after.Entries, before.Entries)
	if historyEnd < 0 {
		return fmt.Errorf("continued transcript does not preserve prior normalized history")
	}
	promptIndex := findEntryTextIndex(after.Entries, historyEnd, recallPrompt)
	if promptIndex < 0 {
		return fmt.Errorf("continued transcript missing recall prompt %q", recallPrompt)
	}
	responseIndex := findEntryTextIndex(after.Entries, promptIndex+1, recallResponse)
	if responseIndex < 0 {
		return fmt.Errorf("continued transcript missing recalled phrase %q after restart", recallResponse)
	}
	return nil
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
	adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
	transcriptPath := strings.TrimSpace(enriched["transcript_path"])
	if transcriptPath == "" {
		transcriptPath = strings.TrimSpace(adapter.DiscoverTranscript(string(profileID), workDir, ""))
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

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, gcPath, args...)
	cmd.Dir = dir
	cmd.Env = env.List()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("timed out after %s", timeout)
	}
	return string(out), err
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

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bdPath, args...)
	cmd.Dir = dir
	cmd.Env = env.List()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("timed out after %s", timeout)
	}
	return string(out), err
}

func supervisorLogs(cityDir string) string {
	out, err := runGCWithTimeout(10*time.Second, liveEnv, cityDir, "supervisor", "logs")
	if err != nil {
		return strings.TrimSpace(strings.TrimSpace(out) + "\nERR: " + err.Error())
	}
	return strings.TrimSpace(out)
}

func seedClaudeProjectOnboarding(configPath, projectDir string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
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
