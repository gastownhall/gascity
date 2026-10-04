package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// feedbackSurveySessionFixture and feedbackSurveyMemoryFixture are
// byte-accurate captures of Claude Code's post-turn feedback survey
// (ga-zg7fjq), kept in sync with the equivalent fixtures in
// internal/runtime/dialog_test.go.
const feedbackSurveySessionFixture = `⏺ Done — pushed the branch and replied on the PR.

● How is Claude doing this session? (optional)
  1: Bad    2: Fine   3: Good   0: Dismiss

╭──────────────────────────────────────────────────────────╮
│ ❯                                                        │
╰──────────────────────────────────────────────────────────╯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

const feedbackSurveyMemoryFixture = `⏺ Reading the config.

● Claude recalled a memory:

  The user prefers tabs over spaces.

  How was Claude's recollection? (optional)
  1: Bad    2: Fine   3: Good   4: Unsure  0: Dismiss

╭──────────────────────────────────────────────────────────╮
│ ❯                                                        │
╰──────────────────────────────────────────────────────────╯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

// TestFeedbackSurveyParkedPaneReadsIdle documents the ga-zg7fjq root cause: a
// pane parked on Claude Code's feedback-survey modal shows neither a busy
// indicator nor a filled composer, so it satisfies WaitForIdle's existing
// idle check exactly like a genuinely idle prompt would. That's why
// NudgeSession's only dismissal hook (inside WaitForIdle's err != nil
// branch) never ran for this modal -- WaitForIdle returns nil before that
// branch is ever reached. This pins the pre-fix idle-detection behavior; it
// must keep passing after the fix, since broadening busy detection to cover
// the survey is explicitly out of scope (a survey-parked pane genuinely is
// idle -- the fix dismisses it as a nudge-delivery step instead).
func TestFeedbackSurveyParkedPaneReadsIdle(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{"session feedback variant", feedbackSurveySessionFixture},
		{"memory recollection variant", feedbackSurveyMemoryFixture},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines := strings.Split(tt.content, "\n")
			if paneContainsBusyIndicator(lines) {
				t.Fatalf("paneContainsBusyIndicator = true, want false (a survey-parked pane must read idle, not busy)")
			}
			found := false
			for _, line := range lines {
				if matchesPromptPrefix(line, DefaultReadyPromptPrefix) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no line matched the ready-prompt prefix %q; want the boxed composer line to match so the pane reads idle:\n%s", DefaultReadyPromptPrefix, tt.content)
			}
		})
	}
}

const claudeSurveyDigitDebounce = 400 * time.Millisecond

type claudeSurveyModel struct {
	visible      bool
	ignoreDigits bool
	input        string
	now          time.Duration
	pendingSince time.Duration
	pending      bool
	keys         []string
}

func (m *claudeSurveyModel) render() string {
	var b strings.Builder
	if m.visible {
		b.WriteString("● How is Claude doing this session? (optional)\n")
		b.WriteString("  1: Bad    2: Fine   3: Good   0: Dismiss\n\n")
	}
	b.WriteString("╭──────────────────────────────╮\n")
	b.WriteString("│ ❯ " + m.input + " │\n")
	b.WriteString("╰──────────────────────────────╯")
	return b.String()
}

func (m *claudeSurveyModel) capture() (string, error) {
	return m.render(), nil
}

func (m *claudeSurveyModel) sendKeys(keys ...string) error {
	for _, key := range keys {
		m.keys = append(m.keys, key)
		if key == "C-u" {
			m.input = ""
		} else {
			m.input += key
		}
		m.pending = m.visible && !m.ignoreDigits && len(m.input) == 1 && strings.ContainsAny(m.input, "0123")
		m.pendingSince = m.now
	}
	return nil
}

func (m *claudeSurveyModel) sleep(d time.Duration) {
	m.now += d
	if m.pending && m.now-m.pendingSince >= claudeSurveyDigitDebounce {
		m.pending = false
		m.visible = false
		m.input = ""
	}
}

func TestDismissFeedbackSurveyModalAgainstDebouncedSurvey(t *testing.T) {
	tests := []struct {
		name        string
		model       claudeSurveyModel
		wantKeys    string
		wantVisible bool
		wantInput   string
	}{
		{"waits out the debounce so the survey consumes the digit", claudeSurveyModel{visible: true}, "0", false, ""},
		{"clears stray digit when survey is already gone", claudeSurveyModel{}, "0,C-u", false, ""},
		{"clears digit the survey ignored", claudeSurveyModel{visible: true, ignoreDigits: true}, "0,C-u", true, ""},
		{"leaves a human draft in place", claudeSurveyModel{visible: true, input: "draft"}, "0", true, "draft0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := test.model

			present, err := dismissFeedbackSurveyModal(feedbackSurveySessionFixture, model.capture, model.sendKeys, model.sleep)
			if err != nil {
				t.Fatalf("dismissFeedbackSurveyModal error = %v", err)
			}
			if !present {
				t.Fatal("dismissFeedbackSurveyModal reported no survey")
			}
			if got := strings.Join(model.keys, ","); got != test.wantKeys {
				t.Fatalf("keys = %q, want %q", got, test.wantKeys)
			}
			if model.visible != test.wantVisible || model.input != test.wantInput {
				t.Fatalf("survey visible = %t, composer = %q; want visible %t, composer %q", model.visible, model.input, test.wantVisible, test.wantInput)
			}
			if model.now > feedbackSurveyDigitDeadline {
				t.Fatalf("waited %s, beyond the %s deadline", model.now, feedbackSurveyDigitDeadline)
			}
		})
	}
}

func TestDismissFeedbackSurveyModalClearsDigitWhenRecaptureFails(t *testing.T) {
	captureErr := errors.New("capture failed")
	var keys []string
	sendKeys := func(sent ...string) error {
		keys = append(keys, sent...)
		return nil
	}
	capture := func() (string, error) { return "", captureErr }

	present, err := dismissFeedbackSurveyModal(feedbackSurveySessionFixture, capture, sendKeys, func(time.Duration) {})
	if !errors.Is(err, captureErr) {
		t.Fatalf("dismissFeedbackSurveyModal error = %v, want %v", err, captureErr)
	}
	if !present {
		t.Fatal("dismissFeedbackSurveyModal reported no survey")
	}
	if got := strings.Join(keys, ","); got != "0,C-u" {
		t.Fatalf("keys = %q, want %q", got, "0,C-u")
	}
}

type staleSurveyScrollbackExecutor struct {
	calls [][]string
}

func (s *staleSurveyScrollbackExecutor) execute(args []string) (string, error) {
	s.calls = append(s.calls, slices.Clone(args))
	if slices.Contains(args, "capture-pane") {
		if slices.Contains(args, "-S") {
			return feedbackSurveySessionFixture, nil
		}
		return "❯ ", nil
	}
	return "", nil
}

func (s *staleSurveyScrollbackExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

func TestDismissFeedbackSurveyModalIgnoresSurveyOnlyInScrollback(t *testing.T) {
	executor := &staleSurveyScrollbackExecutor{}
	tm := &Tmux{cfg: DefaultConfig(), exec: executor}

	tm.DismissFeedbackSurveyModalIfPresent("agent-pane")

	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") {
			t.Fatalf("stale survey scrollback sent stray keys: %v", executor.calls)
		}
	}
	wantCapture := []string{"-u", "capture-pane", "-p", "-t", "=agent-pane:"}
	if len(executor.calls) != 2 || !slices.Equal(executor.calls[1], wantCapture) {
		t.Fatalf("calls = %v, want pane lookup then visible capture %v", executor.calls, wantCapture)
	}
}

func TestDismissFeedbackSurveyModalSendsOneDigitWhileSurveyPersists(t *testing.T) {
	executor := &scriptedTargetExecutor{
		captures: []string{feedbackSurveySessionFixture},
		capture:  strings.Replace(feedbackSurveySessionFixture, "│ ❯   ", "│ ❯ 0 ", 1),
	}
	tm := &Tmux{cfg: DefaultConfig(), exec: executor}

	tm.DismissFeedbackSurveyModalIfPresent("agent-pane")

	var sent []string
	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") {
			sent = append(sent, call[len(call)-1])
		}
	}
	if got := strings.Join(sent, ","); got != "0,C-u" {
		t.Fatalf("send-keys = %q, want one dismiss digit then a clear, with no second digit", got)
	}
}
