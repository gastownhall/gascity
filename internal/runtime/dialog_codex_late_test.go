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
	// Codex 0.162.1 resuming a session (codex resume --last) draws its composer
	// with "Resuming session…" under the banner; the status line and the
	// resumed transcript replace it together once the resume finishes.
	codex0162ResumingComposer = `  >_ OpenAI Codex (v0.162.1)
     ~/city
  Resuming session…

› Ask Codex to do anything

  ? for shortcuts`
	// Codex 0.162.1's first frame on a fresh start shows "loading" in the
	// banner's directory slot.
	codex0162LoadingComposer = `  >_ OpenAI Codex (v0.162.1)
     loading

› Ask Codex to do anything

  ? for shortcuts`
	// Codex 0.156.1 shows loading placeholders in its boxed header.
	codex0156LoadingComposer = `╭───────────────────────────────────────╮
│ >_ OpenAI Codex (v0.156.1)            │
│                                       │
│ model:     loading   /model to change │
│ directory: loading                    │
╰───────────────────────────────────────╯

› Ask Codex to do anything`
	// Codex 0.162.1 on a fresh, never-trusted git folder, with Codex pinned to
	// a CPU shared with eight busy loops: the composer was drawn at 1.4s
	// ("loading" in the directory slot), the directory at 5.0s, and this
	// dialog replaced the composer at 31.8s. (The first-run logo art Codex
	// draws beside an empty composer is omitted.)
	codex0162TrustFolderDialog = `  Folder access
  /home/tester/city

  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings. Folder settings can run code automatically, even without a
  model request. Continue only if you trust these files. Your trust decision will be saved.

› 1. Trust and continue
  2. Back to Agent Command Center

  enter continue · esc back`
	// Codex 0.162.1 started with a first turn on its command line and a slow
	// MCP server: the status line is drawn first, then the turn's MCP boot
	// shows above the composer with "esc to interrupt".
	codex0162SettledFirstTurnMCPBoot = `  >_ OpenAI Codex (v0.162.1)
     ~/city
  permissions: YOLO mode

› Reply with just the word hi

• Starting MCP servers (1/3): codex_apps, slowprobe (0s • esc to interrupt)

› Ask Codex to do anything

  GPT-6.1-Sol default · ~/city
  ← for agents · ? for shortcuts`
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
		if p.screen == codex0162TrustFolderDialog {
			// "1. Trust and continue" is pre-selected; Enter confirms it.
			if k == "Enter" {
				p.confirmed = "trust folder"
				p.screen = codex0162SettledComposer
			}
			continue
		}
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

// codexStartupContext is a session start context with the default 60s
// [session] startup_timeout. The stepped clock never reaches its deadline;
// the watch reads only how much of it remains.
func codexStartupContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestAcceptStartupDialogsAnswersCodexTrustDialogAfterSlowStart pins the live
// 0.162.1 run on a loaded host: Codex drew its composer at 1.4s and finished
// loading at 5.0s, then replaced it with "Trust this folder?" at 31.8s, long
// after the 8s dialog timeout. Nothing on the unsettled composer says a dialog
// is still coming, so a session start watches it into the startup deadline
// and answers the dialog instead of parking the session.
func TestAcceptStartupDialogsAnswersCodexTrustDialogAfterSlowStart(t *testing.T) {
	clock := useSteppedDialogClock(t)
	logs := captureDialogLog(t)
	pane := &fakeLateCodexPane{}
	clock.after(1400*time.Millisecond, func() { pane.show(codex0162LoadingComposer) })
	clock.after(5000*time.Millisecond, func() { pane.show(codex0162EarlyComposer) })
	clock.after(31800*time.Millisecond, func() { pane.show(codex0162TrustFolderDialog) })

	if err := AcceptStartupDialogsWithTimeout(codexStartupContext(t), dialogPollTimeout, pane.peek, pane.sendKeys,
		WithSessionStartDeadline()); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if pane.confirmed != "trust folder" || !reflect.DeepEqual(pane.sent, []string{"Enter"}) {
		t.Fatalf("trust dialog drawn 26.8s after Codex loaded: sent=%v confirmed=%q, want [Enter] trust folder",
			pane.sent, pane.confirmed)
	}
	if got := logs.lines(); len(got) != 0 {
		t.Fatalf("logged %q for a start that ended in a dialog", got)
	}
}

// TestAcceptStartupDialogsAnswersCodexDialogAfterSlowResume: a resume or wake
// on a loaded host can keep Codex at "Resuming session…" past the dialog
// timeout, and a startup dialog drawn after it is still answered.
func TestAcceptStartupDialogsAnswersCodexDialogAfterSlowResume(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: codex0162ResumingComposer}
	clock.after(dialogPollTimeout+4*time.Second, func() { pane.show(codex0162HookReview) })

	if err := AcceptStartupDialogsWithTimeout(codexStartupContext(t), dialogPollTimeout, pane.peek, pane.sendKeys,
		WithSessionStartDeadline()); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if pane.confirmed != "trust" || !reflect.DeepEqual(pane.sent, []string{"Down", "Enter"}) {
		t.Fatalf("hook review drawn after a %v resume: sent=%v confirmed=%q, want [Down Enter] trust",
			dialogPollTimeout+4*time.Second, pane.sent, pane.confirmed)
	}
}

// TestAcceptStartupDialogsBoundsCodexStartupWatchByStartupDeadline: a Codex
// that never settles is watched past the dialog timeout but for no more than
// three quarters of what remains of the session's startup deadline, leaving
// the rest for readiness and the startup nudge.
func TestAcceptStartupDialogsBoundsCodexStartupWatchByStartupDeadline(t *testing.T) {
	for name, screen := range map[string]string{
		"0.162.1 loaded":   codex0162EarlyComposer,
		"0.162.1 resuming": codex0162ResumingComposer,
		"0.162.1 loading":  codex0162LoadingComposer,
		"0.156.1 loading":  codex0156LoadingComposer,
	} {
		t.Run(name, func(t *testing.T) {
			clock := useSteppedDialogClock(t)
			logs := captureDialogLog(t)
			pane := &fakeLateCodexPane{screen: screen}
			start := clock.Now()
			if err := AcceptStartupDialogsWithTimeout(codexStartupContext(t), dialogPollTimeout, pane.peek, pane.sendKeys,
				WithSessionStartDeadline()); err != nil {
				t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
			}
			elapsed := clock.Now().Sub(start)
			if elapsed < 40*time.Second {
				t.Fatalf("unsettled Codex was watched for only %v; a slow start outlasts the dialog timeout", elapsed)
			}
			if elapsed > 46*time.Second {
				t.Fatalf("unsettled Codex was watched for %v, past three quarters of a 60s startup deadline", elapsed)
			}
			if len(pane.sent) != 0 {
				t.Fatalf("sent %v with no dialog on screen", pane.sent)
			}
			if got := logs.lines(); len(got) != 1 || !strings.Contains(got[0], "status line") {
				t.Fatalf("logged %q, want one line naming the missing status line", got)
			}
		})
	}
}

// TestAcceptStartupDialogsSharesCodexStartupBudgetAcrossWatches: each late
// dialog starts another watch, and the watches share one startup-deadline
// budget rather than each taking a share of what is left.
func TestAcceptStartupDialogsSharesCodexStartupBudgetAcrossWatches(t *testing.T) {
	clock := useSteppedDialogClock(t)
	captureDialogLog(t)
	pane := &fakeLateCodexPane{screen: codex0162ResumingComposer}
	clock.after(dialogPollTimeout+4*time.Second, func() { pane.show(codex0162HookReview) })
	// After the dialog is answered Codex goes back to resuming and never
	// finishes.
	sendKeys := func(keys ...string) error {
		err := pane.sendKeys(keys...)
		if pane.confirmed != "" {
			pane.show(codex0162ResumingComposer)
		}
		return err
	}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(codexStartupContext(t), dialogPollTimeout, pane.peek, sendKeys,
		WithSessionStartDeadline()); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if pane.confirmed != "trust" {
		t.Fatalf("hook review after a slow resume: sent=%v confirmed=%q, want trust", pane.sent, pane.confirmed)
	}
	if elapsed := clock.Now().Sub(start); elapsed > 46*time.Second {
		t.Fatalf("watches of a Codex still resuming took %v together, past three quarters of a 60s startup deadline", elapsed)
	}
}

// TestAcceptStartupDialogsKeepsCodexWatchBoundOutsideSessionStart: only a
// session start spends the startup deadline. A call on a running session,
// such as the session layer's deferred check before a send, and a start with
// no deadline keep the dialog timeout bound.
func TestAcceptStartupDialogsKeepsCodexWatchBoundOutsideSessionStart(t *testing.T) {
	for name, tc := range map[string]struct {
		deadline bool
		opts     []StartupDialogOption
	}{
		"deadline, not a session start": {deadline: true},
		"session start, no deadline":    {opts: []StartupDialogOption{WithSessionStartDeadline()}},
	} {
		t.Run(name, func(t *testing.T) {
			clock := useSteppedDialogClock(t)
			captureDialogLog(t)
			pane := &fakeLateCodexPane{screen: codex0162ResumingComposer}
			ctx := context.Background()
			if tc.deadline {
				ctx = codexStartupContext(t)
			}
			start := clock.Now()
			if err := AcceptStartupDialogsWithTimeout(ctx, dialogPollTimeout, pane.peek, pane.sendKeys, tc.opts...); err != nil {
				t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
			}
			if elapsed := clock.Now().Sub(start); elapsed > 2*dialogPollTimeout {
				t.Fatalf("watched for %v, want the %v dialog timeout", elapsed, dialogPollTimeout)
			}
		})
	}
}

// TestAcceptStartupDialogsTreatsCodexFirstTurnMCPBootAsSettled pins the live
// 0.162.1 evidence: Codex boots MCP servers as part of a turn, after it has
// drawn its status line, so a status line under "Starting MCP servers … esc to
// interrupt" is a started Codex running its first turn. No startup dialog
// follows it, and the watch neither waits on it nor sends it keys.
func TestAcceptStartupDialogsTreatsCodexFirstTurnMCPBootAsSettled(t *testing.T) {
	clock := useSteppedDialogClock(t)
	logs := captureDialogLog(t)
	pane := &fakeLateCodexPane{screen: codex0162SettledFirstTurnMCPBoot}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(codexStartupContext(t), dialogPollTimeout, pane.peek, pane.sendKeys,
		WithSessionStartDeadline()); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > time.Second {
		t.Fatalf("Codex booting MCP for its first turn took %v of virtual time", elapsed)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v to a Codex running its first turn", pane.sent)
	}
	if got := logs.lines(); len(got) != 0 {
		t.Fatalf("logged %q for a started Codex", got)
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
