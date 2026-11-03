package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Real editor capture from Codex0.162.1 / Sol6.1 medium on47442dc8f.
// The same request was visible, but Respond refused it after opening this
// editor because the macOS control-key glyph was not recognized.
const codexQuestionEditorGlyphScreen = `qualification mascot?","options":["Otter","Owl"]}]. Wait for my native
  answer; do not answer it yourself and do not substitute plain chat for this
  native tool. After the answer arrives, reply exactly RFC8191_FIRST_<the
  answer>.


• Which animal should be the qualification mascot?

  • Otter
  • Owl

• Working (6s • esc to interrupt)

• Queued follow-up inputs


  Which animal should be the qualification mascot?

  › 1. Otter
    2. Owl
    3. Other

  enter submit   ⌃] skip   ⇧→ main prompt
`

func TestCodexQuestionEditorGlyphParsesActualNativePanel(t *testing.T) {
	for _, skip := range []string{"⌃] skip", "^] skip"} {
		t.Run(skip, func(t *testing.T) {
			pane := strings.ReplaceAll(codexQuestionEditorGlyphScreen, "⌃] skip", skip)
			q := codexQuestionPanel(strings.Split(pane, "\n"))
			if q == nil || q.Collapsed || q.Provider != "codex" || q.Question != "Which animal should be the qualification mascot?" ||
				!slices.Equal(q.answerChoices(), []string{"Otter", "Owl"}) || q.textOption() != 3 || q.FocusedNumber != 1 {
				t.Fatalf("real native editor was not parsed: %+v", q)
			}
			for name, invalid := range map[string]string{
				"no queue heading":       strings.ReplaceAll(pane, "• Queued follow-up inputs", "• Previous answer"),
				"no submit control":      strings.ReplaceAll(pane, "enter submit", "submit in ordinary chat"),
				"no skip control":        strings.ReplaceAll(pane, skip, "skip in ordinary chat"),
				"no main prompt control": strings.ReplaceAll(pane, "main prompt", "another output"),
				"historical editor":      pane + "\n› Ask Codex to do anything\n",
			} {
				if got := codexQuestionPanel(strings.Split(invalid, "\n")); got != nil {
					t.Errorf("%s accepted an unowned panel: %+v", name, got)
				}
			}
		})
	}
}

type codexQuestionEditorGlyphExecutor struct {
	nativeAnswerExecutor
}

func (e *codexQuestionEditorGlyphExecutor) execute(args []string) (string, error) {
	out, err := e.nativeAnswerExecutor.execute(args)
	// The underlying native answer fixture models key effects; every resulting
	// native editor retains the actual provider's question text and shortcuts.
	e.pane = strings.ReplaceAll(e.pane, "test mascot", "qualification mascot")
	e.pane = strings.ReplaceAll(e.pane, "^] skip", "⌃] skip")
	e.pane = strings.ReplaceAll(e.pane, "shift+→ main prompt", "⇧→ main prompt")
	if slices.Contains(args, "capture-pane") {
		out = e.pane
	}
	return out, err
}

func (e *codexQuestionEditorGlyphExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

func TestCodexQuestionEditorGlyphBoundAnswerAfterStaleRefusal(t *testing.T) {
	for _, answer := range []string{"Owl", "Heron"} {
		t.Run(answer, func(t *testing.T) {
			ex := &codexQuestionEditorGlyphExecutor{nativeAnswerExecutor: nativeAnswerExecutor{codex: true, paneExecutor: paneExecutor{pane: codexQuestionGlyphNativeScreen, inMode: "0"}}}
			path := codexGlyphQuestionRollout(t)
			tm := &Tmux{cfg: DefaultConfig(), exec: ex, codexRolloutHeld: func(int) string { return path }}
			p, err := tm.Pending("q")
			if err != nil || p == nil {
				t.Fatalf("Pending=%+v err=%v", p, err)
			}
			if err := tm.Respond("q", runtime.InteractionResponse{RequestID: "stale-native-question", Action: "answer", Text: answer}); !errors.Is(err, runtime.ErrInteractionGone) {
				t.Fatalf("stale=%v", err)
			}
			requireNoKeys(t, ex.calls, false)
			if err := tm.checkNoApprovalPrompt("q"); !errors.Is(err, runtime.ErrPendingInteraction) {
				t.Fatalf("ordinary message guard=%v", err)
			}
			requireNoKeys(t, ex.calls, false)
			if err := tm.Respond("q", runtime.InteractionResponse{RequestID: p.RequestID, Action: "answer", Text: answer}); err != nil {
				t.Fatal(err)
			}
			want := []string{"2"}
			wantEnter := 0
			if answer == "Heron" {
				want = []string{"3", "Heron"}
				wantEnter = 1
			}
			if !slices.Equal(ex.literal, want) {
				t.Fatalf("typed=%q want=%q", ex.literal, want)
			}
			opened, entered := 0, 0
			for _, call := range ex.calls {
				if slices.Contains(call, "S-Left") {
					opened++
				}
				if slices.Contains(call, "send-keys") && slices.Contains(call, "Enter") {
					entered++
				}
			}
			if opened != 1 || entered != wantEnter {
				t.Fatalf("opened=%d entered=%d want Enter=%d", opened, entered, wantEnter)
			}
		})
	}
}

func TestCodexQuestionEditorGlyphRequiresMatchingNativeOccurrence(t *testing.T) {
	tm, ex := nativeQuestionTmux(t, codexQuestionEditorGlyphScreen)
	path := codexGlyphQuestionRollout(t)
	tm.codexRolloutHeld = func(int) string { return path }
	first, err := tm.Pending("q")
	if err != nil || first == nil {
		t.Fatalf("matching native editor=%+v err=%v", first, err)
	}
	requireNoKeys(t, ex.calls, false)
	// A matching screen never authorizes an answer for a previous invocation.
	tm.codexRolloutHeld = func(int) string { return "" }
	if _, err := tm.Pending("q"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("missing native identity=%v", err)
	}
	if err := tm.Respond("q", runtime.InteractionResponse{RequestID: first.RequestID, Action: "answer", Text: "Owl"}); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("unbound editor=%v", err)
	}
	requireNoKeys(t, ex.calls, false)
}
