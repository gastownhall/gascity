//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/testutil"
)

// Compile-time check.
var _ runtime.Provider = (*Provider)(nil)

// tmuxConformanceConfig builds the isolated-socket configuration used by
// TestTmuxConformance. The conformance fixture is a generic long-running
// command, not an agent TUI with an observable idle prompt, so it keeps a
// short real Nudge timeout to cover the wait/fallback branch without
// consuming the production 30-second budget.
func tmuxConformanceConfig() Config {
	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	cfg.NudgeIdleTimeout = 250 * time.Millisecond
	return cfg
}

func TestTmuxConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBackedWithConfig(tmuxConformanceConfig()), runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, fmt.Sprintf("gc-test-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

func TestProvider_StartStopIsRunning(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-adapter"

	// Clean slate.
	_ = p.Stop(name)

	if p.IsRunning(name) {
		t.Fatal("session should not exist before Start")
	}

	if err := p.Start(context.Background(), name, runtime.Config{Command: "sleep 300"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop(name) }()

	if !p.IsRunning(name) {
		t.Fatal("session should be running after Start")
	}

	// Duplicate start returns an error.
	if err := p.Start(context.Background(), name, runtime.Config{}); err == nil {
		t.Fatal("duplicate Start should return error")
	}

	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if p.IsRunning(name) {
		t.Fatal("session should not be running after Stop")
	}

	// Idempotent stop.
	if err := p.Stop(name); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func TestProvider_StartWithEnv(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-adapter-env"
	_ = p.Stop(name)

	err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 300",
		Env:     map[string]string{"GC_TEST": "hello"},
	})
	if err != nil {
		t.Fatalf("Start with env: %v", err)
	}
	defer func() { _ = p.Stop(name) }()

	// Verify the env var was set.
	val, err := p.Tmux().GetEnvironment(name, "GC_TEST")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if val != "hello" {
		t.Fatalf("GC_TEST: got %q, want %q", val, "hello")
	}
}

func TestProviderStopUnattendedSessionRealNamedTmuxBoundary(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script executable not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gctest-certify-unattended-%d-%d", os.Getpid(), time.Now().UnixNano())
	p := NewProviderWithConfig(cfg)
	name := "certify-unattended"
	const token = "certification-token"
	socketPath := namedSocketPath(cfg.SocketName)
	defaultPath := namedSocketPath("default")
	defaultBefore, defaultBeforeErr := os.Lstat(defaultPath)
	serverStopped := false
	t.Cleanup(func() {
		if !serverStopped {
			if err := p.TeardownServer(); err != nil && !errors.Is(err, ErrNoServer) {
				t.Errorf("tear down exact named tmux server: %v", err)
			}
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove exact named tmux socket: %v", err)
		}
	})

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 300",
		Env:     map[string]string{"GC_INSTANCE_TOKEN": token},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stubbornReady := fmt.Sprintf("gc-cert-stubborn-ready-%d", time.Now().UnixNano())
	stubbornCommand := "trap '' HUP TERM; tmux -L " + shellquote.Quote(cfg.SocketName) + " wait-for -S " + shellquote.Quote(stubbornReady) + "; while :; do sleep 1; done"
	secondPane, err := p.tm.run("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "="+name, "exec sh -c "+shellquote.Quote(stubbornCommand))
	if err != nil {
		t.Fatalf("create second-window pane: %v", err)
	}
	if !wellFormedTmuxID(secondPane, '%') {
		t.Fatalf("second-window pane ID = %q, want %%<digits>", secondPane)
	}

	waitSignal := func(signal, description string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		defer cancel()
		if _, err := p.tm.runCtx(ctx, "wait-for", signal); err != nil {
			t.Fatalf("wait for %s: %v", description, err)
		}
	}
	waitSignal(stubbornReady, "stubborn second-pane process startup")
	secondPanePID, err := p.tm.GetPanePID(secondPane)
	if err != nil {
		t.Fatalf("read stubborn second-pane process PID: %v", err)
	}
	if !processAlive(secondPanePID) {
		t.Fatalf("stubborn second-pane process %s is not alive before certified stop", secondPanePID)
	}
	stubbornTarget := mustSnapshotTarget(t, mustProcessSnapshot(t), mustPID(t, secondPanePID))
	// A second provider instance owns the real hidden PTY. It is therefore
	// untracked by p, exactly like a human client from the certifier's point of
	// view; p must observe it without closing or detaching it.
	other := NewProviderWithConfig(cfg)
	if err := other.tm.ensureHiddenAttachedClient(name); err != nil {
		t.Fatalf("attach real PTY through second provider: %v", err)
	}
	defer other.tm.CloseHiddenAttachClient(name)
	if err := p.StopUnattendedSession(name, token); err == nil || !strings.Contains(err.Error(), "attached clients") {
		t.Fatalf("attached real PTY stop = %v, want attached-client park", err)
	}
	if has, err := p.tm.HasSession(name); err != nil || !has {
		t.Fatalf("attached stop killed target: has=%v err=%v", has, err)
	}
	if !other.tm.IsSessionAttached(name) {
		t.Fatal("bound stop detached the PTY tracked only by the second provider")
	}

	detachedSignal := fmt.Sprintf("gc-cert-detached-%d", time.Now().UnixNano())
	if _, err := p.tm.run("set-hook", "-t", name, "client-detached", "wait-for -S "+detachedSignal); err != nil {
		t.Fatalf("arm client-detached signal: %v", err)
	}
	other.tm.CloseHiddenAttachClient(name)
	waitSignal(detachedSignal, "real PTY detachment")

	if _, err := p.tm.run("copy-mode", "-t", secondPane); err != nil {
		t.Fatalf("enter copy mode on second-window pane: %v", err)
	}
	if err := p.StopUnattendedSession(name, token); err == nil || !strings.Contains(err.Error(), "copy-mode panes") {
		t.Fatalf("second-window copy-mode stop = %v, want copy-mode park", err)
	}
	if has, err := p.tm.HasSession(name); err != nil || !has {
		t.Fatalf("copy-mode stop killed target: has=%v err=%v", has, err)
	}
	if _, err := p.tm.run("send-keys", "-t", secondPane, "-X", "cancel"); err != nil {
		t.Fatalf("clear second-window copy mode: %v", err)
	}
	observer := "certify-unattended-observer"
	if _, err := p.tm.run("new-session", "-d", "-s", observer, "sleep 300"); err != nil {
		t.Fatalf("create observer session: %v", err)
	}
	if _, err := p.tm.run("link-window", "-s", "="+name+":^", "-t", "="+observer); err != nil {
		t.Fatalf("link target window into observer session: %v", err)
	}
	if err := other.tm.ensureHiddenAttachedClient(observer); err != nil {
		t.Fatalf("attach observer PTY: %v", err)
	}
	if err := p.StopUnattendedSession(name, token); err == nil || !strings.Contains(err.Error(), "linked windows") {
		t.Fatalf("linked-window stop = %v, want linked-window park", err)
	}
	if has, err := p.tm.HasSession(name); err != nil || !has {
		t.Fatalf("linked-window stop killed target: has=%v err=%v", has, err)
	}
	other.tm.CloseHiddenAttachClient(observer)
	if err := p.tm.KillSession(observer); err != nil {
		t.Fatalf("remove observer session: %v", err)
	}
	if err := p.StopUnattendedSession(name, token); err != nil {
		t.Fatalf("stable detached stop: %v", err)
	}
	if has, err := p.tm.HasSession(name); err != nil || has {
		t.Fatalf("certified stop left session present: has=%v err=%v", has, err)
	}
	// The stop returns once it has sent SIGKILL, before the stubborn process
	// has exited and before tmux, its parent, has reaped it; kill(0) reads both
	// states as alive. Wait for its start identity to go instead. It ignores HUP
	// and TERM, so a stop that never sent the KILL still leaves it running past
	// the deadline.
	waitForProcessTargetsGone(t, []processTarget{stubbornTarget}, testutil.ExecRaceTimeout)

	if err := p.TeardownServer(); err != nil {
		t.Fatalf("tear down exact named tmux server: %v", err)
	}
	serverStopped = true
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove exact named tmux socket: %v", err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("named tmux socket still exists after exact cleanup: %v", err)
	}
	defaultAfter, defaultAfterErr := os.Lstat(defaultPath)
	switch {
	case errors.Is(defaultBeforeErr, os.ErrNotExist):
		if !errors.Is(defaultAfterErr, os.ErrNotExist) {
			t.Fatalf("default tmux socket was created: %v", defaultAfterErr)
		}
	case defaultBeforeErr != nil:
		t.Fatalf("inspect default tmux socket before proof: %v", defaultBeforeErr)
	case defaultAfterErr != nil:
		t.Fatalf("default tmux socket changed during proof: %v", defaultAfterErr)
	case !os.SameFile(defaultBefore, defaultAfter):
		t.Fatal("default tmux socket identity changed during named-socket proof")
	}
}

func TestProvider_StartUnsetsControllerColorEnvironment(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gc-test-color-%d", time.Now().UnixNano())
	p := NewProviderWithConfig(cfg)
	t.Cleanup(func() { _ = p.TeardownServer() })
	name := "gc-test-adapter-color-env"

	outPath := filepath.Join(t.TempDir(), "env.txt")
	tmpPath := outPath + ".tmp"
	ready := "gc-test-color-ready"
	script := "env > " + shellquote.Quote(tmpPath) + "; mv " + shellquote.Quote(tmpPath) + " " + shellquote.Quote(outPath) + "; tmux -L " + shellquote.Quote(cfg.SocketName) + " wait-for -S " + shellquote.Quote(ready) + "; sleep 300"
	commandPath := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatalf("writing Claude fixture: %v", err)
	}
	if err := p.Start(context.Background(), name, runtime.Config{
		Command:      shellquote.Quote(commandPath),
		ProviderName: "claude",
		Env: map[string]string{
			"CI":       "1",
			"NO_COLOR": "1",
			"CIRCLECI": "true",
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	readyCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.Tmux().runCtx(readyCtx, "wait-for", ready); err != nil {
		t.Fatalf("waiting for pane environment signal: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading pane environment after readiness signal: %v", err)
	}
	env := string(data)
	for _, line := range strings.Split(env, "\n") {
		if strings.HasPrefix(line, "CI=") || strings.HasPrefix(line, "NO_COLOR=") {
			t.Fatalf("interactive pane inherited color-killing environment:\n%s", env)
		}
	}
	if !strings.Contains(env, "CIRCLECI=true") {
		t.Fatalf("unrelated CI-vendor environment was removed:\n%s", env)
	}
}

// TestProvider_RelaunchInWarmSession proves the un-weld relaunch path (B1):
// Relaunch respawns the agent with a NEW command inside the SAME box, the box is
// reused (its session env survives, since Relaunch never re-sets env), and a
// relaunch into a non-existent box is an error rather than a silent provision.
func TestProvider_RelaunchInWarmSession(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-relaunch-warm"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker")
	// Single-string sh -c command, passed to tmux the same way the long-prompt
	// path already does (see ensureFreshSession), so tmux runs it intact.
	agentCmd := func(tag string) string {
		return fmt.Sprintf("sh -c 'echo %s > %s; sleep 300'", tag, marker)
	}

	// Provision the warm box (welded Start) with a sentinel session env value.
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: agentCmd("first"),
		WorkDir: workDir,
		Env:     map[string]string{"GC_RELAUNCH_TEST": "warm"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForMarker(t, marker, "first")

	// Relaunch the agent in the SAME box with a new command. Env is provision-half
	// and intentionally NOT re-passed; the box keeps its session environment.
	if err := p.Relaunch(context.Background(), name, runtime.Config{
		Command: agentCmd("second"),
		WorkDir: workDir,
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitForMarker(t, marker, "second")

	if !p.IsRunning(name) {
		t.Fatal("session should still be running after Relaunch")
	}

	// The box was reused, not recreated: the session env set at Start survives a
	// launch-only relaunch (Relaunch never re-sets env).
	val, err := p.Tmux().GetEnvironment(name, "GC_RELAUNCH_TEST")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if val != "warm" {
		t.Fatalf("GC_RELAUNCH_TEST after relaunch = %q, want %q (warm box should be reused, not recreated)", val, "warm")
	}

	// Relaunch into a non-existent box is an error, not a silent provision.
	err = p.Relaunch(context.Background(), "gc-test-relaunch-absent", runtime.Config{Command: "sleep 300"})
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Relaunch of absent box = %v, want ErrSessionNotFound", err)
	}
}

// Relaunch replaces the agent pane (the first pane of the first window), not
// whichever pane is active. A human who split the agent's window and then
// opened and focused a second window must keep both: before the fix,
// respawn-pane -t "=name:" killed the focused window's pane, ran the new agent
// there, and left the old agent running in pane 0.0.
func TestProvider_RelaunchRespawnsAgentPaneNotActiveWindow(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gctest-relaunch-pane-%d-%d", os.Getpid(), time.Now().UnixNano())
	p := NewProviderWithConfig(cfg)
	name := "gc-test-relaunch-pane"
	socketPath := namedSocketPath(cfg.SocketName)
	t.Cleanup(func() {
		if err := p.TeardownServer(); err != nil && !errors.Is(err, ErrNoServer) {
			t.Errorf("tear down test tmux server: %v", err)
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test tmux socket: %v", err)
		}
	})

	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker")
	agentCmd := func(tag string) string {
		return fmt.Sprintf("sh -c 'echo %s > %s; sleep 300'", tag, marker)
	}
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: agentCmd("first"),
		WorkDir: workDir,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForMarker(t, marker, "first")

	agentPane, err := agentPaneID(p.tm, name)
	if err != nil {
		t.Fatalf("agent pane id: %v", err)
	}
	agentPIDBefore, err := p.tm.GetPanePID(agentPane)
	if err != nil {
		t.Fatalf("agent pane pid: %v", err)
	}

	// pane-base-index 1 (a common tmux.conf setting) leaves no pane index 0,
	// so a literal ":^.0" respawn target fails; the agent pane must still be found.
	if _, err := p.tm.run("set-option", "-g", "pane-base-index", "1"); err != nil {
		t.Fatalf("set pane-base-index: %v", err)
	}

	// The human's split in the agent's window: created without -d, so it is
	// that window's active pane. Window ^ now has two panes, so a resolver
	// that answers for the window's active pane (display-message on ":^.0"
	// under pane-base-index 1) or picks the wrong line names the split.
	splitPane, err := p.tm.run("split-window", "-P", "-F", "#{pane_id}", "-t", paneTarget(name)+"^", "sleep 300")
	if err != nil {
		t.Fatalf("split agent window: %v", err)
	}
	if !wellFormedTmuxID(splitPane, '%') {
		t.Fatalf("split pane id = %q, want %%<digits>", splitPane)
	}
	if active, err := p.tm.run("display-message", "-t", paneTarget(name)+"^", "-p", "#{pane_id}"); err != nil || strings.TrimSpace(active) != splitPane {
		t.Fatalf("agent window active pane = (%q, %v), want the split %q (precondition)", strings.TrimSpace(active), err, splitPane)
	}
	splitPID, err := p.tm.GetPanePID(splitPane)
	if err != nil {
		t.Fatalf("split pane pid: %v", err)
	}

	// The human's window: created without -d, so it becomes the active window.
	humanPane, err := p.tm.run("new-window", "-P", "-F", "#{pane_id}", "-t", paneTarget(name), "sleep 300")
	if err != nil {
		t.Fatalf("create second window: %v", err)
	}
	if !wellFormedTmuxID(humanPane, '%') {
		t.Fatalf("second-window pane id = %q, want %%<digits>", humanPane)
	}
	active, err := p.tm.run("display-message", "-t", paneTarget(name), "-p", "#{pane_id}")
	if err != nil {
		t.Fatalf("read active pane: %v", err)
	}
	if strings.TrimSpace(active) != humanPane {
		t.Fatalf("active pane = %q, want the second window's %q (precondition)", strings.TrimSpace(active), humanPane)
	}
	humanPID, err := p.tm.GetPanePID(humanPane)
	if err != nil {
		t.Fatalf("second-window pane pid: %v", err)
	}

	if err := p.Relaunch(context.Background(), name, runtime.Config{
		Command: agentCmd("second"),
		WorkDir: workDir,
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitForMarker(t, marker, "second")

	// The agent pane was respawned: same pane, new process, old agent gone.
	if got, err := agentPaneID(p.tm, name); err != nil || got != agentPane {
		t.Fatalf("first pane after Relaunch = (%q, %v), want %q", got, err, agentPane)
	}
	agentPIDAfter, err := p.tm.GetPanePID(agentPane)
	if err != nil {
		t.Fatalf("agent pane pid after Relaunch: %v", err)
	}
	if agentPIDAfter == agentPIDBefore {
		t.Fatalf("agent pane pid unchanged (%s): the agent pane was not respawned", agentPIDAfter)
	}
	if processAlive(agentPIDBefore) {
		t.Fatalf("old agent process %s still alive after Relaunch", agentPIDBefore)
	}

	// The second window survived with its original process.
	if got, err := p.tm.GetPanePID(humanPane); err != nil || got != humanPID {
		t.Fatalf("second-window pane pid after Relaunch = (%q, %v), want %q (pane respawned or killed)", got, err, humanPID)
	}
	if !processAlive(humanPID) {
		t.Fatalf("second-window process %s killed by Relaunch", humanPID)
	}
	if got, err := p.tm.GetPanePID(splitPane); err != nil || got != splitPID {
		t.Fatalf("split pane pid after Relaunch = (%q, %v), want %q (pane respawned or killed)", got, err, splitPID)
	}
	if !processAlive(splitPID) {
		t.Fatalf("split process %s killed by Relaunch", splitPID)
	}
}

// agentPaneFixture is a Provider on its own tmux server holding one started
// agent session, for the agent-pane resolution tests.
type agentPaneFixture struct {
	p        *Provider
	name     string
	marker   string
	workDir  string
	agent    string // the agent's pane id
	agentPID string
}

func (f *agentPaneFixture) agentCmd(tag string) string {
	return fmt.Sprintf("sh -c 'echo %s > %s; sleep 300'", tag, f.marker)
}

func (f *agentPaneFixture) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.p.tm.run(args...)
	if err != nil {
		t.Fatalf("tmux %q: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// newAgentPaneFixture starts an agent session on a private server whose
// global base-index and pane-base-index are base (a holder session keeps the
// server up while they are set, so the agent session is created under them).
func newAgentPaneFixture(t *testing.T, tag, base string) *agentPaneFixture {
	t.Helper()
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName(tag)
	f := &agentPaneFixture{p: NewProviderWithConfig(cfg), name: "gc-test-agent-pane", workDir: t.TempDir()}
	f.marker = filepath.Join(f.workDir, "marker")
	socketPath := namedSocketPath(cfg.SocketName)
	t.Cleanup(func() {
		if err := f.p.TeardownServer(); err != nil && !errors.Is(err, ErrNoServer) {
			t.Errorf("tear down test tmux server: %v", err)
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test tmux socket: %v", err)
		}
	})
	f.mustRun(t, "new-session", "-d", "-s", "gc-test-holder", "sleep 300")
	f.mustRun(t, "set-option", "-g", "base-index", base)
	f.mustRun(t, "set-option", "-wg", "pane-base-index", base)
	if err := f.p.Start(context.Background(), f.name, runtime.Config{Command: f.agentCmd("first"), WorkDir: f.workDir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForMarker(t, f.marker, "first")
	f.agent = f.mustRun(t, "list-panes", "-t", paneTarget(f.name), "-F", "#{pane_id}")
	f.agentPID = f.mustRun(t, "display-message", "-p", "-t", f.agent, "#{pane_pid}")
	return f
}

// requireAgentPaneResolved checks that the session-keyed reads and the kill
// plan describe the agent's pane, whatever a human has done around it.
func (f *agentPaneFixture) requireAgentPaneResolved(t *testing.T) {
	t.Helper()
	tm := f.p.tm
	if got, err := agentPaneID(tm, f.name); err != nil || got != f.agent {
		t.Errorf("agent pane id = (%q, %v), want the agent's %q", got, err, f.agent)
	}
	if got, err := tm.GetPanePID(f.name); err != nil || got != f.agentPID {
		t.Errorf("GetPanePID = (%q, %v), want the agent's %q", got, err, f.agentPID)
	}
	if dead, err := tm.IsPaneDead(f.name); err != nil || dead {
		t.Errorf("IsPaneDead = (%v, %v), want the live agent's false", dead, err)
	}
	want := f.mustRun(t, "display-message", "-p", "-t", f.agent, "#{pane_current_command}|#{pane_current_path}|#{pane_dead_status}|#{pane_dead_signal}")
	cmd, cmdErr := tm.GetPaneCommand(f.name)
	dir, dirErr := tm.GetPaneWorkDir(f.name)
	status, signal := tm.PaneDeadInfo(f.name)
	if got := strings.Join([]string{cmd, dir, status, signal}, "|"); cmdErr != nil || dirErr != nil || got != want {
		t.Errorf("GetPaneCommand|GetPaneWorkDir|PaneDeadInfo = %q (%v, %v), want the agent pane's %q", got, cmdErr, dirErr, want)
	}
	if !tm.IsSessionRunning(f.name) {
		t.Error("IsSessionRunning = false, want true for the live agent")
	}
	plan, err := tm.capturePaneProcessKillPlan(f.name, nil)
	if err != nil || plan.Leader == nil || strconv.Itoa(plan.Leader.PID) != f.agentPID {
		t.Errorf("kill plan = (%+v, %v), want it rooted at the agent's %s", plan, err, f.agentPID)
	}
}

// relaunchAndRequireAgentRespawned relaunches the session and checks that the
// agent's pane, and only it, got the new process.
func (f *agentPaneFixture) relaunchAndRequireAgentRespawned(t *testing.T, humanPanes ...string) {
	t.Helper()
	humanState := map[string]string{}
	for _, pane := range humanPanes {
		humanState[pane] = f.mustRun(t, "display-message", "-p", "-t", pane, "#{pane_pid} #{pane_dead}")
	}
	if err := f.p.Relaunch(context.Background(), f.name, runtime.Config{Command: f.agentCmd("second"), WorkDir: f.workDir}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitForMarker(t, f.marker, "second")
	if got := f.mustRun(t, "display-message", "-p", "-t", f.agent, "#{pane_pid}"); got == f.agentPID {
		t.Errorf("agent pane %s pid still %s, want a respawned process", f.agent, got)
	} else {
		f.agentPID = got
	}
	if !processAlive(f.agentPID) {
		t.Errorf("respawned agent %s is not alive", f.agentPID)
	}
	for pane, before := range humanState {
		if got := f.mustRun(t, "display-message", "-p", "-t", pane, "#{pane_pid} #{pane_dead}"); got != before {
			t.Errorf("human pane %s = %q after Relaunch, want %q (untouched)", pane, got, before)
		}
	}
	f.requireAgentPaneResolved(t)
}

// Session-keyed pane reads, the kill plan and Relaunch resolve to the agent's
// pane, never the session's active pane. A human has split the agent's window
// (the split's process exited, leaving a dead pane kept by remain-on-exit,
// made active) and opened a second window, which is active. Run under tmux's
// default indexes and under base-index 1 / pane-base-index 1, which has no
// pane 0.
func TestProvider_AgentPaneIgnoresHumanSplitAndWindow(t *testing.T) {
	for _, base := range []string{"0", "1"} {
		t.Run("base-index="+base, func(t *testing.T) {
			f := newAgentPaneFixture(t, "aph"+base, base)
			split := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "true")
			waitForValue(t, "split pane "+split+" pane_dead", "1", func() string {
				return f.mustRun(t, "display-message", "-p", "-t", split, "#{pane_dead}")
			})
			other := f.mustRun(t, "new-window", "-P", "-F", "#{pane_id}", "-t", paneTarget(f.name), "sleep 300")
			if got := f.mustRun(t, "display-message", "-p", "-t", paneTarget(f.name), "#{pane_id}"); got != other {
				t.Fatalf("active pane = %s, want the second window's %s (precondition)", got, other)
			}
			if got := f.mustRun(t, "display-message", "-p", "-t", paneTarget(f.name)+"^", "#{pane_id}"); got != split {
				t.Fatalf("first window's active pane = %s, want the split %s (precondition)", got, split)
			}

			f.requireAgentPaneResolved(t)
			f.relaunchAndRequireAgentRespawned(t, split, other)
		})
	}
}

// The agent pane is found by its marker, not its position. Each case is a
// human action that moves another pane ahead of the agent's in list order,
// which a first-pane resolver would mistake for the agent.
func TestProvider_AgentPaneMarkerSurvivesReorder(t *testing.T) {
	cases := []struct {
		name    string
		reorder func(t *testing.T, f *agentPaneFixture) []string // returns the human panes
	}{
		{"split-window -b", func(t *testing.T, f *agentPaneFixture) []string {
			return []string{f.mustRun(t, "split-window", "-b", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")}
		}},
		{"swap-pane", func(t *testing.T, f *agentPaneFixture) []string {
			human := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")
			f.mustRun(t, "swap-pane", "-s", f.agent, "-t", human)
			return []string{human}
		}},
		{"rotate-window", func(t *testing.T, f *agentPaneFixture) []string {
			human := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")
			f.mustRun(t, "rotate-window", "-t", f.agent)
			return []string{human}
		}},
		{"break-pane", func(t *testing.T, f *agentPaneFixture) []string {
			human := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")
			f.mustRun(t, "break-pane", "-d", "-s", f.agent, "-t", paneTarget(f.name))
			return []string{human}
		}},
		{"new-window -b", func(t *testing.T, f *agentPaneFixture) []string {
			return []string{f.mustRun(t, "new-window", "-b", "-P", "-F", "#{pane_id}", "-t", paneTarget(f.name)+"^", "sleep 300")}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentPaneFixture(t, fmt.Sprintf("apr%d", i), "1")
			humans := tc.reorder(t, f)
			first, _, _ := strings.Cut(f.mustRun(t, "list-panes", "-s", "-t", paneTarget(f.name), "-F", "#{pane_id}"), "\n")
			if first == f.agent {
				t.Fatalf("%s left the agent pane %s first (precondition)", tc.name, f.agent)
			}

			f.requireAgentPaneResolved(t)
			f.relaunchAndRequireAgentRespawned(t, humans...)
		})
	}
}

// A human kills the marked agent pane and the session lives on in their
// split. Nothing may be answered from, or done to, the human's pane: reads
// see no agent, the kill plan is empty, and Relaunch refuses.
func TestProvider_KilledAgentPaneFailsClosed(t *testing.T) {
	f := newAgentPaneFixture(t, "apk", "1")
	human := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")
	humanPID := f.mustRun(t, "display-message", "-p", "-t", human, "#{pane_pid}")
	f.mustRun(t, "kill-pane", "-t", f.agent)
	tm := f.p.tm

	if _, err := agentPaneID(tm, f.name); !errors.Is(err, errAgentPaneGone) {
		t.Errorf("agent pane id err = %v, want errAgentPaneGone", err)
	}
	if dead, err := tm.IsPaneDead(f.name); err != nil || !dead {
		t.Errorf("IsPaneDead = (%v, %v), want dead: the agent's pane is gone", dead, err)
	}
	if tm.IsSessionRunning(f.name) {
		t.Error("IsSessionRunning = true, want false with the agent's pane gone")
	}
	if plan, err := tm.capturePaneProcessKillPlan(f.name, nil); err != nil || plan.Leader != nil || len(plan.Descendants) != 0 {
		t.Errorf("kill plan = (%+v, %v), want empty, never the human's tree", plan, err)
	}
	if err := f.p.Relaunch(context.Background(), f.name, runtime.Config{Command: f.agentCmd("second"), WorkDir: f.workDir}); err == nil {
		t.Error("Relaunch = nil, want a refusal: it would replace the human's pane")
	}
	if got := f.mustRun(t, "display-message", "-p", "-t", human, "#{pane_pid} #{pane_dead}"); got != humanPID+" 0" {
		t.Errorf("human pane %s = %q, want %q (untouched)", human, got, humanPID+" 0")
	}
}

// Another gc session's agent pane, joined in front of this session's agent,
// carries a marker valued with its own session's id: it reads as a foreign
// pane, never as this session's agent.
func TestProvider_JoinedForeignAgentPaneIsNotTheAgent(t *testing.T) {
	f := newAgentPaneFixture(t, "apj", "1")
	if err := f.p.Start(context.Background(), "gc-test-other", runtime.Config{Command: "sleep 300", WorkDir: f.workDir}); err != nil {
		t.Fatalf("Start other: %v", err)
	}
	foreign := f.mustRun(t, "list-panes", "-t", "=gc-test-other:", "-F", "#{pane_id}")
	f.mustRun(t, "new-session", "-d", "-s", "gc-test-keepalive", "sleep 300")
	f.mustRun(t, "join-pane", "-b", "-s", foreign, "-t", f.agent)
	agentWindow := f.mustRun(t, "display-message", "-p", "-t", f.agent, "#{window_id}")
	if got := f.mustRun(t, "display-message", "-p", "-t", foreign, "#{window_id}"); got != agentWindow {
		t.Fatalf("joined pane %s is in window %s, want the agent's %s (precondition)", foreign, got, agentWindow)
	}
	// Whether join-pane -b lists the joined pane first depends on the tmux
	// version, so put it first by id: the foreign pane must lead the list for
	// a first-pane resolver to mistake it for the agent.
	if first, _, _ := strings.Cut(f.mustRun(t, "list-panes", "-s", "-t", paneTarget(f.name), "-F", "#{pane_id}"), "\n"); first != foreign {
		f.mustRun(t, "swap-pane", "-s", f.agent, "-t", foreign)
	}
	first, _, _ := strings.Cut(f.mustRun(t, "list-panes", "-s", "-t", paneTarget(f.name), "-F", "#{pane_id}"), "\n")
	if first != foreign {
		t.Fatalf("first pane = %s, want the joined foreign pane %s (precondition)", first, foreign)
	}

	f.requireAgentPaneResolved(t)
	f.relaunchAndRequireAgentRespawned(t, foreign)
}

// A session created before the marker existed has no marked pane; it falls
// back to the first pane of the first window.
func TestProvider_UnmarkedSessionFallsBackToFirstPane(t *testing.T) {
	f := newAgentPaneFixture(t, "apu", "1")
	// A pre-marker session has neither the pane marker nor the session flag.
	f.mustRun(t, "set-option", "-p", "-u", "-t", f.agent, agentPaneOption)
	f.mustRun(t, "set-option", "-u", "-t", paneTarget(f.name), agentSessionOption)
	if got := f.mustRun(t, "display-message", "-p", "-t", f.agent, agentPaneMarkerFormat); got != "pane" {
		t.Fatalf("agent pane marker after unset = %q, want unmarked (precondition)", got)
	}
	human := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")

	f.requireAgentPaneResolved(t)
	f.relaunchAndRequireAgentRespawned(t, human)
}

// Input and capture paths reach the marked agent pane, not a human pane that
// runs the same CLI and is listed (and focused) first. Process detection alone
// would pick the human pane here: both panes run sh, a configured process name.
func TestProvider_InputPathsReachMarkedAgentPaneOverSameCLIHumanPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("api")
	p := NewProviderWithConfig(cfg)
	socketPath := namedSocketPath(cfg.SocketName)
	t.Cleanup(func() {
		if err := p.TeardownServer(); err != nil && !errors.Is(err, ErrNoServer) {
			t.Errorf("tear down test tmux server: %v", err)
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test tmux socket: %v", err)
		}
	})
	dir := t.TempDir()
	agentIn, humanIn := filepath.Join(dir, "agent-in"), filepath.Join(dir, "human-in")
	pane := func(label, in string) string {
		return fmt.Sprintf("sh -c 'echo %s-SCREEN; exec cat > %s'", label, in)
	}
	const name = "gc-test-input-paths"
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: pane("AGENT", agentIn),
		WorkDir: dir,
		Env:     map[string]string{"GT_PROCESS_NAMES": "sh,cat"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tm := p.tm
	agent := markedAgentPane(t, tm, name)
	// Wait for the agent pane's program before splitting: on a slow default
	// shell (zsh, tmux 3.2a) it may not have run yet, and its screen and
	// input are what this test reads.
	waitForValue(t, "agent pane screen", "true", func() string {
		out, _ := tm.run("capture-pane", "-p", "-t", agent)
		return strconv.FormatBool(strings.Contains(out, "AGENT-SCREEN"))
	})
	// The human's split goes first in the window (-b) and is focused.
	human, err := tm.run("split-window", "-b", "-P", "-F", "#{pane_id}", "-t", agent, pane("HUMAN", humanIn))
	if err != nil {
		t.Fatalf("split-window: %v", err)
	}
	if first, _ := tm.run("display-message", "-p", "-t", paneTarget(name), "#{pane_id}"); first != human {
		t.Fatalf("active pane = %q, want the human split %q (precondition)", first, human)
	}
	readFile := func(path string) string {
		b, _ := os.ReadFile(path)
		return string(b)
	}
	waitForValue(t, "human pane screen", "true", func() string {
		out, _ := tm.run("capture-pane", "-p", "-t", human)
		return strconv.FormatBool(strings.Contains(out, "HUMAN-SCREEN"))
	})

	if got, err := inputPaneID(tm, name); err != nil || got != agent {
		t.Errorf("nudge input pane = (%q, %v), want the marked %q", got, err, agent)
	}
	peek, err := p.Peek(name, 20)
	if err != nil || !strings.Contains(peek, "AGENT-SCREEN") || strings.Contains(peek, "HUMAN-SCREEN") {
		t.Errorf("Peek = (%q, %v), want the agent pane's screen", peek, err)
	}
	if err := tm.SendKeysDebounced(name, "debounced", 0); err != nil {
		t.Fatalf("SendKeysDebounced: %v", err)
	}
	if err := tm.SendKeysReplace(name, "replaced", 0); err != nil {
		t.Fatalf("SendKeysReplace: %v", err)
	}
	waitForValue(t, "agent pane input", "debounced\nreplaced\n", func() string { return readFile(agentIn) })
	if got := readFile(humanIn); got != "" {
		t.Errorf("human pane received %q, want nothing", got)
	}
}

// Idle detection and the approval check read the agent's pane, not a focused
// human pane. A human prompt ("❯ ", which is Claude's ready prefix and a p10k
// prompt line) on the active pane must not make a busy agent read idle, and a
// real approval prompt on the agent's pane must be seen with the human pane
// focused (#7657 review, mc-zndi7.101).
func TestProvider_IdleAndPendingReadAgentPaneNotFocusedHumanPrompt(t *testing.T) {
	f := newAgentPaneFixture(t, "api", "1")
	tm := f.p.tm
	show := func(pane, text string) {
		t.Helper()
		f.mustRun(t, "respawn-pane", "-k", "-t", pane, "printf '%b\\n' "+shellquote.Quote(text)+"; exec sleep 300")
	}
	human := f.mustRun(t, "split-window", "-P", "-F", "#{pane_id}", "-t", f.agent, "sleep 300")
	show(human, "❯ ")
	if active := f.mustRun(t, "display-message", "-p", "-t", paneTarget(f.name), "#{pane_id}"); active != human {
		t.Fatalf("active pane = %s, want the human pane %s (precondition)", active, human)
	}
	captured := func(pane, want string) {
		t.Helper()
		waitForValue(t, "pane "+pane+" shows "+want, "true", func() string {
			out, _ := tm.run("capture-pane", "-p", "-t", pane)
			return strconv.FormatBool(strings.Contains(out, want))
		})
	}
	captured(human, "❯")

	// The agent is busy.
	show(f.agent, "✻ Thinking… (12s · esc to interrupt)")
	captured(f.agent, "esc to interrupt")
	if idle, err := tm.SnapshotIdle(f.name); err != nil || idle {
		t.Errorf("SnapshotIdle = (%v, %v), want busy: the agent's pane shows its spinner", idle, err)
	}
	if err := tm.WaitForIdle(context.Background(), f.name, time.Second); !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("WaitForIdle = %v, want ErrIdleTimeout while the agent is busy", err)
	}

	// The agent waits on an approval prompt.
	show(f.agent, "● Read(file_path: /tmp/test.txt)\n\n This command requires approval\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again for: Read:*\n   3. No")
	captured(f.agent, "Do you want to proceed?")
	if pending, err := tm.Pending(f.name); err != nil || pending == nil {
		t.Errorf("Pending = (%v, %v), want the agent pane's approval prompt", pending, err)
	}
}

// A nudge never lands in a human pane when the agent pane cannot be named: a
// human killed the agent pane (the session lives on in theirs), or two panes
// claim to be the agent's. The nudge errors and the human pane receives
// nothing (#7677 review MF1).
func TestProvider_NudgeFailsClosedWithoutOneAgentPane(t *testing.T) {
	cases := []struct {
		name    string
		wantErr error
		setup   func(t *testing.T, f *agentPaneFixture, human string)
	}{
		{"agent pane killed", errAgentPaneGone, func(t *testing.T, f *agentPaneFixture, _ string) {
			f.mustRun(t, "kill-pane", "-t", f.agent)
		}},
		{"two marked panes", errAgentPaneAmbiguous, func(t *testing.T, f *agentPaneFixture, human string) {
			f.mustRun(t, "set-option", "-p", "-F", "-t", human, agentPaneOption, "#{session_id}")
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentPaneFixture(t, fmt.Sprintf("anf%d", i), "1")
			rec := filepath.Join(f.workDir, "human-input")
			human := f.mustRun(t, "split-window", "-b", "-P", "-F", "#{pane_id}", "-t", f.agent, "cat > "+rec)
			tc.setup(t, f, human)

			if err := f.p.tm.NudgeSession(f.name, "NUDGE-TEXT"); !errors.Is(err, tc.wantErr) {
				t.Errorf("NudgeSession = %v, want %v", err, tc.wantErr)
			}
			if err := f.p.tm.ClearInput(context.Background(), f.name, 0); !errors.Is(err, tc.wantErr) {
				t.Errorf("ClearInput = %v, want %v", err, tc.wantErr)
			}
			// A sentinel typed after the attempts arrives after any stray
			// keys, so once it lands the record is complete.
			f.mustRun(t, "send-keys", "-t", human, "-l", "SENTINEL")
			f.mustRun(t, "send-keys", "-t", human, "Enter")
			waitForValue(t, "human pane record", "SENTINEL", func() string {
				b, _ := os.ReadFile(rec)
				return strings.TrimSpace(string(b))
			})
		})
	}
}

func waitForMarker(t *testing.T, path, want string) {
	t.Helper()
	waitForValue(t, "marker "+strconv.Quote(path), want, func() string {
		b, _ := os.ReadFile(path)
		return strings.TrimSpace(string(b))
	})
}

// waitForValue polls read until it returns want, failing after 10s.
func waitForValue(t *testing.T, what, want string, read func() string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if last = read(); last == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s = %q, want %q (timed out)", what, last, want)
}

func TestProvider_RecyclesDeadPaneWithoutProcessNames(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-dead-pane-recycle"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 0.1",
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start first session: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		has, err := p.Tmux().HasSession(name)
		if err != nil {
			t.Fatalf("HasSession: %v", err)
		}
		if has && !p.IsRunning(name) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p.IsRunning(name) {
		t.Fatal("IsRunning stayed true after one-shot command exited")
	}

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 300",
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start after dead pane: %v", err)
	}
	if !p.IsRunning(name) {
		t.Fatal("session should be running after dead-pane recycle")
	}
}

// A session whose agent has exited stays listed (remain-on-exit) with an empty
// #{pane_pid} on tmux 3.8. Resolving its input pane and nudging it must not
// fail with "no panes found" in a populated pane list.
func TestProvider_NudgeResolvesDeadPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	// A dead pane never looks idle, so Nudge's best-effort idle wait would run
	// its whole default window before sending.
	cfg.NudgeIdleTimeout = time.Second
	p := NewProviderWithConfig(cfg)
	name := "gc-test-dead-pane-nudge"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: "sleep 0.1",
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitForValue(t, "session state", "dead", func() string {
		has, err := p.Tmux().HasSession(name)
		if err != nil {
			t.Fatalf("HasSession: %v", err)
		}
		if has && !p.IsRunning(name) {
			return "dead"
		}
		return "alive"
	})

	target, err := p.Tmux().agentInputTarget(name)
	if err != nil || target == "" {
		t.Fatalf("agentInputTarget = (%q, %v), want the dead agent pane", target, err)
	}
	if err := p.Nudge(name, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge to a dead agent pane: %v", err)
	}
}

func TestProviderObserveLivenessKeepsZombieShellVisible(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-zombie-shell-liveness"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	if err := p.Start(context.Background(), name, runtime.Config{
		Command:      "sh -c 'sleep 2; exec sh -i'",
		WorkDir:      t.TempDir(),
		ProcessNames: []string{"sleep"},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		obs := runtime.ObserveLiveness(p, name, nil)
		if obs.Running && !obs.Alive {
			if !p.IsRunning(name) {
				t.Fatalf("IsRunning = false, want true while zombie shell pane is still present")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	obs := runtime.ObserveLiveness(p, name, nil)
	t.Fatalf("ObserveLiveness() = %#v, want running zombie shell with dead process", obs)
}

func TestProvider_StartCanceledCleansUpSession(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	p := NewProviderWithConfig(cfg)
	name := "gc-test-adapter-canceled"
	_ = p.Stop(name)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := p.Start(ctx, name, runtime.Config{
		Command:           "sleep 300",
		WorkDir:           t.TempDir(),
		ProcessNames:      []string{"sleep"},
		ReadyPromptPrefix: "> ",
		ReadyDelayMs:      1,
	})
	if !errors.Is(err, context.Canceled) {
		_ = p.Stop(name)
		t.Fatalf("Start: got %v, want context canceled", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !p.IsRunning(name) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.Stop(name)
	t.Fatal("session should be cleaned up after canceled start")
}

// TestProvider_RelaunchWithholdsControllerTokenFromRespawnedPane is the respawn
// twin of the create-path pane-child test, and it covers the failure class that
// test structurally cannot see: a pane is started more than once, and only the
// FIRST start goes through NewSessionWithCommandAndEnv. Relaunch reaches the
// agent via respawn-pane, which takes no env argument, so the create path's
// `env -u` command prefix does not apply to it — the respawned agent inherits
// the tmux server's global environment, which still holds the controller's real
// token.
//
// Relaunch is deliberately driven WITHOUT Env here, matching the documented
// contract that env is provision-half and not re-passed: the withholding has to
// survive in the session environment on its own.
func TestProvider_RelaunchWithholdsControllerTokenFromRespawnedPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	const (
		tokenVar = "GC_CONTROLLER_TOKEN"
		token    = "super-secret-controller-token"
	)
	t.Setenv(tokenVar, token)

	// A socket unique to this test, so the tmux server it starts forks from THIS
	// process and its global environment carries the token — that server env is
	// the thing respawn-pane hands to the new process.
	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("rp")
	p := NewProviderWithConfig(cfg)
	name := "gc-test-relaunch-token-pin"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker")
	agentCmd := func(tag string) string {
		return fmt.Sprintf(`sh -c 'printf %%s "%s=[${%s-ABSENT}]" > %s; sleep 300'`, tag, tokenVar, marker)
	}

	if err := p.Start(context.Background(), name, runtime.Config{
		Command: agentCmd("created"),
		WorkDir: workDir,
		Env:     map[string]string{tokenVar: ""},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForMarker(t, marker, "created=[ABSENT]")

	if err := p.Relaunch(context.Background(), name, runtime.Config{
		Command: agentCmd("respawned"),
		WorkDir: workDir,
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitForMarker(t, marker, "respawned=[ABSENT]")

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if strings.Contains(string(got), token) {
		t.Fatalf("respawned pane received the controller token: %s", got)
	}
}

// The warm-box upgrade path. A box provisioned by a gc whose create path built
// only the one-shot `env -u` prefix carries no session-env marker, and a warm
// box is explicitly long-lived — without re-assertion at relaunch it would hand
// the respawned agent the real token for the rest of its life. The session here
// is created the old way on purpose: prefix, no marker.
func TestProvider_RelaunchRepinsControllerTokenInPreexistingWarmBox(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	const (
		tokenVar = "GC_CONTROLLER_TOKEN"
		token    = "super-secret-controller-token"
	)
	t.Setenv(tokenVar, token)

	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("rr")
	p := NewProviderWithConfig(cfg)
	name := "gc-test-relaunch-token-repin"
	_ = p.Stop(name)
	defer func() { _ = p.Stop(name) }()

	workDir := t.TempDir()
	marker := filepath.Join(workDir, "marker")
	agentCmd := func(tag string) string {
		return fmt.Sprintf(`sh -c 'printf %%s "%s=[${%s-ABSENT}]" > %s; sleep 300'`, tag, tokenVar, marker)
	}

	// Provision the way the pre-fix create path did: the withholding exists only
	// as a command prefix, never in the session environment.
	if err := p.Tmux().NewSessionWithCommand(name, workDir, "env -u "+tokenVar+" "+agentCmd("created")); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	waitForMarker(t, marker, "created=[ABSENT]")
	if _, err := p.Tmux().GetEnvironment(name, tokenVar); err == nil {
		t.Fatal("session env already carries a marker; this fixture must model a pre-fix warm box")
	}

	// Relaunch carries the pin in cfg.Env, as the reconciler does.
	if err := p.Relaunch(context.Background(), name, runtime.Config{
		Command: agentCmd("respawned"),
		WorkDir: workDir,
		Env:     map[string]string{tokenVar: ""},
	}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	waitForMarker(t, marker, "respawned=[ABSENT]")

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if strings.Contains(string(got), token) {
		t.Fatalf("respawned pane in a pre-fix warm box received the controller token: %s", got)
	}
}
