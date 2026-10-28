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

// readQuestionFixture reads one of the real pane captures under
// testdata/claude-question.
func readQuestionFixture(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-question", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
}

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

// The captures under testdata/claude-question parse to the dialog fields a
// client is shown. Provenance, per file:
//   - question-page-ga-ngrme8.txt, question-after-enter-ga-ngrme8.txt: real
//     multi-question captures taken for ga-ngrme8 (commit 7ffbed8d04,
//     nudge_human_prompt_test.go) — the page before the first key, and the
//     page after one Enter (Q1 answered, ☒ chip).
//   - question-single-2.1.280.txt: the live single-question frame captured for
//     ga-61fduec (Claude Code v2.1.280, fullscreen TUI).
//   - question-multi-2.1.304-c1.txt: the #8150 report's pane lines (Claude
//     Code 2.1.304, macOS, tmux runtime).
//   - question-multi-wrapped-footer-40x18.txt: a reconstruction of the C1 page
//     at a width that wraps the select footer onto two rows.
func TestParseQuestionDialog_Fixtures(t *testing.T) {
	cases := []struct {
		fixture string
		// wantQuestion and wantFocused may be empty when the capture's
		// rendering leaves them blank.
		wantQuestion string
		wantChoices  []string
		wantFocused  string
	}{
		{
			fixture:      "question-page-ga-ngrme8.txt",
			wantQuestion: "Which approach would you prefer for this feature?",
			wantChoices: []string{
				"Alpha approach (Recommended)",
				"Beta variant",
				"Gamma variant",
				"Type something.",
				"Chat about this",
			},
			wantFocused: "Alpha approach (Recommended)",
		},
		{
			fixture:      "question-after-enter-ga-ngrme8.txt",
			wantQuestion: "How should we handle configuration?",
			wantChoices: []string{
				"Environment variables (Recommended)",
				"Config file Beta",
				"Command-line Gamma",
				"Type something.",
				"Chat about this",
			},
			wantFocused: "Environment variables (Recommended)",
		},
		{
			fixture:      "question-single-2.1.280.txt",
			wantQuestion: "Push deploy/be-tx30v-gate to headfork and open the PR against gastownhall/beads. How do you want to proceed?",
			wantChoices: []string{
				"Run gates, then ship if clean (Recommended)",
				"Run gates, ask again before push",
				"Keep holding",
				"Type something.",
				"Chat about this",
			},
			wantFocused: "Run gates, then ship if clean (Recommended)",
		},
		{
			fixture:      "question-multi-2.1.304-c1.txt",
			wantQuestion: "Which scope should the change cover?",
			wantChoices: []string{
				"Small",
				"Medium",
				"Large",
				"Type something.",
				"Chat about this",
			},
			wantFocused: "Small",
		},
		{
			// A width that wraps the select footer onto two rows; the
			// question wraps with the pane.
			fixture:      "question-multi-wrapped-footer-40x18.txt",
			wantQuestion: "Which scope should the change cover?",
			wantChoices:  []string{"Small", "Medium", "Chat about this"},
			wantFocused:  "Small",
		},
		{
			// The review page: no footer, the Submit answers / Cancel menu
			// directly under "Ready to submit your answers?". One Enter here
			// submits every answer, so it is in scope for #8150.
			fixture:      "review-page-ga-ngrme8.txt",
			wantQuestion: "Ready to submit your answers?",
			wantChoices:  []string{"Submit answers", "Cancel"},
			wantFocused:  "Submit answers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			q := parseQuestionDialog(readQuestionFixture(t, tc.fixture))
			if q == nil {
				t.Fatal("parseQuestionDialog = nil; want the live question page")
			}
			if q.Question != tc.wantQuestion {
				t.Errorf("Question = %q, want %q", q.Question, tc.wantQuestion)
			}
			if !slices.Equal(q.Choices, tc.wantChoices) {
				t.Errorf("Choices = %q, want %q", q.Choices, tc.wantChoices)
			}
			if q.Focused != tc.wantFocused {
				t.Errorf("Focused = %q, want %q", q.Focused, tc.wantFocused)
			}
			if !strings.HasPrefix(q.requestID(), "tmux-question-") {
				t.Errorf("requestID = %q, want the tmux-question- prefix", q.requestID())
			}
		})
	}
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
		"idle composer below": answeredAskUserQuestionPane,
		"agent working below": answeredAskUserQuestionWorkingPane,
		"no dialog at all":    "● Done.\n\n❯ \n",
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
// it spends the idle wait on a pane that cannot turn idle by itself. Every
// live page shape must refuse: the cursorless and answered pages and the
// review page read as idle composers to the idle scan, so a wait-first Nudge
// would submit into them (#8150).
func TestProviderNudgeRefusesAskUserQuestionDialog(t *testing.T) {
	focusInTypeSomething := ` ←  ☐ Scope  ☐ Approach  ☐ Timing  ✔ Submit  →

Which scope should the change cover?

  1. Small
     Only the bug fix
  2. Medium
     Fix plus regression tests
  3. Large
     Fix, tests and a refactor
  4. Type something.
     fix it fast
────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`
	for name, pane := range map[string]string{
		"question page":    askUserQuestionPane,
		"no cursor":        focusInTypeSomething,
		"page after Enter": strings.Join(readQuestionFixture(t, "question-after-enter-ga-ngrme8.txt"), "\n") + "\n",
		"wrapped footer":   strings.Join(readQuestionFixture(t, "question-multi-wrapped-footer-40x18.txt"), "\n") + "\n",
		"review page":      reviewPagePane(t),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.NudgeIdleTimeout = 5 * time.Second
			ex := &paneExecutor{pane: pane, inMode: "0"}
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
		})
	}
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

// A pane reads as an open question dialog when the select footer is its
// bottom-most block — the cursor, the ☐ chips and the question rows above the
// footer never decide: focus in "Type something." draws no cursor, an answered
// question's chip reads ☒, and the question can wrap. What keeps other panes
// from reading as one is the footer itself: the folder-trust dialog's "Enter to
// confirm", an approval's "Esc to cancel · Tab to amend", and a composer
// standing below the footer, which makes the frame history. The control runs
// first so the nil cases cannot pass vacuously.
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
		{"option and header with the confirm footer", askUserQuestionHeaderRows + askUserQuestionOptionRows + "\n" + askUserQuestionFooterConfirm},
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

// Every live question page reports a pending question: the multi-question
// captures, the page after one Enter (☒ chips), the 2.1.280 single-question
// frame, a wrapped select footer, and a page whose focus sits in the
// free-text row, which draws no cursor. The older detector keyed on the ❯
// cursor and ☐ chips, so the cursorless and answered pages read as idle
// composers and a nudge's Enter answered them (#8150).
func TestProviderPendingReportsEveryLiveQuestionPage(t *testing.T) {
	// questionPaneFocusInTypeSomething is a reconstruction: the 2.1.304
	// multi-question page with focus in the free-text row, which draws no ❯.
	const questionPaneFocusInTypeSomething = ` ←  ☐ Scope  ☐ Approach  ☐ Timing  ✔ Submit  →

Which scope should the change cover?

  1. Small
     Only the bug fix
  2. Medium
     Fix plus regression tests
  3. Large
     Fix, tests and a refactor
  4. Type something.
     fix it fast
────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`
	for name, pane := range map[string]string{
		// Real multi-question capture, ga-ngrme8 (7ffbed8d04).
		"multi-question page":  strings.Join(readQuestionFixture(t, "question-page-ga-ngrme8.txt"), "\n") + "\n",
		"page after one Enter": strings.Join(readQuestionFixture(t, "question-after-enter-ga-ngrme8.txt"), "\n") + "\n",
		// Real single-question frame, Claude Code 2.1.280 (ga-61fduec).
		"single-question 2.1.280": askUserQuestionPane,
		// Reconstruction: the 2.1.304 page at a width that wraps the footer.
		"wrapped select footer":    strings.Join(readQuestionFixture(t, "question-multi-wrapped-footer-40x18.txt"), "\n") + "\n",
		"focus in Type something.": questionPaneFocusInTypeSomething,
		// The C1 lines of the #8150 report, Claude Code 2.1.304.
		"multi-question 2.1.304 C1": strings.Join(readQuestionFixture(t, "question-multi-2.1.304-c1.txt"), "\n") + "\n",
		// Minimal panes on the key's boundary: the footer alone is the key.
		"select footer alone":                       "Working...\n" + askUserQuestionFooterSelect,
		"numbered rows with no cursor":              askUserQuestionHeaderRows + "  1. Yes\n  2. No\n\n" + askUserQuestionFooterSelect,
		"idle composer row above the select footer": "● Done.\n\n❯ \n" + askUserQuestionFooterSelect,
	} {
		t.Run(name, func(t *testing.T) {
			tm, _ := questionTmux(pane)
			pending, err := (&Provider{tm: tm}).Pending("auq")
			if err != nil {
				t.Fatalf("Pending: %v", err)
			}
			if pending == nil || pending.Kind != "question" {
				t.Fatalf("Pending = %+v; want a pending question — a nudge's Enter would answer this dialog", pending)
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

// reviewPagePane is the review-page capture as a pane string.
func reviewPagePane(t *testing.T) string {
	t.Helper()
	return strings.Join(readQuestionFixture(t, "review-page-ga-ngrme8.txt"), "\n") + "\n"
}

// The review page is the dialog's second shape: no select footer, just the
// Submit answers / Cancel menu under "Ready to submit your answers?". One
// Enter on it submits every answer, so it must read as a live question — not
// as the idle composer its "❯ 1." row resembles — and its pending question
// must differ from the dialog's question pages.
func TestProviderPendingReportsReviewPageAsQuestion(t *testing.T) {
	tm, _ := questionTmux(reviewPagePane(t))
	pending, err := (&Provider{tm: tm}).Pending("auq")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil || pending.Kind != "question" {
		t.Fatalf("Pending = %+v; want a pending question — a nudge's Enter would submit every answer", pending)
	}
	if !strings.HasPrefix(pending.RequestID, "tmux-question-") {
		t.Errorf("RequestID = %q, want the tmux-question- prefix", pending.RequestID)
	}

	questionTm, _ := questionTmux(strings.Join(readQuestionFixture(t, "question-page-ga-ngrme8.txt"), "\n") + "\n")
	questionPending, err := (&Provider{tm: questionTm}).Pending("auq")
	if err != nil || questionPending == nil {
		t.Fatalf("Pending on the question page = %+v, %v", questionPending, err)
	}
	if questionPending.RequestID == pending.RequestID {
		t.Errorf("review page RequestID %q equals the question page's; the ID must move with the page", pending.RequestID)
	}

	lines := readQuestionFixture(t, "review-page-ga-ngrme8.txt")
	if PaneShowsIdlePrompt(lines, DefaultReadyPromptPrefix) {
		t.Error("PaneShowsIdlePrompt = true for the review page; a nudge's Enter would submit every answer")
	}
	if idle, err := ClassifyIdlePane(lines, DefaultReadyPromptPrefix); err == nil && idle {
		t.Error("ClassifyIdlePane(review page) = (true, nil), want not idle")
	}
}

// Every nudge path must refuse the review page before any key: NudgeSession
// (hence every queued, backstop and sling delivery), NudgeNow, and the
// wait-idle Nudge before it spends its idle wait.
func TestNudgeRefusesReviewPage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NudgeIdleTimeout = 5 * time.Second
	ex := &paneExecutor{pane: reviewPagePane(t), inMode: "0"}
	p := &Provider{tm: &Tmux{cfg: cfg, exec: ex}}

	start := time.Now()
	if err := p.Nudge("auq", runtime.TextContent("hello")); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("Nudge = %v, want runtime.ErrPendingInteraction", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Nudge took %s to refuse; it waited for idle instead of refusing at once", elapsed)
	}
	if err := p.NudgeNow("auq", runtime.TextContent("hello")); !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow = %v, want runtime.ErrPendingInteraction", err)
	}
	requireNoKeys(t, ex.calls, false)
}

// stagedPaneExecutor serves capture-pane with screens[0] until a call matches
// trigger, and screens[1] afterwards: the pane changes under the nudge's feet
// between two of its looks. swapIndex is the index of the first call answered
// with screens[1] context, so a test can check what was keyed before and after
// the change.
type stagedPaneExecutor struct {
	calls     [][]string
	screens   [2]string
	trigger   func(args []string) bool
	inMode    string
	swapped   bool
	swapIndex int
}

func (e *stagedPaneExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, slices.Clone(args))
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	if slices.Contains(args, "#{pane_in_mode}") {
		return e.inMode, nil
	}
	if !e.swapped && e.trigger(args) {
		e.swapped = true
	}
	if slices.Contains(args, "capture-pane") {
		if e.swapped {
			e.swapIndex = len(e.calls) - 1
			return e.screens[1], nil
		}
		return e.screens[0], nil
	}
	return "", nil
}

func (e *stagedPaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// keysAfterSwap fails when the nudge sent keys once the swapped-in screen was
// on the pane: whatever the dialog is, no text, Ctrl-U, Escape or Enter may
// reach it.
func keysAfterSwap(t *testing.T, e *stagedPaneExecutor) {
	t.Helper()
	for _, call := range e.calls[e.swapIndex:] {
		if sendsKeys(call) {
			t.Fatalf("sent keys while the question dialog was on screen: %q", call)
		}
	}
}

// A dialog that appears while the nudge holds the lock — after the first look,
// before the paste — must stop it there: the paste and its Enter would land
// on the dialog and answer it (#8150).
func TestNudgeSessionRefusesQuestionDialogAppearingUnderTheLock(t *testing.T) {
	afterEnter := strings.Join(readQuestionFixture(t, "question-after-enter-ga-ngrme8.txt"), "\n") + "\n"
	ex := &stagedPaneExecutor{
		screens: [2]string{"❯ \n", afterEnter},
		inMode:  "0",
		// The mid-session dismissal peek is the visible-only capture (no
		// "-S"): the dialog appears right after the nudge's first look.
		trigger: func(args []string) bool {
			return slices.Contains(args, "capture-pane") && !slices.Contains(args, "-S")
		},
	}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	err := tm.NudgeSession("auq", "hello")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeSession = %v, want runtime.ErrPendingInteraction", err)
	}
	if !strings.Contains(err.Error(), "question dialog") || !strings.Contains(err.Error(), "tmux-question-") {
		t.Fatalf("refusal %q must say it is a question dialog and carry the tmux-question- request ID", err)
	}
	if callIndexWithTokens(ex.calls, "send-keys", "-l", "hello") >= 0 {
		t.Fatal("the message was pasted after the dialog appeared")
	}
	keysAfterSwap(t, ex)
}

// A dialog that appears after the paste must stop the nudge before its Enter:
// the text sits in the pane's input, but the submit key is what answers the
// dialog, so it is never sent.
func TestNudgeSessionRefusesQuestionDialogAppearingAfterThePaste(t *testing.T) {
	afterEnter := strings.Join(readQuestionFixture(t, "question-after-enter-ga-ngrme8.txt"), "\n") + "\n"
	ex := &stagedPaneExecutor{
		screens: [2]string{"❯ \n", afterEnter},
		inMode:  "0",
		trigger: func(args []string) bool { return sendsKeys(args) && slices.Contains(args, "-l") },
	}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}
	err := tm.NudgeSession("auq", "hello")
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeSession = %v, want runtime.ErrPendingInteraction", err)
	}
	if !strings.Contains(err.Error(), "question dialog") || !strings.Contains(err.Error(), "tmux-question-") {
		t.Fatalf("refusal %q must say it is a question dialog and carry the tmux-question- request ID", err)
	}
	if callIndexWithTokens(ex.calls, "send-keys", "-l", "hello") < 0 {
		t.Fatal("the message was never pasted; the dialog appeared only after the paste")
	}
	keysAfterSwap(t, ex)
}
