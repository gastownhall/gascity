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

	c := helpers.NewCity(t, testEnvC)
	c.Init("claude")

	initialSessionBeadsOut, err := bdCmd(testEnvC, c.Dir, "list", "--include-infra", "--label", "gc:session", "--json", "--limit=20")
	require.NoError(t, err, "bd list session beads before sling: %s", initialSessionBeadsOut)
	initialSessionBeads := parseBeadListJSON(t, initialSessionBeadsOut)
	for _, bead := range initialSessionBeads {
		if metaString(bead.Metadata, "template") == "claude" {
			require.NotEqual(t, "true", metaString(bead.Metadata, "pool_managed"), "fresh city should not start with a claude pool worker bead: %+v", bead)
		}
	}

	out, err := runGCWithTimeout(20*time.Second, testEnvC, c.Dir,
		"sling", "claude", "Write the current time to time.txt")
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
		for _, sessionBead := range sessionBeads {
			if metaString(sessionBead.Metadata, "template") != "claude" {
				continue
			}
			state := metaString(sessionBead.Metadata, "state")
			if state != "creating" && state != "active" && state != "awake" {
				continue
			}
			if metaString(sessionBead.Metadata, "session_name") == "" {
				continue
			}
			spawnedSessionBead = sessionBead
			running, total, ok := parseRunningAgents(statusOut)
			return ok && total > 0 && running > 0
		}
		return false
	})

	if !spawned {
		sessionOut, sessionErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "list")
		if sessionErr != nil {
			sessionOut = strings.TrimSpace(sessionOut + "\nERR: " + sessionErr.Error())
		}
		supervisorOut, supervisorErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "supervisor", "logs")
		if supervisorErr != nil {
			supervisorOut = strings.TrimSpace(supervisorOut + "\nERR: " + supervisorErr.Error())
		}

		t.Fatalf("fresh gc init city never spawned a running claude pool worker after gc sling within 90s\nlast status:\n%s\nlast session json:\n%s\nsessions:\n%s\nsupervisor logs:\n%s",
			lastStatus, lastSessionsOut, sessionOut, supervisorOut)
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

	timeFile := filepath.Join(c.Dir, "time.txt")
	var lastWorkBead beadJSON
	completed := pollForCondition(t, 4*time.Minute, 10*time.Second, func() bool {
		bead, beadErr := showBeadJSON(c.Dir, workBeadID)
		if beadErr == nil {
			lastWorkBead = bead
		}
		data, readErr := os.ReadFile(timeFile)
		if readErr != nil {
			return false
		}
		if strings.TrimSpace(string(data)) == "" {
			return false
		}
		return beadErr == nil && bead.Status == "closed"
	})

	if completed {
		return
	}

	sessionOut, sessionErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "session", "list")
	if sessionErr != nil {
		sessionOut = strings.TrimSpace(sessionOut + "\nERR: " + sessionErr.Error())
	}
	supervisorOut, supervisorErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "supervisor", "logs")
	if supervisorErr != nil {
		supervisorOut = strings.TrimSpace(supervisorOut + "\nERR: " + supervisorErr.Error())
	}
	timeContents, timeErr := os.ReadFile(timeFile)
	timeDiag := string(timeContents)
	if timeErr != nil {
		timeDiag = timeErr.Error()
	}

	t.Fatalf("fresh gc init city spawned a claude worker but did not complete routed work within 4m\nwork bead:\n%+v\nsession bead:\n%+v\ntime.txt:\n%s\nstatus:\n%s\nsessions:\n%s\nsupervisor logs:\n%s",
		lastWorkBead, spawnedSessionBead, timeDiag, lastStatus, sessionOut, supervisorOut)
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
