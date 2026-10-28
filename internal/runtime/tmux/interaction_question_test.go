package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// askUserQuestionPane is a live capture (Claude Code v2.1.280, fullscreen TUI)
// of an open AskUserQuestion dialog; header lines sanitized. The select footer
// is the last row on screen.
const askUserQuestionPane = ` ▐▛███▛█   Claude Code v2.1.280
▝▜██████▀  Sonnet 5 · Claude Max
  ▝▝ ▝▝    /home/user/projects/gc-management/.gc/worktrees/beads/deployer
❯ Check your hook and continue the deploy.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 ☐ be-tx30v
│ Push deploy/be-tx30v-gate to headfork and open the PR against gastownhall/beads. How do you want to proceed?
❯ 1. Run gates, then ship if clean (Recommended)
     Run gates, then ship if clean
  2. Run gates, ask again before push
     Run gates, ask again before push
  3. Keep holding
     Keep holding
  4. Type something.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  5. Chat about this
Enter to select · ↑/↓ to navigate · Esc to cancel
`

// answeredAskUserQuestionTail is what a pane shows once the dialog above it
// was answered: the frame stays in scrollback, the agent reports the answer,
// works, and the idle composer returns underneath.
const answeredAskUserQuestionTail = `
● User answered Claude's questions:
  · Push deploy/be-tx30v-gate ... → Run gates, then ship if clean (Recommended)

● Running the gates now.

────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────
  ? for shortcuts
`

// answeredAskUserQuestionPane is the dialog frame scrolled above an idle
// composer.
const answeredAskUserQuestionPane = askUserQuestionPane + answeredAskUserQuestionTail

// answeredAskUserQuestionWorkingPane is the same frame while the agent is still
// working on the answer: no composer yet, only output below the footer.
const answeredAskUserQuestionWorkingPane = askUserQuestionPane + `
● User answered Claude's questions:
  · Push deploy/be-tx30v-gate ... → Run gates, then ship if clean (Recommended)

● Running the gates now.
  ⎿  make check (esc to interrupt)
`

// Pane fragments for the matcher's negative space. The folder-trust pane is a
// reconstruction from the dialog's known footer, not a live capture.
const (
	askUserQuestionFooterSelect  = "Enter to select · ↑/↓ to navigate · Esc to cancel\n"
	askUserQuestionFooterConfirm = "Enter to confirm · Esc to cancel\n"
	askUserQuestionOptionRows    = "❯ 1. Yes, proceed\n  2. No, exit\n"
	askUserQuestionHeaderRows    = " ☐ be-tx30v\n│ Which approach?\n"
)

// paneExecutor answers capture-pane with pane, the #{pane_in_mode} probe with
// inMode and agent-pane lookups with the fake agent pane, and every other tmux
// call with "". It records every call.
type paneExecutor struct {
	calls  [][]string
	pane   string
	inMode string
}

func (e *paneExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	if slices.Contains(args, "#{pane_in_mode}") {
		return e.inMode, nil
	}
	if slices.Contains(args, "capture-pane") {
		return e.pane, nil
	}
	return "", nil
}

func (e *paneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// captures counts the pane captures the executor served.
func (e *paneExecutor) captures() int {
	n := 0
	for _, call := range e.calls {
		if slices.Contains(call, "capture-pane") {
			n++
		}
	}
	return n
}

// requireNoKeys fails when calls hold a send-keys, other than the copy-mode
// exit (send-keys -X cancel) when allowCancel is set. That exit types nothing.
func requireNoKeys(t *testing.T, calls [][]string, allowCancel bool) {
	t.Helper()
	for _, call := range calls {
		if !sendsKeys(call) {
			continue
		}
		if allowCancel && slices.Contains(call, "-X") && slices.Contains(call, "cancel") {
			continue
		}
		t.Fatalf("sent keys while an AskUserQuestion dialog owned the pane: %q", call)
	}
}

func questionTmux(pane string) (*Tmux, *paneExecutor) {
	ex := &paneExecutor{pane: pane, inMode: "0"}
	return &Tmux{cfg: DefaultConfig(), exec: ex}, ex
}

// An open AskUserQuestion owns the pane's input: a submitted Enter selects the
// focused option. Pending must report it, as a question with no response verbs,
// so guarded send paths refuse to type.
func TestProviderPendingReportsAskUserQuestionDialog(t *testing.T) {
	tm, ex := questionTmux(askUserQuestionPane)
	pending, err := (&Provider{tm: tm}).Pending("auq")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil {
		t.Fatal("Pending = nil for an open AskUserQuestion dialog; a nudge's Enter will answer it with option 1")
	}
	if pending.Kind != "question" {
		t.Errorf("Kind = %q, want %q", pending.Kind, "question")
	}
	if len(pending.Options) != 0 {
		t.Errorf("Options = %q, want none: Respond offers no verbs for a question", pending.Options)
	}
	if !strings.HasPrefix(pending.RequestID, "tmux-question-") {
		t.Errorf("RequestID = %q, want the tmux-question- prefix", pending.RequestID)
	}
	for _, want := range []string{"be-tx30v", "How do you want to proceed?"} {
		if !strings.Contains(pending.Prompt, want) {
			t.Errorf("Prompt = %q, want it to carry %q", pending.Prompt, want)
		}
	}
	if pending.Metadata["source"] != "tmux" {
		t.Errorf("Metadata[source] = %q, want tmux", pending.Metadata["source"])
	}
	if got := ex.captures(); got != 1 {
		t.Errorf("Pending took %d pane captures, want 1 per look", got)
	}

	again, err := (&Provider{tm: tm}).Pending("auq")
	if err != nil || again == nil || again.RequestID != pending.RequestID {
		t.Fatalf("second Pending = %+v, %v; want the same stable RequestID %q", again, err, pending.RequestID)
	}
}

// An answered frame in scrollback is not a pending interaction: only a dialog
// the pane still shows owns its input.
func TestProviderPendingIgnoresAnsweredAskUserQuestionDialog(t *testing.T) {
	for name, pane := range map[string]string{
		"idle composer below":  answeredAskUserQuestionPane,
		"agent working below":  answeredAskUserQuestionWorkingPane,
		"no dialog at all":     "● Done.\n\n❯ \n",
		"only the select line": askUserQuestionFooterSelect,
	} {
		t.Run(name, func(t *testing.T) {
			tm, _ := questionTmux(pane)
			pending, err := (&Provider{tm: tm}).Pending("auq")
			if err != nil || pending != nil {
				t.Fatalf("Pending = %+v, %v; want nothing for a pane with no live dialog", pending, err)
			}
			if err := tm.checkNoApprovalPrompt("auq"); err != nil {
				t.Fatalf("checkNoApprovalPrompt = %v; an answered dialog must not hold a nudge", err)
			}
		})
	}
}

// A pane has one input owner. An approval left above a live dialog is stale, so
// the question wins; a dialog answered above a live approval is stale, so the
// approval path is main's.
func TestProviderPendingPrecedenceBetweenQuestionAndApproval(t *testing.T) {
	t.Run("live question over a stale approval", func(t *testing.T) {
		tm, _ := questionTmux(approvalPromptPane() + "\n" + askUserQuestionPane)
		pending, err := (&Provider{tm: tm}).Pending("auq")
		if err != nil || pending == nil {
			t.Fatalf("Pending = %+v, %v; want the question", pending, err)
		}
		if pending.Kind != "question" || !strings.HasPrefix(pending.RequestID, "tmux-question-") {
			t.Fatalf("Pending = %+v, want the live question to win over the stale approval above it", pending)
		}
	})
	t.Run("live approval under an answered question", func(t *testing.T) {
		tm, _ := questionTmux(answeredAskUserQuestionPane + "\n" + approvalPromptPane())
		pending, err := (&Provider{tm: tm}).Pending("auq")
		if err != nil || pending == nil {
			t.Fatalf("Pending = %+v, %v; want the approval", pending, err)
		}
		if pending.Kind != "approval" || strings.HasPrefix(pending.RequestID, "tmux-question-") {
			t.Fatalf("Pending = %+v, want the approval path for an answered dialog above a live approval", pending)
		}
	})
}

// Every typed-nudge path (queue poller, claim backstops, drain reminders,
// warm-bind, mail notify, sling, gc nudge) funnels into nudgeSession. It must
// not paste or press Enter while an AskUserQuestion owns the pane, and it must
// say so in the one error every consumer classifies.
func TestNudgeSessionSendsNoKeysIntoAskUserQuestionDialog(t *testing.T) {
	for _, tc := range []struct {
		name        string
		inMode      string
		allowCancel bool
	}{
		{"pane not in copy-mode", "0", false},
		// Leaving copy-mode types nothing; no text, Enter, C-u or Escape may follow.
		{"pane parked in copy-mode", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &paneExecutor{pane: askUserQuestionPane, inMode: tc.inMode}
			tm := &Tmux{cfg: DefaultConfig(), exec: ex}
			err := tm.NudgeSession("auq", "<system-reminder>\n- [sling] Work slung. Check your hook.\n</system-reminder>\n")
			if !errors.Is(err, runtime.ErrPendingInteraction) {
				t.Fatalf("NudgeSession = %v, want runtime.ErrPendingInteraction", err)
			}
			requireNoKeys(t, ex.calls, tc.allowCancel)
		})
	}
}

// A dialog answered long ago must not hold every later nudge.
func TestNudgeSessionDeliversPastAnAnsweredAskUserQuestionDialog(t *testing.T) {
	ex := &paneExecutor{pane: answeredAskUserQuestionPane, inMode: "0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	err := tm.NudgeSession("auq", "hello")
	if errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeSession = %v; an answered dialog above an idle composer is not pending", err)
	}
	if callIndexWithTokens(ex.calls, "send-keys", "-l", "hello") < 0 {
		t.Fatalf("the message was never pasted; calls=%v", ex.calls)
	}
}

// Provider.Nudge and NudgeNow refuse before any key, and Nudge refuses before
// it spends the idle wait on a pane that cannot turn idle by itself.
func TestProviderNudgeRefusesAskUserQuestionDialog(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NudgeIdleTimeout = 5 * time.Second
	ex := &paneExecutor{pane: askUserQuestionPane, inMode: "0"}
	p := &Provider{tm: &Tmux{cfg: cfg, exec: ex}}

	start := time.Now()
	err := p.Nudge("auq", runtime.TextContent("hello"))
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("Nudge = %v, want runtime.ErrPendingInteraction", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Nudge took %s to refuse; it waited for idle instead of refusing at once", elapsed)
	}
	err = p.NudgeNow("auq", runtime.TextContent("hello"))
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow = %v, want runtime.ErrPendingInteraction", err)
	}
	requireNoKeys(t, ex.calls, false)
}

// A look that cannot read the pane fails closed: what it cannot see may be a
// dialog.
func TestNudgeSessionFailsClosedWhenTheQuestionLookCannotReadThePane(t *testing.T) {
	ex := &captureFailExecutor{paneExecutor: paneExecutor{pane: askUserQuestionPane, inMode: "0"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	err := tm.NudgeSession("auq", "hello")
	if err == nil {
		t.Fatal("NudgeSession = nil with an unreadable pane, want the look's refusal")
	}
	requireNoKeys(t, ex.calls, false)
}

// captureFailExecutor fails every capture-pane.
type captureFailExecutor struct{ paneExecutor }

func (e *captureFailExecutor) execute(args []string) (string, error) {
	if slices.Contains(args, "capture-pane") {
		e.calls = append(e.calls, slices.Clone(args))
		return "", errors.New("capture-pane: timed out")
	}
	return e.paneExecutor.execute(args)
}

func (e *captureFailExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// The operator text for a question dialog says it is answered in the terminal.
// It is not a permission prompt, and respond has nothing to offer.
func TestAskUserQuestionRefusalTextIsKindAware(t *testing.T) {
	tm, _ := questionTmux(askUserQuestionPane)
	err := tm.checkNoApprovalPrompt("auq")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("checkNoApprovalPrompt = %v, want runtime.ErrPendingInteraction", err)
	}
	text := err.Error()
	for _, banned := range []string{"permission prompt", "approve or deny", "respond"} {
		if strings.Contains(text, banned) {
			t.Errorf("refusal %q mentions %q, which does not apply to a question dialog", text, banned)
		}
	}
	for _, want := range []string{"question", "gc session attach"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q does not mention %q", text, want)
		}
	}
}

// The dialog's numbered options are per-dialog, not the fixed Yes/Yes-always/No
// menu Respond's approval switch encodes: Respond refuses, with the typed
// error the API maps to 409 invalid_interaction, before any key is sent. A
// refusal that already pressed "1" has answered the dialog, and one that
// returned nil would report a still-open dialog as answered.
func TestRespondRefusesAskUserQuestionDialog(t *testing.T) {
	for name, pane := range map[string]string{
		"bare dialog":                askUserQuestionPane,
		"dialog over stale approval": approvalPromptPane() + "\n" + askUserQuestionPane,
	} {
		t.Run(name, func(t *testing.T) {
			for _, action := range []string{"approve", "deny"} {
				ex := &paneExecutor{pane: pane, inMode: "1"}
				tm := &Tmux{cfg: DefaultConfig(), exec: ex}
				err := tm.Respond("auq", runtime.InteractionResponse{Action: action})
				if !errors.Is(err, runtime.ErrInteractionActionUnavailable) {
					t.Fatalf("Respond(%s) = %v, want runtime.ErrInteractionActionUnavailable", action, err)
				}
				requireNoKeys(t, ex.calls, false)
			}
		})
	}
}

// An open AskUserQuestion dialog is not an idle boundary even though its
// focused row ("❯ 1. …") matches the ready-prompt prefix. Like an open approval
// it reads indeterminate (no idle prompt, no busy indicator), never idle.
func TestSnapshotPaneIdleRejectsAskUserQuestionDialog(t *testing.T) {
	tm, _ := questionTmux(askUserQuestionPane)
	idle, err := tm.snapshotPaneIdleWithPrefix("auq", DefaultReadyPromptPrefix)
	if idle {
		t.Fatal("open AskUserQuestion dialog reads as an idle prompt; WaitForIdle returns nil and Provider.Nudge submits into it")
	}
	if !errors.Is(err, runtime.ErrIdleIndeterminate) {
		t.Fatalf("snapshotPaneIdleWithPrefix error = %v, want runtime.ErrIdleIndeterminate", err)
	}

	// PaneShowsIdlePrompt is shared with remote runtimes' captures.
	if PaneShowsIdlePrompt(strings.Split(askUserQuestionPane, "\n"), DefaultReadyPromptPrefix) {
		t.Fatal("PaneShowsIdlePrompt = true for an open AskUserQuestion dialog")
	}
	if !PaneShowsIdlePrompt(strings.Split("● Done.\n\n❯ \n", "\n"), DefaultReadyPromptPrefix) {
		t.Fatal("PaneShowsIdlePrompt = false for an idle composer")
	}
}

// A pane reads as an open question page only when it has all three anchors
// (select footer, a numbered option row, a ☐/☒ header row), with the footer
// in the pane's bottom block. Each anchor is what keeps a different pane from
// reading as an open dialog: the folder-trust dialog must stay nudgeable, an
// idle composer's "❯ " is not a numbered option, and an answered frame in
// scrollback is not live.
// The control runs first so the nil cases cannot pass vacuously.
func TestPendingRejectsNonDialogPanes(t *testing.T) {
	pendingFor := func(pane string) (*runtime.PendingInteraction, error) {
		tm, _ := questionTmux(pane)
		return (&Provider{tm: tm}).Pending("auq")
	}
	control := askUserQuestionHeaderRows + askUserQuestionOptionRows + askUserQuestionFooterSelect
	if got, err := pendingFor(control); err != nil || got == nil || got.Kind != "question" {
		t.Fatalf("control pane (header + option + select footer): Pending = %+v, %v; the cases below would pass vacuously:\n%s", got, err, control)
	}
	if got, err := pendingFor(askUserQuestionPane); err != nil || got == nil {
		t.Fatalf("the live capture: Pending = %+v, %v", got, err)
	}

	for _, tc := range []struct{ name, pane string }{
		{"folder-trust pane", " Do you trust the files in this folder?\n\n /work/repo\n\n" + askUserQuestionOptionRows + "\n" + askUserQuestionFooterConfirm},
		{"footer only", "Working...\n" + askUserQuestionFooterSelect},
		{"footer and option without header", askUserQuestionOptionRows + "\n" + askUserQuestionFooterSelect},
		{"footer and header with no numbered option", askUserQuestionHeaderRows + "\n" + askUserQuestionFooterSelect},
		{"option and header with the confirm footer", askUserQuestionHeaderRows + askUserQuestionOptionRows + "\n" + askUserQuestionFooterConfirm},
		{"idle composer with the select footer", "● Done.\n\n❯ \n" + askUserQuestionFooterSelect},
		{"answered frame above an idle composer", answeredAskUserQuestionPane},
		{"answered frame above working output", answeredAskUserQuestionWorkingPane},
		{"control frame with an idle composer under it", control + "\n❯ \n  ? for shortcuts\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pendingFor(tc.pane)
			if err != nil || got != nil {
				t.Fatalf("Pending = %+v, %v; want nothing for a pane that is not a live open dialog:\n%s", got, err, tc.pane)
			}
		})
	}
}

// Every dismisser sends keys. None may match an AskUserQuestion dialog: their
// Enter or digit would answer it. Folder-trust's "Enter to confirm" footer
// stays the startup dismisser's business.
func TestDismissersLeaveAskUserQuestionDialogAlone(t *testing.T) {
	newTmux := func() (*Tmux, *paneExecutor) {
		ex := &paneExecutor{pane: askUserQuestionPane, inMode: "0"}
		return &Tmux{cfg: DefaultConfig(), exec: ex}, ex
	}

	t.Run("dismissMidSessionDialogBeforeNudge", func(t *testing.T) {
		tm, ex := newTmux()
		if tm.dismissMidSessionDialogBeforeNudge(context.Background(), "auq") {
			t.Fatal("dismissed a dialog that is a question")
		}
		requireNoKeys(t, ex.calls, false)
	})
	t.Run("dismissFeedbackSurveyModalIn", func(t *testing.T) {
		tm, ex := newTmux()
		if err := tm.dismissFeedbackSurveyModalIn(context.Background(), "auq", "auq"); err != nil {
			t.Fatalf("dismissFeedbackSurveyModalIn: %v", err)
		}
		requireNoKeys(t, ex.calls, false)
	})
	t.Run("DismissModelSwitchModalIfPresent", func(t *testing.T) {
		tm, ex := newTmux()
		tm.DismissModelSwitchModalIfPresent("auq")
		requireNoKeys(t, ex.calls, false)
	})
	t.Run("DismissKnownDialogs", func(t *testing.T) {
		tm, ex := newTmux()
		_ = tm.DismissKnownDialogs(context.Background(), "auq", 300*time.Millisecond)
		requireNoKeys(t, ex.calls, false)
	})
}

// readQuestionFixture reads a capture under testdata/claude-question. The
// ga-ngrme8 files are the pane captures taken one key at a time while gc's
// nudge key sequence was replayed against a live multi-question
// AskUserQuestion dialog (commit 7ffbed8d04): the first page, the page after
// one Enter answered Q1, the review page, and a seat's idle pane quoting a
// dialog in its scrollback.
func readQuestionFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-question", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return strings.TrimRight(string(data), "\n")
}

// askUserQuestionC1Pane is the multi-question page from the #8150 triage
// repro: a tab strip, options with description rows, and the rule between
// "Type something." and "Chat about this".
const askUserQuestionC1Pane = `⏺ I have a few questions before I plan this.

────────────────────────────────────────────────────────────
 ←  ☐ Scope  ☐ Approach  ☐ Timing  ✔ Submit  →

Which scope should the change cover?

❯ 1. Small
     Only the bug fix
  2. Medium
     Fix plus regression tests
  3. Large
     Fix, tests and a refactor
  4. Type something.
────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// askUserQuestionWrappedFooterPane is a question page in a pane too narrow for
// the select footer, which wraps onto a second row.
const askUserQuestionWrappedFooterPane = `←  ☐ Scope  ☐ Approach  ✔ Submit  →
Which scope should the change cover?
❯ 1. Small
  2. Medium
  3. Type something.
───────────────────────────────────
  4. Chat about this
Enter to select · Tab/Arrow keys to
navigate · Esc to cancel`

// askUserQuestionTypeSomethingPane is the 2.1.280 frame with focus moved to the
// "Type something." row, which draws no cursor: no option row carries ❯.
var askUserQuestionTypeSomethingPane = strings.Replace(askUserQuestionPane, "❯ 1. Run gates", "  1. Run gates", 1)

// askUserQuestionPageCase is an open question page and what the parser reads
// from it.
type askUserQuestionPageCase struct {
	name     string
	pane     string
	question string
	choices  []string
	focused  string
}

func askUserQuestionPageCases(t *testing.T) []askUserQuestionPageCase {
	t.Helper()
	single := []string{
		"Run gates, then ship if clean (Recommended)",
		"Run gates, ask again before push",
		"Keep holding",
		"Type something.",
		"Chat about this",
	}
	singleQuestion := "Push deploy/be-tx30v-gate to headfork and open the PR against gastownhall/beads. How do you want to proceed?"
	return []askUserQuestionPageCase{
		{
			name:     "triage repro page",
			pane:     askUserQuestionC1Pane,
			question: "Which scope should the change cover?",
			choices:  []string{"Small", "Medium", "Large", "Type something.", "Chat about this"},
			focused:  "Small",
		},
		{
			name:     "multi-question first page (ga-ngrme8)",
			pane:     readQuestionFixture(t, "multi-question-page-ga-ngrme8.txt"),
			question: "Which approach would you prefer for this feature?",
			choices:  []string{"Alpha approach (Recommended)", "Beta variant", "Gamma variant", "Type something.", "Chat about this"},
			focused:  "Alpha approach (Recommended)",
		},
		{
			name:     "page after one answer (ga-ngrme8)",
			pane:     readQuestionFixture(t, "multi-question-after-answer-ga-ngrme8.txt"),
			question: "How should we handle configuration?",
			choices:  []string{"Environment variables (Recommended)", "Config file Beta", "Command-line Gamma", "Type something.", "Chat about this"},
			focused:  "Environment variables (Recommended)",
		},
		{
			name:     "single-question frame (2.1.280)",
			pane:     askUserQuestionPane,
			question: singleQuestion,
			choices:  single,
			focused:  "Run gates, then ship if clean (Recommended)",
		},
		{
			name:     "focus in Type something, no cursor drawn",
			pane:     askUserQuestionTypeSomethingPane,
			question: singleQuestion,
			choices:  single,
		},
		{
			name:     "footer wrapped at a narrow width",
			pane:     askUserQuestionWrappedFooterPane,
			question: "Which scope should the change cover?",
			choices:  []string{"Small", "Medium", "Type something.", "Chat about this"},
			focused:  "Small",
		},
	}
}

// An open question page is no idle boundary in any of its shapes: the
// focused row ("❯ 1. …") matches the ready prefix, and focus in "Type
// something." draws no cursor at all, so the scan must key on the dialog.
func TestAskUserQuestionPagesAreNotIdle(t *testing.T) {
	for _, tc := range askUserQuestionPageCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(tc.pane, "\n")
			q := parseAskUserQuestion(lines)
			if q == nil {
				t.Fatalf("parseAskUserQuestion = nil for an open question page:\n%s", tc.pane)
			}
			if q.Question != tc.question || !slices.Equal(q.Choices, tc.choices) || q.Focused != tc.focused {
				t.Errorf("parsed question = %q %q focused %q, want %q %q focused %q",
					q.Question, q.Choices, q.Focused, tc.question, tc.choices, tc.focused)
			}
			if PaneShowsIdlePrompt(lines, DefaultReadyPromptPrefix) {
				t.Errorf("PaneShowsIdlePrompt = true; a nudge's Enter would answer the question")
			}
			if idle, err := ClassifyIdlePane(lines, DefaultReadyPromptPrefix); idle || !errors.Is(err, runtime.ErrIdleIndeterminate) {
				t.Errorf("ClassifyIdlePane = (%v, %v), want (false, runtime.ErrIdleIndeterminate)", idle, err)
			}
		})
	}
}

// requireQuestionRefusal fails unless err is a nudge refusal of an open
// question dialog: runtime.ErrPendingInteraction, saying "question dialog",
// with the dialog's tmux-question- request ID.
func requireQuestionRefusal(t *testing.T, call string, err error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("%s = %v, want runtime.ErrPendingInteraction", call, err)
	}
	for _, want := range []string{"question dialog", "tmux-question-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s refusal %q does not carry %q", call, err.Error(), want)
		}
	}
}

// Every nudge path refuses every question page shape, whenever the page
// shows up: at NudgeNow's first check, under the nudge lock, or after the
// paste (before the Enter). Nothing reaches the dialog: no text, Ctrl-U,
// Escape or Enter. The wait-idle Nudge refuses before its idle wait.
func TestNudgePathsSendNoKeyIntoAnyAskUserQuestionPage(t *testing.T) {
	for _, tc := range askUserQuestionPageCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("NudgeSession", func(t *testing.T) {
				tm, ex := questionTmux(tc.pane)
				requireQuestionRefusal(t, "NudgeSession", tm.NudgeSession("auq", "hello"))
				requireNoKeys(t, ex.calls, false)
			})
			for _, when := range []struct {
				name  string
				setup func(*fakeClaudePane)
			}{
				{"on screen at the first check", func(p *fakeClaudePane) { p.promptOnCapture = 1 }},
				{"appears under the nudge lock", func(p *fakeClaudePane) { p.promptOnCapture = 2 }},
				{"appears after the paste", func(p *fakeClaudePane) { p.promptAfterPaste = true }},
			} {
				t.Run("NudgeNow "+when.name, func(t *testing.T) {
					pane := newFakeClaudePane("")
					pane.prompt = tc.pane
					when.setup(pane)
					requireQuestionRefusal(t, "NudgeNow", nudgeNowText(t, pane, "run the tests"))
					if len(pane.promptKeys) != 0 {
						t.Fatalf("the question dialog received %q, want nothing", pane.promptKeys)
					}
					if len(pane.submitted) != 0 {
						t.Fatalf("submitted %q, want nothing", pane.submitted)
					}
				})
			}
			t.Run("wait-idle Nudge", func(t *testing.T) {
				cfg := DefaultConfig()
				cfg.NudgeIdleTimeout = 5 * time.Second
				ex := &paneExecutor{pane: tc.pane, inMode: "0"}
				p := &Provider{tm: &Tmux{cfg: cfg, exec: ex}}
				start := time.Now()
				requireQuestionRefusal(t, "Nudge", p.Nudge("auq", runtime.TextContent("hello")))
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					t.Fatalf("Nudge took %s to refuse; it waited for idle instead of refusing at once", elapsed)
				}
				requireNoKeys(t, ex.calls, false)
			})
		})
	}
}

// Pending reports a question page as Kind "question" with no options and the
// question in its prompt. The request ID names the page: the same on every
// poll of it, whichever option has focus and whichever tab chips are checked
// when the user steps back to it, and new once the dialog moves to its next
// page. Back at the composer, nothing is pending.
func TestPendingQuestionRequestIDFollowsThePage(t *testing.T) {
	poll := func(pane string) *runtime.PendingInteraction {
		t.Helper()
		tm, _ := questionTmux(pane)
		pending, err := (&Provider{tm: tm}).Pending("auq")
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		return pending
	}
	first := readQuestionFixture(t, "multi-question-page-ga-ngrme8.txt")
	refocused := strings.Replace(strings.Replace(first, "❯ 1. Alpha", "1. Alpha", 1), "\n2. Beta", "\n❯ 2. Beta", 1)
	steppedBack := strings.Replace(first, "☐ Q1  ☐ Q2", "☒ Q1  ☒ Q2", 1)
	next := readQuestionFixture(t, "multi-question-after-answer-ga-ngrme8.txt")

	page := poll(first)
	if page == nil {
		t.Fatal("Pending = nil for an open question page")
	}
	if page.Kind != "question" || len(page.Options) != 0 || !strings.HasPrefix(page.RequestID, "tmux-question-") {
		t.Fatalf("Pending = %+v, want Kind question, no options, a tmux-question- request ID", page)
	}
	if !strings.Contains(page.Prompt, "Which approach would you prefer for this feature?") {
		t.Errorf("Prompt = %q, want the question", page.Prompt)
	}
	for name, pane := range map[string]string{
		"the same page again":                      first,
		"the same page, focus moved":               refocused,
		"the same page, stepped back to, answered": steppedBack,
	} {
		if again := poll(pane); again == nil || again.RequestID != page.RequestID {
			t.Errorf("%s: Pending = %+v, want request ID %q", name, again, page.RequestID)
		}
	}

	nextPage := poll(next)
	if nextPage == nil || nextPage.Kind != "question" {
		t.Fatalf("Pending = %+v for the page after one answer, want Kind question", nextPage)
	}
	if nextPage.RequestID == page.RequestID {
		t.Errorf("the next page kept request ID %q; a new page is a new request", page.RequestID)
	}
	if len(nextPage.Options) != 0 || !strings.Contains(nextPage.Prompt, "How should we handle configuration?") {
		t.Errorf("Pending = %+v, want no options and the next question", nextPage)
	}

	if done := poll(next + "\n" + answeredAskUserQuestionTail); done != nil {
		t.Errorf("Pending = %+v once the pane is back at its composer, want nil", done)
	}
}

// askUserQuestionProceedPane is an answered-chip question page whose question
// reads like a permission prompt's: question-first classification keeps it a
// question, so Respond's approve keys never reach it.
const askUserQuestionProceedPane = `←  ☒ Scope  ☐ Confirm  ✔ Submit  →
Do you want to proceed?
❯ 1. Yes
  2. No
  3. Type something.
────────────────────────────────────────────────────────────
  4. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel`

// Respond refuses every question page, one asking "Do you want to …?"
// included, for every action with or without a request ID, before any key.
func TestRespondRefusesEveryAskUserQuestionPage(t *testing.T) {
	panes := map[string]string{"question reading Do you want to proceed?": askUserQuestionProceedPane}
	for _, tc := range askUserQuestionPageCases(t) {
		panes[tc.name] = tc.pane
	}
	for name, pane := range panes {
		t.Run(name, func(t *testing.T) {
			tm, _ := questionTmux(pane)
			live := "tmux-question-0123456789abcdef"
			if pending, err := (&Provider{tm: tm}).Pending("auq"); err == nil && pending != nil {
				live = pending.RequestID
			}
			for _, action := range []string{"approve", "deny", "1"} {
				for _, id := range []string{"", live} {
					ex := &paneExecutor{pane: pane, inMode: "0"}
					tm := &Tmux{cfg: DefaultConfig(), exec: ex}
					err := tm.Respond("auq", runtime.InteractionResponse{Action: action, RequestID: id})
					if !errors.Is(err, runtime.ErrInteractionActionUnavailable) {
						t.Fatalf("Respond(%s, request %q) = %v, want runtime.ErrInteractionActionUnavailable", action, id, err)
					}
					requireNoKeys(t, ex.calls, false)
				}
			}
		})
	}
}

// The review page has no footer, and one Enter on it submits every answer: it
// is not idle, every nudge look refuses it with no key sent, and Pending
// reports it as a question.
func TestAskUserQuestionReviewPageHoldsEveryLook(t *testing.T) {
	review := readQuestionFixture(t, "review-page-ga-ngrme8.txt")
	panes := map[string]string{
		"review page (ga-ngrme8)":     review,
		"cursor on Cancel":            strings.Replace(strings.Replace(review, "❯ 1. Submit", "1. Submit", 1), "\n2. Cancel", "\n❯ 2. Cancel", 1),
		"title without the tab strip": "Review your answers\n● Which approach?\n→ Alpha\n\nReady to submit your answers?\n\n❯ 1. Submit answers\n  2. Cancel\n",
	}
	for name, pane := range panes {
		t.Run(name, func(t *testing.T) {
			lines := strings.Split(pane, "\n")
			if PaneShowsIdlePrompt(lines, DefaultReadyPromptPrefix) {
				t.Error("PaneShowsIdlePrompt = true for the review page; a nudge's Enter would submit every answer")
			}
			if idle, err := ClassifyIdlePane(lines, DefaultReadyPromptPrefix); idle || !errors.Is(err, runtime.ErrIdleIndeterminate) {
				t.Errorf("ClassifyIdlePane = (%v, %v), want (false, runtime.ErrIdleIndeterminate)", idle, err)
			}

			tm, ex := questionTmux(pane)
			pending, err := (&Provider{tm: tm}).Pending("auq")
			if err != nil || pending == nil || pending.Kind != "question" || len(pending.Options) != 0 {
				t.Fatalf("Pending = %+v, %v; want Kind question with no options", pending, err)
			}
			if !strings.Contains(pending.Prompt, "Ready to submit your answers?") {
				t.Errorf("Prompt = %q, want the review prompt", pending.Prompt)
			}
			requireQuestionRefusal(t, "NudgeSession", tm.NudgeSession("auq", "hello"))
			requireNoKeys(t, ex.calls, false)

			for _, setup := range []func(*fakeClaudePane){
				func(p *fakeClaudePane) { p.promptOnCapture = 1 },
				func(p *fakeClaudePane) { p.promptOnCapture = 2 },
				func(p *fakeClaudePane) { p.promptAfterPaste = true },
			} {
				fake := newFakeClaudePane("")
				fake.prompt = pane
				setup(fake)
				requireQuestionRefusal(t, "NudgeNow", nudgeNowText(t, fake, "run the tests"))
				if len(fake.promptKeys) != 0 || len(fake.submitted) != 0 {
					t.Fatalf("the review page received %q (submitted %q), want nothing", fake.promptKeys, fake.submitted)
				}
			}

			cfg := DefaultConfig()
			cfg.NudgeIdleTimeout = 5 * time.Second
			wex := &paneExecutor{pane: pane, inMode: "0"}
			start := time.Now()
			requireQuestionRefusal(t, "Nudge", (&Provider{tm: &Tmux{cfg: cfg, exec: wex}}).Nudge("auq", runtime.TextContent("hello")))
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("Nudge took %s to refuse; it waited for idle", elapsed)
			}
			requireNoKeys(t, wex.calls, false)
		})
	}
}

// numberedDraftComposerPane is an idle composer holding a draft of numbered
// lines, with no dialog footer anywhere.
const numberedDraftComposerPane = `⏺ Done. The tests pass.
────────────────────────────────────────────────────────────────────────────
❯ 1. Submit answers
  2. Cancel
────────────────────────────────────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

// A question or review page quoted in scrollback above a live composer, or a
// composer draft of numbered lines, is no dialog: the pane reads idle, nothing
// is pending, and the nudge is delivered.
func TestQuotedAskUserQuestionAboveAComposerIsNotADialog(t *testing.T) {
	composer := "\n" + answeredAskUserQuestionTail
	for name, pane := range map[string]string{
		"quoted dialog above the composer (ga-ngrme8)": readQuestionFixture(t, "quoted-dialog-above-composer-ga-ngrme8.txt"),
		"question page above the composer":             readQuestionFixture(t, "multi-question-after-answer-ga-ngrme8.txt") + composer,
		"review page above the composer":               readQuestionFixture(t, "review-page-ga-ngrme8.txt") + composer,
		"numbered draft in the composer":               numberedDraftComposerPane,
	} {
		t.Run(name, func(t *testing.T) {
			lines := strings.Split(pane, "\n")
			if q := parseAskUserQuestion(lines); q != nil {
				t.Fatalf("parseAskUserQuestion = %+v, want nil", q)
			}
			if idle, err := ClassifyIdlePane(lines, DefaultReadyPromptPrefix); !idle || err != nil {
				t.Errorf("ClassifyIdlePane = (%v, %v), want (true, nil)", idle, err)
			}
			tm, ex := questionTmux(pane)
			if pending, err := (&Provider{tm: tm}).Pending("auq"); err != nil || pending != nil {
				t.Fatalf("Pending = %+v, %v; want nothing", pending, err)
			}
			if err := tm.NudgeSession("auq", "hello"); errors.Is(err, runtime.ErrPendingInteraction) {
				t.Fatalf("NudgeSession = %v; nothing on screen waits for an answer", err)
			}
			if callIndexWithTokens(ex.calls, "send-keys", "-l", "hello") < 0 {
				t.Fatalf("the message was never pasted; calls=%v", ex.calls)
			}
		})
	}
}
