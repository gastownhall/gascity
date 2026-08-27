//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func TestTmuxConformance(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	// The conformance fixture is a generic long-running command, not an agent
	// TUI with an observable idle prompt. Keep a short real timeout so the
	// Provider.Nudge wait/fallback branch stays covered without consuming the
	// production 30-second budget.
	cfg.NudgeIdleTimeout = 250 * time.Millisecond
	// Exercise the production construction path so one real tmux suite covers
	// both the Provider contract and the seam-backed cut-over.
	p := NewSeamBackedWithConfig(cfg)
	var counter int64

	runtimetest.RunProviderTestsWithOptions(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-test-conform-%d", id)
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, name
	}, runtimetest.Options{
		SkipStartError: func(err error) (string, bool) {
			if errors.Is(err, ErrServerDegraded) {
				return fmt.Sprintf("tmux test socket degraded before Start could run: %v", err), true
			}
			return "", false
		},
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
	if _, err := p.tm.run("select-window", "-t", "="+name+":^"); err != nil {
		t.Fatalf("reselect input window after copy-mode setup: %v", err)
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
	if _, err := p.tm.run("link-window", "-s", "="+name+":0", "-t", "="+observer); err != nil {
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
	if processAlive(secondPanePID) {
		t.Fatalf("certified stop left stubborn second-pane process %s alive", secondPanePID)
	}

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

func TestProviderNudgeFencesCopyModeAndTokenAcrossRealNamedSocket(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gctest-nudge-copy-fence-%d-%d", os.Getpid(), time.Now().UnixNano())
	cfg.NudgeIdleTimeout = 0
	p := NewProviderWithConfig(cfg)
	name := "nudge-copy-fence"
	socketPath := namedSocketPath(cfg.SocketName)
	defaultPath := namedSocketPath("default")
	defaultBefore, defaultBeforeErr := os.Lstat(defaultPath)
	t.Cleanup(func() {
		if err := p.TeardownServer(); err != nil && !errors.Is(err, ErrNoServer) {
			t.Errorf("tear down exact named tmux server: %v", err)
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove exact named tmux socket: %v", err)
		}
	})

	marker := filepath.Join(t.TempDir(), "nudge-marker")
	message := fmt.Sprintf("copy-fence-marker-%d", time.Now().UnixNano())
	signal := fmt.Sprintf("gc-nudge-copy-fence-%d", time.Now().UnixNano())
	const instanceToken = "original-instance-token"
	command := "while IFS= read -r line; do printf '%s\\n' \"$line\" >> " + shellquote.Quote(marker) + "; tmux -L " + shellquote.Quote(cfg.SocketName) + " wait-for -S " + shellquote.Quote(signal) + "; done"
	if err := p.Start(context.Background(), name, runtime.Config{
		Command:      command,
		ProviderName: "codex",
		Env:          map[string]string{"GC_INSTANCE_TOKEN": instanceToken},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	secondPane, err := p.tm.run("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "="+name, "sleep 300")
	if err != nil {
		t.Fatalf("create second-window pane: %v", err)
	}
	if !wellFormedTmuxID(secondPane, '%') {
		t.Fatalf("second-window pane ID = %q, want %%<digits>", secondPane)
	}
	if _, err := p.tm.run("select-window", "-t", "="+name+":^"); err != nil {
		t.Fatalf("select input window: %v", err)
	}
	deliveredLines := func(what string) []string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		defer cancel()
		if _, err := p.tm.runCtx(ctx, "wait-for", signal); err != nil {
			t.Fatalf("wait for %s delivery: %v", what, err)
		}
		data, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("read nudge marker after %s: %v", what, err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	paneInMode := func(pane string) string {
		t.Helper()
		mode, err := p.tm.run("display-message", "-t", pane, "-p", "#{pane_in_mode}")
		if err != nil {
			t.Fatalf("read copy mode for pane %s: %v", pane, err)
		}
		return strings.TrimSpace(mode)
	}

	// A park on a pane the nudge is not typing into cannot swallow it: tmux
	// routes paste-buffer and send-keys to the pane we name. Refusing here would
	// let one scrolled-back side pane silence the agent for as long as the park
	// lasts — and a wheel-up park outlives the human detaching.
	if _, err := p.tm.run("copy-mode", "-t", secondPane); err != nil {
		t.Fatalf("enter copy mode on second-window pane: %v", err)
	}
	unrelatedMessage := message + "-unrelated-park"
	if err := p.Nudge(name, runtime.TextContent(unrelatedMessage)); err != nil {
		t.Fatalf("Nudge with an unrelated pane parked in copy mode = %v, want delivery", err)
	}
	if got := deliveredLines("unrelated park"); len(got) != 1 || got[0] != unrelatedMessage {
		t.Fatalf("nudge marker = %v, want [%q]", got, unrelatedMessage)
	}
	// The other pane keeps its scrollback: only the pane we type into is cleared.
	if got := paneInMode(secondPane); got != "1" {
		t.Fatalf("unrelated pane copy mode = %q after delivery, want it undisturbed", got)
	}
	if _, err := p.tm.run("send-keys", "-t", secondPane, "-X", "cancel"); err != nil {
		t.Fatalf("cancel copy mode on exact second pane: %v", err)
	}
	if _, err := p.tm.run("select-window", "-t", "="+name+":^"); err != nil {
		t.Fatalf("reselect input window after copy-mode cancel: %v", err)
	}
	inputPane, err := p.tm.run("display-message", "-t", name, "-p", "#{pane_id}")
	if err != nil || !wellFormedTmuxID(inputPane, '%') {
		t.Fatalf("resolve exact input pane: pane=%q err=%v", inputPane, err)
	}

	// ga-c4w: the target pane's own park swallows every keystroke, and it
	// survives the human detaching, so the nudge path force-exits copy mode on
	// the pane it is about to type into and delivers.
	if _, err := p.tm.run("copy-mode", "-t", inputPane); err != nil {
		t.Fatalf("park the input pane in copy mode: %v", err)
	}
	parkedMessage := message + "-parked-target"
	if err := p.NudgeNow(name, runtime.TextContent(parkedMessage)); err != nil {
		t.Fatalf("NudgeNow on a copy-mode-parked target = %v, want cancel-and-deliver", err)
	}
	if got := deliveredLines("parked target"); len(got) != 2 || got[1] != parkedMessage {
		t.Fatalf("nudge marker = %v, want %q appended", got, parkedMessage)
	}
	if got := paneInMode(inputPane); got != "0" {
		t.Fatalf("input pane copy mode = %q after delivery, want it cleared", got)
	}

	if _, err := p.tm.run("set-hook", "-t", name, "window-resized", "copy-mode -t "+inputPane); err != nil {
		t.Fatalf("arm resize-to-copy-mode race: %v", err)
	}
	raceMessage := message + "-resize-race"
	raceErr := p.NudgeNow(name, runtime.TextContent(raceMessage))
	if _, err := p.tm.run("set-hook", "-u", "-t", name, "window-resized"); err != nil {
		t.Fatalf("disarm resize-to-copy-mode race: %v", err)
	}
	if !errors.Is(raceErr, runtime.ErrInputFenced) {
		t.Fatalf("NudgeNow when copy mode enters during guarded delay = %v, want ErrInputFenced", raceErr)
	}
	if data, err := os.ReadFile(marker); err != nil {
		t.Fatalf("read nudge marker after resize race: %v", err)
	} else if strings.Contains(string(data), raceMessage) {
		t.Fatalf("nudge marker = %q after resize race, want zero input", data)
	}
	if _, err := p.tm.run("send-keys", "-t", inputPane, "-X", "cancel"); err != nil {
		t.Fatalf("clear resize-race copy mode: %v", err)
	}
	if _, err := p.tm.run("set-hook", "-t", name, "window-resized", "set-environment -t ="+name+" GC_INSTANCE_TOKEN replacement-instance-token"); err != nil {
		t.Fatalf("arm resize-to-token-change race: %v", err)
	}
	tokenRaceMessage := message + "-token-race"
	tokenRaceErr := p.NudgeFenced(name, instanceToken, runtime.TextContent(tokenRaceMessage))
	if _, err := p.tm.run("set-hook", "-u", "-t", name, "window-resized"); err != nil {
		t.Fatalf("disarm resize-to-token-change race: %v", err)
	}
	if !errors.Is(tokenRaceErr, runtime.ErrInputFenced) {
		t.Fatalf("NudgeFenced when token changes during guarded delay = %v, want ErrInputFenced", tokenRaceErr)
	}
	if data, err := os.ReadFile(marker); err != nil {
		t.Fatalf("read nudge marker after token race: %v", err)
	} else if strings.Contains(string(data), tokenRaceMessage) {
		t.Fatalf("nudge marker = %q after token race, want zero input", data)
	}
	if err := p.SetMeta(name, "GC_INSTANCE_TOKEN", instanceToken); err != nil {
		t.Fatalf("restore exact instance token: %v", err)
	}

	// An attached client is visibility, not an input hazard: tmux still routes
	// pane-targeted input to the pane we named. Fencing on attachment would mean
	// watching a session stops the controller from talking to it, with no
	// recovery until the human detaches.
	other := NewProviderWithConfig(cfg)
	if err := other.tm.ensureHiddenAttachedClient(name); err != nil {
		t.Fatalf("attach independent PTY: %v", err)
	}
	attachedMessage := message + "-attached"
	if err := p.NudgeNow(name, runtime.TextContent(attachedMessage)); err != nil {
		t.Fatalf("NudgeNow with an attached PTY = %v, want delivery", err)
	}
	if got := deliveredLines("attached client"); len(got) != 3 || got[2] != attachedMessage {
		t.Fatalf("nudge marker = %v, want %q appended", got, attachedMessage)
	}
	other.tm.CloseHiddenAttachClient(name)

	// The same window can be visible through another attached session while this
	// source session still reports detached. That is ownership we never
	// certified, so it stays fenced.
	linkedName := name + "-linked"
	if _, err := p.tm.run("new-session", "-d", "-s", linkedName, "sleep 300"); err != nil {
		t.Fatalf("create linked witness session: %v", err)
	}
	if _, err := p.tm.run("link-window", "-d", "-s", inputPane, "-t", "="+linkedName+":9"); err != nil {
		t.Fatalf("link input window into witness session: %v", err)
	}
	if _, err := p.tm.run("select-window", "-t", "="+linkedName+":9"); err != nil {
		t.Fatalf("select linked input window: %v", err)
	}
	linkedClient := NewProviderWithConfig(cfg)
	if err := linkedClient.tm.ensureHiddenAttachedClient(linkedName); err != nil {
		t.Fatalf("attach client through linked session: %v", err)
	}
	linkedMessage := message + "-linked"
	if err := p.NudgeNow(name, runtime.TextContent(linkedMessage)); !errors.Is(err, runtime.ErrInputFenced) {
		t.Fatalf("NudgeNow through an attached linked session = %v, want ErrInputFenced", err)
	}
	if data, err := os.ReadFile(marker); err != nil {
		t.Fatalf("read nudge marker through linked session: %v", err)
	} else if strings.Contains(string(data), linkedMessage) {
		t.Fatalf("nudge marker = %q through linked session, want no delivery", data)
	}
	linkedClient.tm.CloseHiddenAttachClient(linkedName)
	if _, err := p.tm.run("kill-session", "-t", "="+linkedName); err != nil {
		t.Fatalf("remove linked witness session: %v", err)
	}
	if err := p.NudgeFenced(name, instanceToken, runtime.TextContent(message)); err != nil {
		t.Fatalf("NudgeFenced with matching token after fences clear: %v", err)
	}
	if got := deliveredLines("token-matched nudge"); len(got) != 4 || got[3] != message {
		t.Fatalf("nudge marker = %v, want %q appended", got, message)
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

func waitForMarker(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if last = strings.TrimSpace(string(b)); last == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("marker %q = %q, want %q (timed out)", path, last, want)
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
