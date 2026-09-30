package tmux

import (
	"context"
	"slices"
	"testing"
)

// surveyInScrollbackExecutor models a pane whose VISIBLE screen is an idle
// Claude Code prompt with no feedback survey, while the 120-line history
// window ("capture-pane -S -120") still holds an earlier survey's option row
// above later output. Visible-only captures (no "-S") see no survey.
type surveyInScrollbackExecutor struct {
	calls [][]string
}

const surveyScrollbackHistory = `● How is Claude doing this session? (optional)
  1: Bad    2: Fine   3: Good   0: Dismiss

⏺ Done — pushed the branch and replied on the PR.
` + surveyVisibleIdle

const surveyVisibleIdle = `
╭──────────────────────────────────────────────────────────╮
│ ❯                                                        │
╰──────────────────────────────────────────────────────────╯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

func (e *surveyInScrollbackExecutor) execute(args []string) (string, error) {
	cp := make([]string, len(args))
	copy(cp, args)
	e.calls = append(e.calls, cp)
	if slices.Contains(args, "capture-pane") {
		if slices.Contains(args, "-S") {
			return surveyScrollbackHistory, nil
		}
		return surveyVisibleIdle, nil
	}
	return "", nil
}

func (e *surveyInScrollbackExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// hq-hfsut (2): a nudge into a pane with no survey on screen must not type
// the survey's "0" dismiss key; otherwise "0" + Enter lands in the empty
// composer as a user message.
func TestNudgeSessionSendsNoSurveyDismissKeyWhenSurveyNotOnScreen(t *testing.T) {
	ex := &surveyInScrollbackExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: ex}

	if err := tm.NudgeSession("agent-pane", "hello world"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}
	var zeros [][]string
	for _, c := range ex.calls {
		if slices.Contains(c, "send-keys") && slices.Contains(c, "0") {
			zeros = append(zeros, c)
		}
	}
	if len(zeros) != 0 {
		t.Fatalf("sent the survey dismiss key %d time(s) into a pane with no survey on screen: %v", len(zeros), zeros)
	}
}
