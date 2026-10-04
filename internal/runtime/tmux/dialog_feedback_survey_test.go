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

func TestDismissFeedbackSurveyModalKeySafety(t *testing.T) {
	captureErr := errors.New("capture failed")
	tests := []struct {
		name           string
		captured       string
		captureErr     error
		wantKeys       string
		wantCaptureErr bool
	}{
		{"clears stray digit when survey vanishes", "╭───╮\n│ ❯ 0 │\n╰───╯", nil, "0,C-u", false},
		{"sends only dismiss digit while survey remains", feedbackSurveySessionFixture, nil, "0", false},
		{"clears digit when recapture fails", "", captureErr, "0,C-u", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var keys []string
			capture := func() (string, error) {
				return test.captured, test.captureErr
			}
			sendKeys := func(sent ...string) error {
				keys = append(keys, sent...)
				return nil
			}

			present, err := dismissFeedbackSurveyModal(feedbackSurveySessionFixture, capture, sendKeys, func(time.Duration) {})
			if test.wantCaptureErr != errors.Is(err, captureErr) {
				t.Fatalf("dismissFeedbackSurveyModal error = %v, wantCaptureErr %t", err, test.wantCaptureErr)
			}
			if !present {
				t.Fatal("dismissFeedbackSurveyModal reported no survey")
			}
			if got := strings.Join(keys, ","); got != test.wantKeys {
				t.Fatalf("keys = %q, want %q", got, test.wantKeys)
			}
		})
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

func TestDismissFeedbackSurveyModalDoesNotRetryAfterSurveyVanishes(t *testing.T) {
	executor := &scriptedTargetExecutor{captures: []string{
		feedbackSurveySessionFixture,
		"❯ ",
	}}
	tm := &Tmux{cfg: DefaultConfig(), exec: executor}

	tm.DismissFeedbackSurveyModalIfPresent("agent-pane")

	var sent [][]string
	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") {
			sent = append(sent, call)
		}
	}
	if len(sent) != 1 || sent[0][len(sent[0])-1] != "0" {
		t.Fatalf("send-keys calls = %v, want one dismiss digit and no retry", sent)
	}
}
