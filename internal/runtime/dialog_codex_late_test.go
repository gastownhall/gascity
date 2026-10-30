package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Screens captured from stock codex-cli 0.162.1 under a managed tmux session.
// Codex draws the composer first; the status line under it ("<model> · <dir>")
// appears only once startup has finished, after any startup dialog.
const (
	codex0162EarlyComposer = `  >_ OpenAI Codex (v0.162.1)
     ~/city

› Ask Codex to do anything
  ? for shortcuts`
	codex0162SettledComposer = `  >_ OpenAI Codex (v0.162.1)
     ~/city
  permissions: YOLO mode

› Ask Codex to do anything
  GPT-6.1-Sol default · ~/city
  ? for shortcuts`
	codex0162HookReview = `  Hooks need review
  4 hooks are new or changed.
  Hooks can run outside the sandbox after you trust them.
› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)
  enter confirm · esc skip`
	// Codex echoes a first turn delivered on its command line in the same frame
	// as its status line.
	codex0162SettledRateLimitEcho = `  >_ OpenAI Codex (v0.162.1)
     ~/city
  permissions: YOLO mode

› Commit the staged API change as "Add rate limiting endpoint (gc-58)".

› Ask Codex to do anything
  GPT-6.1-Sol default · ~/city
  ? for shortcuts`
	codex0162HookReviewUnderBanner = `  >_ OpenAI Codex (v0.162.1)
     ~/city

` + codex0162HookReview
	claudeComposer = `╭───────────────────────────────────────────╮
│ ✻ Welcome to Claude Code!                 │
╰───────────────────────────────────────────╯

────────────────────────────────────────────
❯
────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`
)

// fakeLateCodexPane models stock Codex 0.156+ startup as measured live on
// 0.162.1: the composer is drawn first (~0.5s) and "Hooks need review" replaces
// it 1.6-3.0s later. Down moves the menu cursor; Enter confirms its row.
type fakeLateCodexPane struct {
	mu        sync.Mutex
	screen    string
	cursor    int // 0 "Review hooks", 1 "Trust all and continue"
	sent      []string
	confirmed string
}

func (p *fakeLateCodexPane) peek(int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screen, nil
}

func (p *fakeLateCodexPane) show(screen string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.screen = screen
}

func (p *fakeLateCodexPane) sendKeys(keys ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, keys...)
	for _, k := range keys {
		if p.screen != codex0162HookReview {
			continue
		}
		switch k {
		case "Down":
			p.cursor++
		case "Enter":
			if p.cursor == 1 {
				p.confirmed = "trust"
			} else {
				p.confirmed = "review"
			}
			p.screen = codex0162SettledComposer
		}
	}
	return nil
}

// dialogLogCapture records the messages logged through dialogLogf.
type dialogLogCapture struct {
	mu   sync.Mutex
	logs []string
}

// captureDialogLog swaps dialogLogf for a recorder for the rest of the test.
func captureDialogLog(t *testing.T) *dialogLogCapture {
	t.Helper()
	c := &dialogLogCapture{}
	old := dialogLogf
	dialogLogf = func(format string, args ...any) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.logs = append(c.logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { dialogLogf = old })
	return c
}

func (c *dialogLogCapture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.logs...)
}

// TestAcceptStartupDialogsAnswersCodexHookReviewDrawnAfterComposer pins #6464.
// Codex draws its composer before "Hooks need review", and the composer's "› "
// prompt is the same signal the handlers read as "no dialog is coming". A
// managed session must not park at a dialog that arrives after that prompt.
func TestAcceptStartupDialogsAnswersCodexHookReviewDrawnAfterComposer(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{}
	clock.after(500*time.Millisecond, func() { pane.show(codex0162EarlyComposer) })
	clock.after(2200*time.Millisecond, func() { pane.show(codex0162HookReview) })

	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if pane.confirmed != "trust" || !reflect.DeepEqual(pane.sent, []string{"Down", "Enter"}) {
		t.Fatalf("hook review drawn after the composer: sent=%v confirmed=%q, want [Down Enter] trust", pane.sent, pane.confirmed)
	}
}

// TestAcceptStartupDialogsReturnsPromptlyForSettledCodex: a Codex pane that has
// finished startup (status line drawn) costs no extra wait.
func TestAcceptStartupDialogsReturnsPromptlyForSettledCodex(t *testing.T) {
	clock := useSteppedDialogClock(t)
	logs := captureDialogLog(t)
	pane := &fakeLateCodexPane{screen: codex0162SettledComposer}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > time.Second {
		t.Fatalf("settled Codex composer took %v of virtual time", elapsed)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v with no dialog on screen", pane.sent)
	}
	if got := logs.lines(); len(got) != 0 {
		t.Fatalf("logged %q for a settled Codex composer", got)
	}
}

// TestAcceptStartupDialogsStopsWatchingWhenCodexSettles: the common start, a
// composer that settles without a dialog, ends the watch at the status line
// instead of waiting out the timeout.
func TestAcceptStartupDialogsStopsWatchingWhenCodexSettles(t *testing.T) {
	clock := useSteppedDialogClock(t)
	logs := captureDialogLog(t)
	pane := &fakeLateCodexPane{}
	clock.after(500*time.Millisecond, func() { pane.show(codex0162EarlyComposer) })
	clock.after(2200*time.Millisecond, func() { pane.show(codex0162SettledComposer) })
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > 3*time.Second {
		t.Fatalf("Codex settling at 2.2s took %v of virtual time", elapsed)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v with no dialog on screen", pane.sent)
	}
	if got := logs.lines(); len(got) != 0 {
		t.Fatalf("logged %q for a Codex composer that settled", got)
	}
}

// TestAcceptStartupDialogsAnswersCodexDialogOnce: a dialog the ordered pass
// has answered is not answered again by the watch, even when it stays on
// screen under the Codex banner.
func TestAcceptStartupDialogsAnswersCodexDialogOnce(t *testing.T) {
	useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: codex0162HookReviewUnderBanner}
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if !reflect.DeepEqual(pane.sent, []string{"Down", "Enter"}) {
		t.Fatalf("hook review left on screen: sent=%v, want one [Down Enter]", pane.sent)
	}
}

// TestAcceptStartupDialogsIgnoresRateLimitEchoInSettledCodex: a first turn
// that mentions rate limits, echoed once Codex settles, is the session's
// prompt, not a rate-limit dialog, so no keys land in the working session.
func TestAcceptStartupDialogsIgnoresRateLimitEchoInSettledCodex(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: codex0162EarlyComposer}
	clock.after(2200*time.Millisecond, func() { pane.show(codex0162SettledRateLimitEcho) })
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v to a settled Codex whose first turn mentions rate limits", pane.sent)
	}
}

func TestContainsRateLimitDialogIgnoresSettledCodex(t *testing.T) {
	t.Parallel()
	if ContainsRateLimitDialog(codex0162SettledRateLimitEcho) {
		t.Error("ContainsRateLimitDialog matched a settled Codex pane's first turn")
	}
	starting := strings.Replace(codex0162SettledRateLimitEcho, "  GPT-6.1-Sol default · ~/city\n", "", 1)
	if !ContainsRateLimitDialog(starting) {
		t.Error("ContainsRateLimitDialog stopped matching a Codex pane that is still starting")
	}
}

// TestAcceptStartupDialogsBoundsCodexStartupWatch: a Codex pane that never
// shows its status line is watched no longer than the dialog timeout.
func TestAcceptStartupDialogsBoundsCodexStartupWatch(t *testing.T) {
	clock := useSteppedDialogClock(t)
	logs := captureDialogLog(t)
	pane := &fakeLateCodexPane{screen: codex0162EarlyComposer}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > 2*dialogPollTimeout {
		t.Fatalf("unsettled Codex composer was watched for %v", elapsed)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v with no dialog on screen", pane.sent)
	}
	// The wait is the operator-visible cost of a Codex that never draws its
	// status line, so it is logged once, naming the missing signal.
	if got := logs.lines(); len(got) != 1 || !strings.Contains(got[0], "status line") {
		t.Fatalf("logged %q, want one line naming the missing status line", got)
	}
}

// TestAcceptStartupDialogsDoesNotDelayNonCodexPrompt: only a Codex pane waits
// for late dialogs; a Claude composer returns as soon as it is ready.
func TestAcceptStartupDialogsDoesNotDelayNonCodexPrompt(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: claudeComposer}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > time.Second {
		t.Fatalf("Claude composer took %v of virtual time; the late-dialog watch must be Codex-only", elapsed)
	}
}

// TestAcceptStartupDialogsFromStreamEndsAtStartingCodexComposer: a stream
// cannot wait for the dialog Codex draws after its composer, so it ends there
// and reports false, sending the caller to AcceptStartupDialogs, whose peek
// watch does wait. Later snapshots are drained so the sender never blocks.
func TestAcceptStartupDialogsFromStreamEndsAtStartingCodexComposer(t *testing.T) {
	t.Parallel()
	// The source stays open until cleanup, as a live watcher's does.
	snapshots := make(chan string)
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for _, frame := range []string{codex0162EarlyComposer, codex0162SettledComposer, codex0162SettledComposer} {
			select {
			case snapshots <- frame:
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-drained
		close(snapshots)
	})
	var sent []string
	start := time.Now()
	observed, err := AcceptStartupDialogsFromStreamWithStatus(context.Background(), 10*time.Second, snapshots,
		func(keys ...string) error {
			sent = append(sent, keys...)
			return nil
		})
	if err != nil {
		t.Fatalf("AcceptStartupDialogsFromStreamWithStatus: %v", err)
	}
	if observed {
		t.Fatal("stream reported a Codex composer that is still starting as observed; its late dialog would park the session")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stream took %v to end at the starting Codex composer", elapsed)
	}
	if len(sent) != 0 {
		t.Fatalf("sent %v with no dialog in the stream", sent)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("sender blocked on a stream that ended at the starting Codex composer")
	}
}

// TestAcceptStartupDialogsFromStreamObservesSettledCodex: a Codex composer
// with its status line is readiness, so the stream needs no peek fallback.
func TestAcceptStartupDialogsFromStreamObservesSettledCodex(t *testing.T) {
	t.Parallel()
	for name, screen := range map[string]string{
		"settled composer":         codex0162SettledComposer,
		"first turn on rate limit": codex0162SettledRateLimitEcho,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snapshots := make(chan string, 1)
			snapshots <- screen
			close(snapshots)
			var sent []string
			observed, err := AcceptStartupDialogsFromStreamWithStatus(context.Background(), 10*time.Second, snapshots,
				func(keys ...string) error {
					sent = append(sent, keys...)
					return nil
				})
			if err != nil {
				t.Fatalf("AcceptStartupDialogsFromStreamWithStatus: %v", err)
			}
			if !observed {
				t.Fatal("stream did not observe a settled Codex composer")
			}
			if len(sent) != 0 {
				t.Fatalf("sent %v to a settled Codex composer", sent)
			}
		})
	}
}
