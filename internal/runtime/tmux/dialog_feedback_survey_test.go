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

const (
	claudeSurveyDigitDebounce = 400 * time.Millisecond
	claudeSurveyMountDelay    = 600 * time.Millisecond
)

type claudeSurveyModel struct {
	visible      bool
	mountedAt    time.Duration
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
		mounted := m.now-m.mountedAt >= claudeSurveyMountDelay
		m.pending = m.visible && mounted && !m.ignoreDigits && len(m.input) == 1 && strings.ContainsAny(m.input, "0123")
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

func detached() bool { return false }

func TestDismissFeedbackSurveyModalStopsWhenHumanAttachesMidDismissal(t *testing.T) {
	for _, test := range []struct {
		name      string
		attachAt  time.Duration
		wantKeys  string
		wantErr   error
		wantInput string
	}{
		{"during the mount window sends nothing", feedbackSurveyMountGuard / 2, "", nil, ""},
		{"after the digit leaves it for the human instead of clearing", feedbackSurveyMountGuard + feedbackSurveyDigitPollInterval, "0", errFeedbackSurveyDigitUnresolved, "0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := claudeSurveyModel{visible: true, mountedAt: -time.Minute, ignoreDigits: true}
			attached := func() bool { return model.now >= test.attachAt }

			err := dismissFeedbackSurveyModal(model.capture, attached, model.sendKeys, model.sleep)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("dismissFeedbackSurveyModal error = %v, want %v", err, test.wantErr)
			}
			if got := strings.Join(model.keys, ","); got != test.wantKeys {
				t.Fatalf("keys = %q, want %q once a human attached", got, test.wantKeys)
			}
			if model.input != test.wantInput {
				t.Fatalf("composer = %q, want %q", model.input, test.wantInput)
			}
		})
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
		{"waits out the debounce so the survey consumes the digit", claudeSurveyModel{visible: true, mountedAt: -time.Minute}, "0", false, ""},
		{"waits out the mount window of a fresh survey", claudeSurveyModel{visible: true}, "0", false, ""},
		{"sends nothing once the survey is gone", claudeSurveyModel{}, "", false, ""},
		{"clears digit the survey ignored", claudeSurveyModel{visible: true, mountedAt: -time.Minute, ignoreDigits: true}, "0,C-u", true, ""},
		{"leaves a human draft untouched", claudeSurveyModel{visible: true, mountedAt: -time.Minute, input: "draft"}, "", true, "draft"},
		{"leaves a multiline draft with a blank first row untouched", claudeSurveyModel{visible: true, mountedAt: -time.Minute, input: "\n│ draft"}, "", true, "\n│ draft"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := test.model

			if err := dismissFeedbackSurveyModal(model.capture, detached, model.sendKeys, model.sleep); err != nil {
				t.Fatalf("dismissFeedbackSurveyModal error = %v", err)
			}
			if got := strings.Join(model.keys, ","); got != test.wantKeys {
				t.Fatalf("keys = %q, want %q", got, test.wantKeys)
			}
			if model.visible != test.wantVisible || model.input != test.wantInput {
				t.Fatalf("survey visible = %t, composer = %q; want visible %t, composer %q", model.visible, model.input, test.wantVisible, test.wantInput)
			}
			if limit := feedbackSurveyMountGuard + feedbackSurveyDigitDeadline; model.now > limit {
				t.Fatalf("waited %s, beyond the %s limit", model.now, limit)
			}
		})
	}

	t.Run("leaves a multiline draft typed after the dismissal digit untouched", func(t *testing.T) {
		var keys []string
		captures := 0
		multilineDraft := strings.Replace(feedbackSurveySessionFixture, "│ ❯                                                        │", "│ ❯ 0                                                      │\n│ draft                                                    │", 1)
		capture := func() (string, error) {
			captures++
			if captures == 1 {
				return feedbackSurveySessionFixture, nil
			}
			return multilineDraft, nil
		}
		err := dismissFeedbackSurveyModal(capture, detached, func(sent ...string) error {
			keys = append(keys, sent...)
			return nil
		}, func(time.Duration) {})
		if !errors.Is(err, errFeedbackSurveyDigitUnresolved) {
			t.Fatalf("dismissFeedbackSurveyModal error = %v, want %v so the nudge does not submit onto the draft", err, errFeedbackSurveyDigitUnresolved)
		}
		if got := strings.Join(keys, ","); got != "0" {
			t.Fatalf("keys = %q, want %q when a continuation row contains a draft", got, "0")
		}
	})
}

func TestDismissFeedbackSurveyModalRecaptureFailure(t *testing.T) {
	captureErr := errors.New("capture failed")
	for _, test := range []struct {
		name               string
		successfulCaptures int
		wantKeys           string
		wantErr            error
		typeDraft          bool
	}{
		{"before the digit sends nothing and lets the nudge proceed", 0, "", nil, false},
		{"after the digit preserves uncertain input", 1, "0", captureErr, false},
		{"after the digit preserves an attached user's draft", 1, "0", captureErr, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var keys []string
			var composer string
			sendKeys := func(sent ...string) error {
				keys = append(keys, sent...)
				for _, key := range sent {
					if key == "C-u" {
						composer = ""
					} else {
						composer += key
					}
				}
				if test.typeDraft && slices.Contains(sent, "0") {
					composer += "draft"
				}
				return nil
			}
			captures := 0
			capture := func() (string, error) {
				captures++
				if captures > test.successfulCaptures {
					return "", captureErr
				}
				return feedbackSurveySessionFixture, nil
			}

			if err := dismissFeedbackSurveyModal(capture, detached, sendKeys, func(time.Duration) {}); !errors.Is(err, test.wantErr) {
				t.Fatalf("dismissFeedbackSurveyModal error = %v, want %v", err, test.wantErr)
			}
			if got := strings.Join(keys, ","); got != test.wantKeys {
				t.Fatalf("keys = %q, want %q", got, test.wantKeys)
			}
			if test.typeDraft && !strings.Contains(composer, "draft") {
				t.Fatalf("composer = %q, want attached user's draft preserved after the capture failure", composer)
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

	if err := tm.DismissFeedbackSurveyModalIfPresent("agent-pane"); err != nil {
		t.Fatalf("DismissFeedbackSurveyModalIfPresent error = %v", err)
	}

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
	executor := &scriptedTargetExecutor{capture: feedbackSurveySessionFixture, display: "agent-pane|0"}
	tm := &Tmux{cfg: DefaultConfig(), exec: executor}

	if err := tm.DismissFeedbackSurveyModalIfPresent("agent-pane"); err != nil {
		t.Fatalf("DismissFeedbackSurveyModalIfPresent error = %v", err)
	}

	var sent []string
	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") {
			sent = append(sent, call[len(call)-1])
		}
	}
	if got := strings.Join(sent, ","); got != "0,C-u" {
		t.Fatalf("send-keys = %q, want one dismiss digit then a clear of the possibly unread digit while the survey persists", got)
	}
}

func TestDismissFeedbackSurveyModalLeavesHumanSessionsAlone(t *testing.T) {
	for _, test := range []struct {
		name    string
		display string
	}{
		{"attached client", "agent-pane|1"},
		{"attachment unknown", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &scriptedTargetExecutor{capture: feedbackSurveySessionFixture, display: test.display}
			tm := &Tmux{cfg: DefaultConfig(), exec: executor}

			if err := tm.DismissFeedbackSurveyModalIfPresent("agent-pane"); err != nil {
				t.Fatalf("DismissFeedbackSurveyModalIfPresent error = %v", err)
			}

			for _, call := range executor.calls {
				if slices.Contains(call, "send-keys") {
					t.Fatalf("survey dismissal keyed a session a human may be typing in: %v", executor.calls)
				}
			}
		})
	}
}

type failingRecaptureExecutor struct {
	scriptedTargetExecutor
	captureFails bool
}

func (f *failingRecaptureExecutor) execute(args []string) (string, error) {
	if slices.Contains(args, "send-keys") && slices.Contains(args, "0") {
		f.captureFails = true
	}
	if f.captureFails && slices.Contains(args, "capture-pane") {
		f.calls = append(f.calls, slices.Clone(args))
		return "", errors.New("capture-pane failed")
	}
	return f.scriptedTargetExecutor.execute(args)
}

func (f *failingRecaptureExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestDismissFeedbackSurveyModalReportsUnresolvedDigit(t *testing.T) {
	executor := &failingRecaptureExecutor{scriptedTargetExecutor: scriptedTargetExecutor{capture: feedbackSurveySessionFixture, display: "agent-pane|0"}}
	tm := &Tmux{cfg: DefaultConfig(), exec: executor}

	if err := tm.DismissFeedbackSurveyModalIfPresent("agent-pane"); err == nil {
		t.Fatal("DismissFeedbackSurveyModalIfPresent error = nil after the post-digit capture failed, want an error so the nudge does not paste onto the digit")
	}
	var sent []string
	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") {
			sent = append(sent, call[len(call)-1])
		}
	}
	if got := strings.Join(sent, ","); got != "0" {
		t.Fatalf("send-keys = %q, want only the dismiss digit", got)
	}
}

func TestDismissFeedbackSurveyModalReportsUnreadableComposerAfterDigit(t *testing.T) {
	var keys []string
	captures := 0
	capture := func() (string, error) {
		captures++
		if captures == 1 {
			return feedbackSurveySessionFixture, nil
		}
		return "⏺ Done — pushed the branch and replied on the PR.", nil
	}
	err := dismissFeedbackSurveyModal(capture, detached, func(sent ...string) error {
		keys = append(keys, sent...)
		return nil
	}, func(time.Duration) {})
	if !errors.Is(err, errFeedbackSurveyDigitUnresolved) {
		t.Fatalf("dismissFeedbackSurveyModal error = %v, want %v when no frame after the digit shows the composer", err, errFeedbackSurveyDigitUnresolved)
	}
	if got := strings.Join(keys, ","); got != "0" {
		t.Fatalf("keys = %q, want only the dismiss digit", got)
	}
}

func TestFeedbackSurveyComposerReadsEveryContinuationRow(t *testing.T) {
	pane := "╭──────────────╮\n│ ❯            │\n│ 0            │\n│ draft        │\n╰──────────────╯"
	composer, observed := feedbackSurveyComposer(pane)
	if !observed {
		t.Fatal("feedbackSurveyComposer observed no composer")
	}
	if composer != "0\ndraft" {
		t.Fatalf("composer = %q, want %q so a digit on a continuation row is not mistaken for a lone dismiss digit", composer, "0\ndraft")
	}
}

func TestNudgeSessionDoesNotPasteOntoUnresolvedSurveyDigit(t *testing.T) {
	executor := &failingRecaptureExecutor{scriptedTargetExecutor: scriptedTargetExecutor{capture: feedbackSurveySessionFixture, display: "agent-pane|0"}}
	cfg := DefaultConfig()
	cfg.NudgeReadyTimeout = 10 * time.Millisecond
	tm := &Tmux{cfg: cfg, exec: executor}

	if err := tm.NudgeSession("agent-pane", "hello"); err == nil {
		t.Fatal("NudgeSession error = nil after the survey digit was left unresolved")
	}
	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") && slices.Contains(call, "hello") {
			t.Fatalf("NudgeSession pasted onto an unresolved survey digit: %v", executor.calls)
		}
	}
}

func TestNudgeSessionDeliversWhenSurveyPeekFails(t *testing.T) {
	executor := &failingRecaptureExecutor{scriptedTargetExecutor: scriptedTargetExecutor{display: "agent-pane|0"}, captureFails: true}
	cfg := DefaultConfig()
	cfg.NudgeReadyTimeout = 10 * time.Millisecond
	tm := &Tmux{cfg: cfg, exec: executor}

	_ = tm.NudgeSession("agent-pane", "hello")

	for _, call := range executor.calls {
		if slices.Contains(call, "send-keys") && slices.Contains(call, "hello") {
			return
		}
	}
	t.Fatalf("NudgeSession dropped the message after a capture-pane failure in the survey peek: %v", executor.calls)
}
