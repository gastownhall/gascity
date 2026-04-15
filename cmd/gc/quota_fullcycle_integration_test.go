//go:build acceptance_b

// Full scan+rotate cycle integration test for quota rotation.
//
// Requires real tmux. Creates test tmux sessions, injects rate-limit text,
// runs scan, then rotate, and verifies the session's CLAUDE_CONFIG_DIR is
// updated. PRD refs: G4 (full rotation cycle), G7 (integration tested).
package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/account"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// TestScanRotateFullCycle creates real tmux sessions, injects rate-limit
// text via send-keys, runs doQuotaScan to detect the rate-limited account,
// then runs doQuotaRotate to switch the session to an available account,
// and verifies the CLAUDE_CONFIG_DIR environment variable was updated.
func TestScanRotateFullCycle(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	socket := guard.SocketName()

	// Setup: create temp config dirs for two accounts.
	configDir1 := t.TempDir() // "work1" config
	configDir2 := t.TempDir() // "work2" config

	// Register two accounts.
	reg := account.Registry{
		Default: "work1",
		Accounts: []account.Account{
			{Handle: "work1", ConfigDir: configDir1},
			{Handle: "work2", ConfigDir: configDir2},
		},
	}

	// Create a tmux session on the isolated socket for "agent1".
	sessionName := guard.SessionName("agent1")
	runTmux(t, socket, "new-session", "-d", "-s", sessionName)

	// Set CLAUDE_CONFIG_DIR to work1's config dir on the session.
	runTmux(t, socket, "setenv", "-t", sessionName, "CLAUDE_CONFIG_DIR", configDir1)

	// Inject rate-limit text into the pane via send-keys.
	runTmux(t, socket, "send-keys", "-t", sessionName, "echo 'Your account has reached its rate limit'", "Enter")

	// Wait for the text to appear in the pane.
	time.Sleep(500 * time.Millisecond)

	// Build a TmuxOps that uses our isolated socket.
	tmuxOps := socketTmuxOps(t, socket)

	// Scan: should detect work1 as rate-limited.
	providerPatterns := map[string][]string{"test": {"rate limit"}}
	clk := clock.Real{}
	state, warnings, err := doQuotaScan(tmuxOps, providerPatterns, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaScan failed: %v (warnings: %v)", err, warnings)
	}

	w1State, ok := state.Accounts["work1"]
	if !ok {
		t.Fatalf("scan did not produce state for work1; got accounts: %v (warnings: %v)", state.Accounts, warnings)
	}
	if w1State.Status != config.QuotaStatusLimited {
		t.Errorf("work1 status after scan = %q, want %q", w1State.Status, config.QuotaStatusLimited)
	}

	// Rotate: should switch the session from work1 to work2.
	newState, rotateWarnings, err := doQuotaRotate(tmuxOps, state, reg, clk)
	if err != nil {
		t.Fatalf("doQuotaRotate failed: %v (warnings: %v)", err, rotateWarnings)
	}

	// Verify work2 was selected (LRU: work2 never used before).
	w2State, ok := newState.Accounts["work2"]
	if !ok {
		t.Fatalf("rotate did not produce state for work2; got accounts: %v", newState.Accounts)
	}
	if w2State.LastUsed == "" {
		t.Error("work2 last_used should be set after rotation")
	}

	// Verify the session's CLAUDE_CONFIG_DIR was updated to work2's config dir.
	newConfigDir := readTmuxEnv(t, socket, sessionName, "CLAUDE_CONFIG_DIR")
	if newConfigDir != configDir2 {
		t.Errorf("session CLAUDE_CONFIG_DIR = %q, want %q (work2)", newConfigDir, configDir2)
	}
}

// runTmux executes a tmux command on the given socket.
func runTmux(t *testing.T, socket string, args ...string) {
	t.Helper()
	fullArgs := append([]string{"-L", socket}, args...)
	out, err := exec.Command("tmux", fullArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
}

// readTmuxEnv reads a tmux session environment variable.
func readTmuxEnv(t *testing.T, socket, session, key string) string {
	t.Helper()
	args := []string{"-L", socket, "showenv", "-t", session, key}
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		t.Fatalf("tmux showenv %s: %v", key, err)
	}
	// Output format: "KEY=value\n"
	line := strings.TrimSpace(string(out))
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected tmux showenv output: %q", line)
	}
	return parts[1]
}

// socketTmuxOps returns a TmuxOps that uses an isolated tmux socket
// instead of the default server. This is similar to DefaultTmuxOps()
// but targets the test-specific socket.
func socketTmuxOps(t *testing.T, socket string) TmuxOps {
	t.Helper()
	return TmuxOps{
		CapturePane: func(sessionName string, lines int) (string, error) {
			args := []string{"-L", socket, "capture-pane", "-t", sessionName, "-p", "-S", fmt.Sprintf("-%d", lines)}
			out, err := exec.Command("tmux", args...).Output()
			return string(out), err
		},
		ShowEnv: func(sessionName, key string) (string, error) {
			args := []string{"-L", socket, "showenv", "-t", sessionName, key}
			out, err := exec.Command("tmux", args...).Output()
			if err != nil {
				return "", err
			}
			line := strings.TrimSpace(string(out))
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				return "", nil
			}
			return parts[1], nil
		},
		SetEnv: func(sessionName, key, value string) error {
			args := []string{"-L", socket, "setenv", "-t", sessionName, key, value}
			return exec.Command("tmux", args...).Run()
		},
		RespawnPane: func(sessionName string) error {
			// Get pane ID first.
			args := []string{"-L", socket, "list-panes", "-t", sessionName, "-F", "#{pane_id}"}
			out, err := exec.Command("tmux", args...).Output()
			if err != nil {
				return err
			}
			paneID := strings.TrimSpace(strings.Split(string(out), "\n")[0])
			// Respawn with a simple shell.
			args = []string{"-L", socket, "respawn-pane", "-k", "-t", paneID, "bash"}
			return exec.Command("tmux", args...).Run()
		},
		ListPanes: func() ([]PaneInfo, error) {
			args := []string{"-L", socket, "list-sessions", "-F", "#{session_name}"}
			out, err := exec.Command("tmux", args...).Output()
			if err != nil {
				return nil, err
			}
			var panes []PaneInfo
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if line == "" {
					continue
				}
				panes = append(panes, PaneInfo{SessionName: line})
			}
			return panes, nil
		},
		IsRunning: func() bool {
			args := []string{"-L", socket, "list-sessions"}
			return exec.Command("tmux", args...).Run() == nil
		},
	}
}
