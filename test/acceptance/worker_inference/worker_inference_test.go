//go:build acceptance_c

package workerinference_test

import (
	"context"
	"encoding/json"
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
	agentsRunningPattern = regexp.MustCompile(`(?m)^\s*(\d+)/(\d+)\s+agents running\b`)
	createdBeadPattern   = regexp.MustCompile(`(?m)^Created (\S+)\b`)
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
		reporter.Record(workertest.Fail(profileID, requirement, err.Error()).WithEvidence(evidence))
		t.FailNow()
	}
	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshSpawn, "fresh city sling spawned a live worker session").WithEvidence(spawnEvidence))

	taskEvidence["expected_output"] = outputText
	if strings.TrimSpace(run.OutputContents) != outputText {
		reporter.Record(workertest.Fail(profileID, workertest.RequirementInferenceFreshTask, "live worker output did not match the requested content").WithEvidence(taskEvidence))
		t.FailNow()
	}
	reporter.Record(workertest.Pass(profileID, workertest.RequirementInferenceFreshTask, "live worker completed a machine-checkable file-writing task").WithEvidence(taskEvidence))

	adapter := workerpkg.SessionLogAdapter{SearchPaths: liveSetup.SearchPaths}
	transcriptPath, snapshot, transcriptEvidence, err := waitForTranscript(adapter, liveSetup.Profile, run.CityDir, prompt, outputText)
	if err != nil {
		reporter.Record(workertest.Fail(profileID, workertest.RequirementInferenceTranscript, err.Error()).WithEvidence(transcriptEvidence))
		t.FailNow()
	}

	transcriptEvidence["transcript_path"] = transcriptPath
	transcriptEvidence["entry_count"] = strconv.Itoa(len(snapshot.Entries))
	transcriptEvidence["tail_activity"] = string(snapshot.TailState.Activity)
	transcriptEvidence["logical_conversation_id"] = snapshot.LogicalConversationID
	reporter.Require(t, workertest.Pass(profileID, workertest.RequirementInferenceTranscript, "live transcript was discovered and normalized after the completed task").WithEvidence(transcriptEvidence))
}

func runFreshInitSlingWork(t *testing.T, provider, prompt, outputRel string) (inferenceRun, map[string]string, map[string]string, string, error) {
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
