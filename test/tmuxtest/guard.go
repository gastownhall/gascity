// Package tmuxtest provides helpers for integration tests that need real tmux.
//
// Guard manages tmux session lifecycle for tests: it generates unique city
// names with a "gctest-" prefix, tracks created sessions, and guarantees
// cleanup even on test failures. Three layers prevent orphan sessions:
//
//  1. Pre-sweep (TestMain): kill all gctest-* socket servers from prior crashes.
//  2. Per-test (t.Cleanup): kill sessions created by this guard.
//  3. Post-sweep (TestMain defer): final sweep after all tests complete.
//
// All operations use isolated tmux socket roots and named gctest-* sockets so
// tests never interfere with the user's running tmux server.
package tmuxtest

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const tmuxGuardCommandTimeout = 2 * time.Second

const tmuxSiblingSocketStaleAfter = 24 * time.Hour

const (
	tmuxEnv     = "TMUX"
	tmuxPaneEnv = "TMUX_PANE"
	tmuxTmpEnv  = "TMUX_TMPDIR"
)

// ConfigureProcessEnv points all tmux commands in the current process tree at
// socketRoot and removes inherited client bindings from an outer tmux session.
func ConfigureProcessEnv(socketRoot string) error {
	socketRoot = strings.TrimSpace(socketRoot)
	if socketRoot == "" {
		return fmt.Errorf("tmux socket root is empty")
	}
	if err := os.MkdirAll(socketRoot, 0o700); err != nil {
		return fmt.Errorf("creating tmux socket root %q: %w", socketRoot, err)
	}
	if err := os.Unsetenv(tmuxEnv); err != nil {
		return fmt.Errorf("unsetting %s: %w", tmuxEnv, err)
	}
	if err := os.Unsetenv(tmuxPaneEnv); err != nil {
		return fmt.Errorf("unsetting %s: %w", tmuxPaneEnv, err)
	}
	if err := os.Setenv(tmuxTmpEnv, socketRoot); err != nil {
		return fmt.Errorf("setting %s: %w", tmuxTmpEnv, err)
	}
	return nil
}

// RequireTmux skips the test if tmux is not installed.
func RequireTmux(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// Guard manages tmux session lifecycle for a single test. It generates a
// unique city name with the "gctest-" prefix and guarantees cleanup of all
// sessions matching that city via t.Cleanup.
type Guard struct {
	t          testing.TB
	cityName   string // "gctest-<nibble>-<nibble>-..."
	socketName string // tmux socket for isolation (defaults to cityName)
}

// NewGuard creates a guard with a unique city name. Registers t.Cleanup
// to kill all sessions created under this guard's city name.
func NewGuard(t testing.TB) *Guard {
	return NewGuardWithSocket(t, "")
}

// NewGuardWithSocket creates a guard using the specified tmux socket.
func NewGuardWithSocket(t testing.TB, socketName string) *Guard {
	t.Helper()
	RequireTmux(t)

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("tmuxtest: generating random city name: %v", err)
	}
	hex := fmt.Sprintf("%x", b)
	parts := make([]string, 0, len(hex)+1)
	parts = append(parts, "gctest")
	for _, r := range hex {
		parts = append(parts, string(r))
	}
	cityName := strings.Join(parts, "-")
	if socketName == "" {
		socketName = cityName
	}

	g := &Guard{t: t, cityName: cityName, socketName: socketName}
	t.Cleanup(func() {
		g.killGuardSessions()
	})
	return g
}

// CityName returns the unique city name (e.g., "gctest-a-1-b-2-c-3-d-4").
func (g *Guard) CityName() string {
	return g.cityName
}

// SocketName returns the tmux socket name used by this guard.
func (g *Guard) SocketName() string {
	return g.socketName
}

// SessionName returns the expected tmux session name for an agent.
// Default session naming is just the sanitized agent name because per-city
// tmux socket isolation makes a city prefix unnecessary.
func (g *Guard) SessionName(agentName string) string {
	return strings.ReplaceAll(agentName, "/", "--")
}

// HasSession checks if a specific tmux session exists.
func (g *Guard) HasSession(name string) bool {
	g.t.Helper()
	args := tmuxArgs(g.socketName, "has-session", "-t", name)
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		// tmux has-session exits 1 when session doesn't exist
		// and also when no server is running. Both mean "not found".
		_ = out
		return false
	}
	return true
}

// killGuardSessions kills all tmux sessions matching this guard's city
// socket. One city maps to one socket, so all sessions on that socket
// belong to this guard.
func (g *Guard) killGuardSessions() {
	g.t.Helper()
	_ = killTestSocketServer(g.socketName)
}

// KillAllTestSessions kills tmux sessions for all orphaned gctest-* sockets.
// Call from TestMain before and after test runs to clean up orphans.
func KillAllTestSessions(t testing.TB) {
	t.Helper()
	var cleaned int
	for _, socketPath := range listTestSocketPaths() {
		if err := killTestSocketPath(socketPath); err == nil {
			cleaned++
		}
	}
	if cleaned > 0 {
		t.Logf("tmuxtest: cleaned up %d orphaned test socket(s)", cleaned)
	}
}

// tmuxArgs prepends -L socketName to the given tmux arguments when socketName
// is non-empty.
func tmuxArgs(socketName string, args ...string) []string {
	if socketName == "" {
		return args
	}
	return append([]string{"-L", socketName}, args...)
}

// listSessionsWithPrefix returns all tmux session names starting with prefix.
func killTestSocketServer(socketName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	args := tmuxArgs(socketName, "kill-server")
	return exec.CommandContext(ctx, "tmux", args...).Run()
}

func killTestSocketPath(socketPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxGuardCommandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", "-S", socketPath, "kill-server").Run()
}

// listTestSocketPaths returns tmux socket paths for orphaned gctest cities.
func listTestSocketPaths() []string {
	activeRoot := strings.TrimSpace(os.Getenv(tmuxTmpEnv))
	if activeRoot != "" {
		activeRoot = filepath.Clean(activeRoot)
	}
	now := time.Now()
	uid := strconv.Itoa(os.Getuid())
	var sockets []string
	for _, root := range tmuxSocketSearchRoots() {
		entries, err := filepath.Glob(filepath.Join(root, "tmux-"+uid, "gctest-*"))
		if err != nil {
			continue
		}
		for _, socketPath := range entries {
			if root == activeRoot || testSocketPathIsStale(socketPath, now) {
				sockets = append(sockets, socketPath)
			}
		}
	}
	return sockets
}

func testSocketPathIsStale(socketPath string, now time.Time) bool {
	info, err := os.Stat(socketPath)
	if err != nil {
		return false
	}
	return now.Sub(info.ModTime()) >= tmuxSiblingSocketStaleAfter
}

func tmuxSocketSearchRoots() []string {
	roots := make([]string, 0, 8)
	seen := make(map[string]struct{})
	addRoot := func(root string) {
		root = strings.TrimSpace(root)
		if root == "" {
			return
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		roots = append(roots, root)
	}

	activeRoot := os.Getenv(tmuxTmpEnv)
	addRoot(activeRoot)
	for _, pattern := range tmuxSocketRootPatterns(activeRoot) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			addRoot(match)
		}
	}
	return roots
}

func tmuxSocketRootPatterns(activeRoot string) []string {
	activeRoot = strings.TrimSpace(activeRoot)
	if activeRoot == "" || filepath.Base(activeRoot) != "tmux" {
		return nil
	}
	activeRoot = filepath.Clean(activeRoot)
	runRoot := filepath.Dir(activeRoot)
	runName := filepath.Base(runRoot)
	namespace := filepath.Dir(runRoot)
	if runName == "runtime" {
		runRoot = filepath.Dir(runRoot)
		runName = filepath.Base(runRoot)
		namespace = filepath.Dir(runRoot)
		return runtimeTmuxSocketRootPatterns(namespace, runName)
	}
	return directTmuxSocketRootPatterns(namespace, runName)
}

func directTmuxSocketRootPatterns(namespace, runName string) []string {
	switch {
	case strings.HasPrefix(runName, "gc-integration-"):
		return []string{filepath.Join(namespace, "gc-integration-*", "tmux")}
	case strings.HasPrefix(runName, "gctutorial-"):
		return []string{filepath.Join(namespace, "gctutorial-*", "tmux")}
	case strings.HasPrefix(runName, "gct"):
		return []string{filepath.Join(namespace, "gct*", "tmux")}
	default:
		return nil
	}
}

func runtimeTmuxSocketRootPatterns(namespace, runName string) []string {
	switch {
	case strings.HasPrefix(runName, "gcac-"):
		return []string{filepath.Join(namespace, "gcac-*", "runtime", "tmux")}
	case strings.HasPrefix(runName, "gcwi-"):
		return []string{filepath.Join(namespace, "gcwi-*", "runtime", "tmux")}
	case strings.HasPrefix(runName, "gc-acceptance-b-"):
		return []string{filepath.Join(namespace, "gc-acceptance-b-*", "runtime", "tmux")}
	case strings.HasPrefix(runName, "gc-acceptance-"):
		return []string{filepath.Join(namespace, "gc-acceptance-*", "runtime", "tmux")}
	default:
		return nil
	}
}
