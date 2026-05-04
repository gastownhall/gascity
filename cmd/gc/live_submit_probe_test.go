//go:build liveprobe

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func preferRealBDOnPath(t *testing.T) {
	t.Helper()
	skipSlowCmdGCTest(t, "requires a live bd-managed session probe; run make test-cmd-gc-process for full coverage")

	currentPath := os.Getenv("PATH")
	pathEntries := filepath.SplitList(currentPath)
	for _, dir := range pathEntries {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "bd")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		cmd := exec.Command(candidate, "update", "--help")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Setenv("PATH", dir+string(os.PathListSeparator)+currentPath)
			return
		}
		if strings.Contains(string(out), `unknown subcommand "update"`) {
			continue
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+currentPath)
		return
	}

	t.Fatalf("no real bd binary found on PATH")
}

func resolveLiveProbeSessionID(cityPath string, cfg *config.City, store beads.Store, target, sessionID string) (string, error) {
	if sessionID != "" {
		return sessionID, nil
	}
	id, err := resolveSessionIDMaterializingNamed(cityPath, cfg, store, target)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	return ensureSessionIDForTemplateWithOptions(cityPath, cfg, store, target, nil, ensureSessionForTemplateOptions{})
}

func TestLiveClaudeInterruptNow(t *testing.T) {
	preferRealBDOnPath(t)

	cityPath := os.Getenv("GC_LIVE_CITY")
	if cityPath == "" {
		cityPath = "/tmp/gc-claude-it"
	}
	target := os.Getenv("GC_LIVE_TARGET")
	if target == "" {
		target = "mayor"
	}
	sessionID := os.Getenv("GC_LIVE_SESSION_ID")

	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityPath, err)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityPath, err)
	}
	sp, err := newSessionProviderByName(cfg.Session.Provider, cfg.Session, cfg.Workspace.Name, cityPath)
	if err != nil {
		t.Fatalf("newSessionProviderByName: %v", err)
	}
	mgr := newSessionManagerWithConfig(cityPath, store, sp, cfg)
	id, err := resolveLiveProbeSessionID(cityPath, cfg, store, target, sessionID)
	if err != nil {
		t.Fatalf("resolveLiveProbeSessionID(%q): %v", target, err)
	}
	info, err := mgr.Get(id)
	if err != nil {
		t.Fatalf("mgr.Get(%q): %v", id, err)
	}
	resumeCmd, hints := buildResumeCommand(t.TempDir(), cfg, info, "", io.Discard)
	socket := cfg.Session.Socket
	if socket == "" {
		socket = cfg.Workspace.Name
	}

	// Best-effort reset to an idle prompt before the probe.
	_ = tmuxSendKeys(socket, target, "C-c")
	_ = waitForPane(socket, target, 10*time.Second, func(text string) bool {
		return strings.Contains(text, "\n❯")
	})

	base := fmt.Sprintf("%d", time.Now().UnixNano())
	firstToken := "GC_LIVE_CLAUDE_1_" + base
	secondToken := "GC_LIVE_CLAUDE_2_" + base

	firstMessage := fmt.Sprintf("Use Bash to run sleep 20. After it finishes, reply with %s and nothing else.", firstToken)
	if _, err := mgr.Submit(context.Background(), id, firstMessage, resumeCmd, hints, session.SubmitIntentDefault); err != nil {
		t.Fatalf("first Submit(default): %v", err)
	}
	time.Sleep(4 * time.Second)

	secondMessage := fmt.Sprintf("Reply with %s and nothing else.", secondToken)
	if _, err := mgr.Submit(context.Background(), id, secondMessage, resumeCmd, hints, session.SubmitIntentInterruptNow); err != nil {
		t.Fatalf("second Submit(interrupt_now): %v", err)
	}

	if err := waitForPane(socket, target, 90*time.Second, func(text string) bool {
		return strings.Contains(text, "\n● "+secondToken)
	}); err != nil {
		t.Fatalf("waiting for second token %q: %v", secondToken, err)
	}

	pane, err := capturePane(socket, target, 220)
	if err != nil {
		t.Fatalf("capturePane(final): %v", err)
	}
	if strings.Contains(pane, "\n● "+firstToken) {
		t.Fatalf("saw first completion token after interrupt_now:\n%s", pane)
	}
}

func TestLiveGeminiSubmitIntents(t *testing.T) {
	preferRealBDOnPath(t)

	cityPath := os.Getenv("GC_LIVE_CITY")
	if cityPath == "" {
		cityPath = "/tmp/gc-gemini-it"
	}
	target := os.Getenv("GC_LIVE_TARGET")
	if target == "" {
		target = "mayor"
	}
	sessionID := os.Getenv("GC_LIVE_SESSION_ID")

	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityPath, err)
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityPath, err)
	}
	sp, err := newSessionProviderByName(cfg.Session.Provider, cfg.Session, cfg.Workspace.Name, cityPath)
	if err != nil {
		t.Fatalf("newSessionProviderByName: %v", err)
	}
	mgr := newSessionManagerWithConfig(cityPath, store, sp, cfg)
	id, err := resolveLiveProbeSessionID(cityPath, cfg, store, target, sessionID)
	if err != nil {
		t.Fatalf("resolveLiveProbeSessionID(%q): %v", target, err)
	}
	info, err := mgr.Get(id)
	if err != nil {
		t.Fatalf("mgr.Get(%q): %v", id, err)
	}
	resumeCmd, hints := buildResumeCommand(t.TempDir(), cfg, info, "", io.Discard)
	socket := cfg.Session.Socket
	if socket == "" {
		socket = cfg.Workspace.Name
	}

	isGeminiPrompt := func(text string) bool {
		return strings.Contains(text, "Type your message or @path/to/file")
	}
	isGeminiToken := func(token string) func(string) bool {
		return func(text string) bool {
			return paneHasTrimmedLine(text, "✦ "+token)
		}
	}

	_ = tmuxSendKeys(socket, target, "Escape")
	_ = waitForPane(socket, target, 10*time.Second, isGeminiPrompt)

	base := fmt.Sprintf("%d", time.Now().UnixNano())

	idleToken := "GC_LIVE_GEM_IDLE_" + base
	if _, err := mgr.Submit(context.Background(), id, fmt.Sprintf("Reply with %s and nothing else.", idleToken), resumeCmd, hints, session.SubmitIntentDefault); err != nil {
		t.Fatalf("idle Submit(default): %v", err)
	}
	if err := waitForPane(socket, target, 90*time.Second, isGeminiToken(idleToken)); err != nil {
		t.Fatalf("waiting for idle token %q: %v", idleToken, err)
	}

	defaultDone := "GC_LIVE_GEM_DEFAULT_DONE_" + base
	defaultSecond := "GC_LIVE_GEM_DEFAULT_2_" + base
	if _, err := mgr.Submit(context.Background(), id, geminiBusyTurnPrompt("default", 180, defaultDone), resumeCmd, hints, session.SubmitIntentDefault); err != nil {
		t.Fatalf("busy Submit(default first): %v", err)
	}
	time.Sleep(1 * time.Second)
	if _, err := mgr.Submit(context.Background(), id, fmt.Sprintf("Reply with %s and nothing else.", defaultSecond), resumeCmd, hints, session.SubmitIntentDefault); err != nil {
		t.Fatalf("busy Submit(default second): %v", err)
	}
	if err := waitForPane(socket, target, 180*time.Second, func(text string) bool {
		return paneHasTrimmedLine(text, defaultDone)
	}); err != nil {
		t.Fatalf("waiting for default completion marker %q: %v", defaultDone, err)
	}
	if err := waitForPane(socket, target, 90*time.Second, isGeminiToken(defaultSecond)); err != nil {
		t.Fatalf("waiting for default second token %q: %v", defaultSecond, err)
	}
	pane, err := capturePane(socket, target, 320)
	if err != nil {
		t.Fatalf("capturePane(default): %v", err)
	}
	if !paneHasTrimmedLine(pane, defaultDone) || !paneHasTrimmedLine(pane, "✦ "+defaultSecond) {
		t.Fatalf("default path missing tokens:\n%s", pane)
	}
	if paneTrimmedLineIndex(pane, "✦ "+defaultSecond) < paneTrimmedLineIndex(pane, defaultDone) {
		t.Fatalf("default busy submit completed out of order:\n%s", pane)
	}

	followUpDone := "GC_LIVE_GEM_FOLLOW_DONE_" + base
	followUpSecond := "GC_LIVE_GEM_FOLLOW_2_" + base
	if _, err := mgr.Submit(context.Background(), id, geminiBusyTurnPrompt("follow", 180, followUpDone), resumeCmd, hints, session.SubmitIntentDefault); err != nil {
		t.Fatalf("follow_up Submit(default first): %v", err)
	}
	time.Sleep(1 * time.Second)
	outcome, err := mgr.Submit(context.Background(), id, fmt.Sprintf("Reply with %s and nothing else.", followUpSecond), resumeCmd, hints, session.SubmitIntentFollowUp)
	if err != nil {
		t.Fatalf("follow_up Submit(follow_up second): %v", err)
	}
	if !outcome.Queued {
		t.Fatalf("follow_up outcome = %#v, want queued", outcome)
	}
	if err := waitForPane(socket, target, 180*time.Second, func(text string) bool {
		return paneHasTrimmedLine(text, followUpDone)
	}); err != nil {
		t.Fatalf("waiting for follow_up completion marker %q: %v", followUpDone, err)
	}
	if err := waitForPane(socket, target, 90*time.Second, isGeminiToken(followUpSecond)); err != nil {
		t.Fatalf("waiting for follow_up second token %q: %v", followUpSecond, err)
	}
	pane, err = capturePane(socket, target, 360)
	if err != nil {
		t.Fatalf("capturePane(follow_up): %v", err)
	}
	if !paneHasTrimmedLine(pane, followUpDone) || !paneHasTrimmedLine(pane, "✦ "+followUpSecond) {
		t.Fatalf("follow_up path missing tokens:\n%s", pane)
	}
	if paneTrimmedLineIndex(pane, "✦ "+followUpSecond) < paneTrimmedLineIndex(pane, followUpDone) {
		t.Fatalf("follow_up completed out of order:\n%s", pane)
	}

	interruptDone := "GC_LIVE_GEM_INTERRUPT_DONE_" + base
	interruptSecond := "GC_LIVE_GEM_INTERRUPT_2_" + base
	if _, err := mgr.Submit(context.Background(), id, geminiBusyTurnPrompt("interrupt", 180, interruptDone), resumeCmd, hints, session.SubmitIntentDefault); err != nil {
		t.Fatalf("interrupt_now Submit(default first): %v", err)
	}
	time.Sleep(1 * time.Second)
	if _, err := mgr.Submit(context.Background(), id, fmt.Sprintf("Reply with %s and nothing else.", interruptSecond), resumeCmd, hints, session.SubmitIntentInterruptNow); err != nil {
		t.Fatalf("interrupt_now Submit(interrupt_now second): %v", err)
	}
	if err := waitForPane(socket, target, 90*time.Second, isGeminiToken(interruptSecond)); err != nil {
		t.Fatalf("waiting for interrupt second token %q: %v", interruptSecond, err)
	}
	pane, err = capturePane(socket, target, 420)
	if err != nil {
		t.Fatalf("capturePane(interrupt_now): %v", err)
	}
	if paneHasTrimmedLine(pane, interruptDone) {
		t.Fatalf("saw interrupted first token after interrupt_now:\n%s", pane)
	}
}

func geminiBusyTurnPrompt(label string, count int, completionMarker string) string {
	if count <= 0 {
		count = 1
	}
	base := fmt.Sprintf(
		"Write exactly %d numbered lines of the form '%s line N'. Do not use code fences or extra commentary.",
		count,
		label,
	)
	if completionMarker == "" {
		return base
	}
	return fmt.Sprintf(
		"Write exactly %d numbered lines of the form '%s line N'. After the numbered lines, write one final line exactly %s and nothing after it. Do not use code fences or extra commentary.",
		count,
		label,
		completionMarker,
	)
}

func paneHasTrimmedLine(text, want string) bool {
	return paneTrimmedLineIndex(text, want) >= 0
}

func paneTrimmedLineIndex(text, want string) int {
	for i, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == want {
			return i
		}
	}
	return -1
}

func waitForPane(socket, target string, timeout time.Duration, predicate func(string) bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		text, err := capturePane(socket, target, 220)
		if err == nil && predicate(text) {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

func capturePane(socket, target string, lines int) (string, error) {
	cmd := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", target, "-S", fmt.Sprintf("-%d", lines))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func tmuxSendKeys(socket, target string, keys ...string) error {
	args := []string{"-L", socket, "send-keys", "-t", target}
	args = append(args, keys...)
	cmd := exec.Command("tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
