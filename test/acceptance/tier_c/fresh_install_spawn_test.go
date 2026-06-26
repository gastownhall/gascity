//go:build acceptance_c

package tierc_test

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

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var (
	agentsRunningPattern = regexp.MustCompile(`(?m)^\s*(\d+)/(\d+)\s+agents running\b`)
	createdBeadPattern   = regexp.MustCompile(`(?m)^Created (\S+)\b`)
)

type freshInstallSlingResult struct {
	CityDir            string
	WorkBeadID         string
	WorkBead           beadJSON
	SpawnedSessionBead beadJSON
	OutputPath         string
	OutputContents     string
}

// TestFreshInit_SlingSpawnsDefaultPoolWorker covers the first-run UX from
// issue #286: a brand-new city created with gc init should be able to route
// work to the default claude pool and spawn at least one running worker.
//
// This stays in Tier C because it exercises the real provider-backed startup
// path rather than a fake runtime.
func TestFreshInit_SlingSpawnsDefaultPoolWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("Tier C: skipping in short mode")
	}

	_ = runFreshInitSlingClaudeWork(t, "Write the current time to time.txt", "time.txt")
}

// TestFreshInit_SlingClaudeUsesUnrestrictedPermissionMode covers the root
// cause from issue #278: a freshly initialized claude worker should launch
// with unrestricted permissions so autonomous bash-heavy work does not block
// on permission prompts.
//
// This remains Tier C because the assertion is made on the real spawned
// session bead after going through the full provider-backed fresh-install path.
func TestFreshInit_ClaudeUnrestricted(t *testing.T) {
	if testing.Short() {
		t.Skip("Tier C: skipping in short mode")
	}

	result := runFreshInitSlingClaudeWork(t, "Write the current time to permission-check.txt", "permission-check.txt")
	spawnedSessionBead, err := showBeadJSON(result.CityDir, result.SpawnedSessionBead.ID)
	require.NoError(t, err, "refresh spawned session bead %s", result.SpawnedSessionBead.ID)
	command := metaString(spawnedSessionBead.Metadata, "command")
	require.NotEmpty(t, command, "spawned worker should persist the resolved launch command")
	require.Contains(t, command, "--dangerously-skip-permissions", "fresh claude worker should launch unrestricted")
	require.NotContains(t, command, "--permission-mode auto-edit", "fresh claude worker should not launch in auto-edit mode")
}

func runFreshInitSlingClaudeWork(t *testing.T, prompt, outputRel string) freshInstallSlingResult {
	t.Helper()

	c := helpers.NewCity(t, testEnvC)
	c.Init("claude")
	applyTierCAcceptanceConfig(t, c)
	// The built-in maintenance dog pool is auto-included; this fixture needs
	// a generic claude pool target for mol-do-work.
	configureFreshInitClaudePool(t, c)

	initialSessionBeadsOut, err := bdCmd(testEnvC, c.Dir, "list", "--include-infra", "--label", "gc:session", "--json", "--limit=20")
	require.NoError(t, err, "bd list session beads before sling: %s", initialSessionBeadsOut)
	initialSessionBeads := parseBeadListJSON(t, initialSessionBeadsOut)
	for _, bead := range initialSessionBeads {
		if metaString(bead.Metadata, "template") == "claude" {
			require.NotEqual(t, "true", metaString(bead.Metadata, "pool_managed"), "fresh city should not start with a claude pool worker bead: %+v", bead)
		}
	}

	out, err := runGCWithTimeout(20*time.Second, testEnvC, c.Dir,
		"sling", "claude", prompt)
	require.NoError(t, err, "gc sling: %s", out)
	t.Logf("Slung work: %s", strings.TrimSpace(out))

	workBeadID := parseCreatedBeadID(out)
	require.NotEmpty(t, workBeadID, "gc sling output should include created bead id:\n%s", out)

	workBead, err := showBeadJSON(c.Dir, workBeadID)
	require.NoError(t, err, "bd show work bead %s", workBeadID)
	require.Equal(t, "open", workBead.Status, "slung work bead should start open")
	if routedTo := metaString(workBead.Metadata, "gc.routed_to"); routedTo != "" {
		require.Equal(t, "claude", routedTo, "when present, gc.routed_to should target claude")
	}

	var lastStatus string
	var lastSessionsOut string
	var spawnedSessionBead beadJSON
	spawned := pollForCondition(t, 90*time.Second, 5*time.Second, func() bool {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "status")
		lastStatus = statusOut
		if statusErr != nil {
			lastStatus = strings.TrimSpace(statusOut + "\nERR: " + statusErr.Error())
			return false
		}

		sessionsOut, sessionsErr := bdCmd(testEnvC, c.Dir, "list", "--include-infra", "--label", "gc:session", "--json", "--limit=20")
		lastSessionsOut = sessionsOut
		if sessionsErr != nil {
			lastSessionsOut = strings.TrimSpace(sessionsOut + "\nERR: " + sessionsErr.Error())
			return false
		}
		sessionBeads := parseBeadListJSON(t, sessionsOut)
		sessionListOut, sessionListErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "list")
		for _, sessionBead := range sessionBeads {
			if metaString(sessionBead.Metadata, "template") != "claude" {
				continue
			}
			state := metaString(sessionBead.Metadata, "state")
			if state != "creating" && state != "active" && state != "awake" {
				continue
			}
			sessionName := metaString(sessionBead.Metadata, "session_name")
			if sessionName == "" {
				continue
			}
			spawnedSessionBead = sessionBead
			if state == "active" || state == "awake" {
				return true
			}
			if sessionListErr != nil {
				return false
			}
			return sessionListShowsActive(sessionListOut, sessionBead.ID, sessionName)
		}
		return false
	})

	if !spawned {
		sessionOut, sessionErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "list")
		if sessionErr != nil {
			sessionOut = strings.TrimSpace(sessionOut + "\nERR: " + sessionErr.Error())
		}
		sessionLogsOut := ""
		sessionPeekOut := ""
		if sessionName := metaString(spawnedSessionBead.Metadata, "session_name"); sessionName != "" {
			sessionLogsOut, sessionErr = runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "logs", sessionName, "--tail", "0")
			if sessionErr != nil {
				sessionLogsOut = strings.TrimSpace(sessionLogsOut + "\nERR: " + sessionErr.Error())
			}
			sessionPeekOut, sessionErr = runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "peek", sessionName, "--lines", "200")
			if sessionErr != nil {
				sessionPeekOut = strings.TrimSpace(sessionPeekOut + "\nERR: " + sessionErr.Error())
			}
		}
		supervisorOut, supervisorErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "supervisor", "logs")
		if supervisorErr != nil {
			supervisorOut = strings.TrimSpace(supervisorOut + "\nERR: " + supervisorErr.Error())
		}

		t.Fatalf("fresh gc init city never spawned a running claude pool worker after gc sling within 90s\nlast status:\n%s\nlast session json:\n%s\nsessions:\n%s\nsession logs:\n%s\nsession peek:\n%s\nsupervisor logs:\n%s",
			lastStatus, lastSessionsOut, sessionOut, sessionLogsOut, sessionPeekOut, supervisorOut)
	}

	if poolManaged := metaString(spawnedSessionBead.Metadata, "pool_managed"); poolManaged != "" {
		require.Equal(t, "true", poolManaged, "spawned worker should be pool managed when metadata is present")
	}
	if template := metaString(spawnedSessionBead.Metadata, "template"); template != "" {
		require.Equal(t, "claude", template, "spawned worker should use the claude template when metadata is present")
	}
	sessionName := metaString(spawnedSessionBead.Metadata, "session_name")
	require.NotEmpty(t, sessionName, "spawned worker should record session_name metadata")
	require.True(t, strings.HasPrefix(sessionName, "claude-"), "spawned worker should use a claude-* session name, got %q", sessionName)

	outputPath := filepath.Join(c.Dir, outputRel)
	var lastWorkBead beadJSON
	completed := pollForCondition(t, 4*time.Minute, 10*time.Second, func() bool {
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

	sessionOut, sessionErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "list")
	if sessionErr != nil {
		sessionOut = strings.TrimSpace(sessionOut + "\nERR: " + sessionErr.Error())
	}
	sessionLogsOut, sessionLogsErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "logs", sessionName, "--tail", "0")
	if sessionLogsErr != nil {
		sessionLogsOut = strings.TrimSpace(sessionLogsOut + "\nERR: " + sessionLogsErr.Error())
	}
	sessionPeekOut, sessionPeekErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "peek", sessionName, "--lines", "200")
	if sessionPeekErr != nil {
		sessionPeekOut = strings.TrimSpace(sessionPeekOut + "\nERR: " + sessionPeekErr.Error())
	}
	supervisorOut, supervisorErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "supervisor", "logs")
	if supervisorErr != nil {
		supervisorOut = strings.TrimSpace(supervisorOut + "\nERR: " + supervisorErr.Error())
	}
	outputContents, outputErr := os.ReadFile(outputPath)
	outputDiag := string(outputContents)
	if outputErr != nil {
		outputDiag = outputErr.Error()
	}

	if !completed {
		t.Fatalf("fresh gc init city spawned a claude worker but did not complete routed work within 4m\nwork bead:\n%+v\nsession bead:\n%+v\noutput file (%s):\n%s\nstatus:\n%s\nsessions:\n%s\nsession logs:\n%s\nsession peek:\n%s\nsupervisor logs:\n%s",
			lastWorkBead, spawnedSessionBead, outputRel, outputDiag, lastStatus, sessionOut, sessionLogsOut, sessionPeekOut, supervisorOut)
	}

	return freshInstallSlingResult{
		CityDir:            c.Dir,
		WorkBeadID:         workBeadID,
		WorkBead:           lastWorkBead,
		SpawnedSessionBead: spawnedSessionBead,
		OutputPath:         outputPath,
		OutputContents:     strings.TrimSpace(string(outputContents)),
	}
}

func configureFreshInitClaudePool(t *testing.T, c *helpers.City) {
	t.Helper()
	c.WriteV2AgentDir("claude",
		`provider = "claude"`,
		`default_sling_formula = "mol-do-work"`,
		`min_active_sessions = 0`,
		`max_active_sessions = 1`,
	)
	promptPath := filepath.Join(helpers.FindModuleRoot(), "internal", "bootstrap", "packs", "core", "assets", "prompts", "pool-worker.md")
	prompt, err := os.ReadFile(promptPath)
	require.NoError(t, err, "read canonical pool-worker prompt")
	prompt = append(prompt, []byte("\n## Acceptance Fixture\n\nFor file-writing tasks in this acceptance test, create or update the requested file in the city directory before closing the work bead.\n")...)
	err = os.WriteFile(filepath.Join(c.Dir, "agents", "claude", "prompt.template.md"), []byte(prompt), 0o644)
	require.NoError(t, err, "write claude test prompt")
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

func TestSessionListShowsActiveAcceptsLiveStateDuringMetadataLag(t *testing.T) {
	output := `2026/06/13 22:51:49 WARN native_store_unavailable
ID           TEMPLATE  STATE   REASON          TARGET              TITLE   AGE  LAST ACTIVE  LAST NUDGE
a9-wisp-0my  claude    active  session,config  claude-a9-wisp-0my  claude  19s  0s ago       -
a9-wisp-q0w  claude    creating create,config  s-a9-wisp-q0w       claude  27s  -            -
`

	if !sessionListShowsActive(output, "a9-wisp-0my", "claude-a9-wisp-0my") {
		t.Fatal("expected active live session row to satisfy spawned-worker check")
	}
	if sessionListShowsActive(output, "a9-wisp-q0w", "s-a9-wisp-q0w") {
		t.Fatal("creating live session row must not satisfy spawned-worker check")
	}
	if sessionListShowsActive(output, "missing", "missing") {
		t.Fatal("unrelated session must not satisfy spawned-worker check")
	}
}

func sessionListShowsActive(output, beadID, sessionName string) bool {
	beadID = strings.TrimSpace(beadID)
	sessionName = strings.TrimSpace(sessionName)
	if beadID == "" && sessionName == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		state := fields[2]
		if state != "active" && state != "awake" {
			continue
		}
		if beadID != "" && fields[0] == beadID {
			return true
		}
		if sessionName == "" {
			continue
		}
		for _, field := range fields[3:] {
			if field == sessionName {
				return true
			}
		}
	}
	return false
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
	out, err := bdCmd(testEnvC, dir, "show", beadID, "--json")
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
