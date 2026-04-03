//go:build acceptance_c

package tierc_test

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

var agentsRunningPattern = regexp.MustCompile(`(?m)^\s*(\d+)/(\d+)\s+agents running\b`)

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

	out, err := runGCWithTimeout(20*time.Second, testEnvC, c.Dir,
		"sling", "claude", "Write the current time to time.txt")
	require.NoError(t, err, "gc sling: %s", out)
	t.Logf("Slung work: %s", strings.TrimSpace(out))

	var lastStatus string
	spawned := pollForCondition(t, 90*time.Second, 5*time.Second, func() bool {
		statusOut, statusErr := runGCWithTimeout(10*time.Second, testEnvC, c.Dir, "status")
		lastStatus = statusOut
		if statusErr != nil {
			lastStatus = strings.TrimSpace(statusOut + "\nERR: " + statusErr.Error())
			return false
		}
		running, total, ok := parseRunningAgents(statusOut)
		return ok && total > 0 && running > 0
	})

	if spawned {
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

	t.Fatalf("fresh gc init city never spawned a running pool worker after gc sling within 90s\nlast status:\n%s\nsessions:\n%s\nsupervisor logs:\n%s",
		lastStatus, sessionOut, supervisorOut)
}

func runGCWithTimeout(timeout time.Duration, env *helpers.Env, dir string, args ...string) (string, error) {
	gcPath, err := exec.LookPath("gc")
	if err != nil {
		return "", fmt.Errorf("gc not found in PATH: %w", err)
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
