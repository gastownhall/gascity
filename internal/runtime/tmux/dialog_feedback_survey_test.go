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

// Real Claude Code 2.1.285 visible screens (120x40, tmux 3.7c), captured with
// CLAUDE_FORCE_DISPLAY_SURVEY=1: the survey parked under an idle composer, and
// the same pane after "0" + Enter dismissed it (#6859).
func readFeedbackSurveyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude-feedback-survey", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// surveyTUIExecutor is a fake tmux that behaves like Claude Code's survey as
// observed on real panes (#6859). While the survey is up, "0" then Enter
// dismisses it; Enter is consumed by the survey and does not submit. Once the
// survey is gone, keys go to the composer and Enter submits whatever the
// composer holds. The dismissed survey keeps showing for staleFrames visible
// captures, as the real TUI does for the first tens of milliseconds after
// Enter, before it redraws.
type surveyTUIExecutor struct {
	surveyScreen    string
	dismissedScreen string
	// history, when set, is what a scrollback (-S) capture returns.
	history string

	surveyUp    bool
	staleFrames int
	// loseKeys drops this many dismiss pairs (a keystroke lost to a
	// slow-to-wake pane) before the survey reacts.
	loseKeys int

	pendingZero bool
	composer    string
	submitted   []string
	zerosSent   int
	sends       [][]string
}

func (f *surveyTUIExecutor) execute(args []string) (string, error) {
	cmd := ""
	for _, a := range args {
		if a == "capture-pane" || a == "send-keys" || a == "list-panes" {
			cmd = a
			break
		}
	}
	switch cmd {
	case "capture-pane":
		if slices.Contains(args, "-S") && f.history != "" {
			return f.history, nil
		}
		if f.surveyUp {
			return f.surveyScreen, nil
		}
		if f.staleFrames > 0 {
			f.staleFrames--
			return f.surveyScreen, nil
		}
		return f.dismissedScreen, nil
	case "send-keys":
		cp := make([]string, len(args))
		copy(cp, args)
		f.sends = append(f.sends, cp)
		f.key(args[len(args)-1])
	}
	return "", nil
}

func (f *surveyTUIExecutor) key(k string) {
	if k == "0" {
		f.zerosSent++
	}
	if f.surveyUp {
		switch k {
		case "0":
			f.pendingZero = true
		case "Enter":
			if f.pendingZero && f.loseKeys > 0 {
				f.loseKeys--
			} else if f.pendingZero {
				f.surveyUp = false
			}
			f.pendingZero = false
		}
		return
	}
	switch k {
	case "Enter":
		if f.composer != "" {
			f.submitted = append(f.submitted, f.composer)
		}
		f.composer = ""
	case "C-u":
		f.composer = ""
	default:
		f.composer += k
	}
}

func (f *surveyTUIExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func newSurveyTUI(t *testing.T) *surveyTUIExecutor {
	t.Helper()
	return &surveyTUIExecutor{
		surveyScreen:    readFeedbackSurveyFixture(t, "survey-idle-120x40.txt"),
		dismissedScreen: readFeedbackSurveyFixture(t, "dismissed-idle-120x40.txt"),
		surveyUp:        true,
	}
}

func TestDismissFeedbackSurveyModalIfPresent(t *testing.T) {
	// The fixtures must stay what they claim to be.
	if tui := newSurveyTUI(t); !runtime.ContainsFeedbackSurveyModal(tui.surveyScreen) || runtime.ContainsFeedbackSurveyModal(tui.dismissedScreen) {
		t.Fatal("fixtures: survey-idle must show the survey and dismissed-idle must not")
	}

	// #6859: right after Enter the pane still shows the survey for a frame.
	// Re-checking then and typing "0" + Enter again puts "0" in the live
	// composer and submits it as a prompt, costing a model turn.
	t.Run("stale frame after dismiss sends no second 0", func(t *testing.T) {
		tui := newSurveyTUI(t)
		tui.staleFrames = 1
		tm := NewTmux()
		tm.exec = tui

		if err := tm.DismissFeedbackSurveyModalIfPresent("agent"); err != nil {
			t.Fatalf("DismissFeedbackSurveyModalIfPresent: %v", err)
		}

		if tui.surveyUp {
			t.Fatal("survey still up, want it dismissed")
		}
		if len(tui.submitted) != 0 || tui.composer != "" {
			t.Fatalf("dismissal submitted %q and left %q in the composer, want nothing: %v", tui.submitted, tui.composer, tui.sends)
		}
		if tui.zerosSent != 1 {
			t.Fatalf("sent the dismiss key %d times, want 1: %v", tui.zerosSent, tui.sends)
		}
	})

	// #6844: a survey row that is only in scrollback is not a live survey.
	t.Run("survey only in scrollback sends nothing", func(t *testing.T) {
		tui := newSurveyTUI(t)
		tui.surveyUp = false
		tui.history = tui.surveyScreen + "\n" + tui.dismissedScreen
		tm := NewTmux()
		tm.exec = tui

		if err := tm.DismissFeedbackSurveyModalIfPresent("agent"); err != nil {
			t.Fatalf("DismissFeedbackSurveyModalIfPresent: %v", err)
		}

		if len(tui.sends) != 0 {
			t.Fatalf("sent keys into a pane with no survey on screen: %v", tui.sends)
		}
	})

	// A dismiss pair the pane never saw leaves the survey on screen once the
	// TUI has had time to redraw; only then is it safe to send the pair again.
	t.Run("survey still on screen after redraw is dismissed again", func(t *testing.T) {
		tui := newSurveyTUI(t)
		tui.loseKeys = 1
		tm := NewTmux()
		tm.exec = tui

		if err := tm.DismissFeedbackSurveyModalIfPresent("agent"); err != nil {
			t.Fatalf("DismissFeedbackSurveyModalIfPresent: %v", err)
		}

		if tui.surveyUp {
			t.Fatalf("survey still up after a lost first pair, want it dismissed by the retry: %v", tui.sends)
		}
		if tui.zerosSent != 2 || len(tui.submitted) != 0 || tui.composer != "" {
			t.Fatalf("zeros sent=%d submitted=%q composer=%q, want 2 zeros and nothing typed into the composer", tui.zerosSent, tui.submitted, tui.composer)
		}
	})

	t.Run("survey that never leaves the screen is an error", func(t *testing.T) {
		tui := newSurveyTUI(t)
		tui.loseKeys = 100
		tm := NewTmux()
		tm.exec = tui
		sendKeys := func(keys ...string) error {
			for _, k := range keys {
				if _, err := tm.run("send-keys", "-t", "agent", k); err != nil {
					return err
				}
			}
			return nil
		}
		capture := func() (string, error) { return tm.CaptureVisiblePane("agent") }

		err := dismissFeedbackSurveyOnScreen(capture, sendKeys, func(time.Duration) {})

		if err == nil {
			t.Fatal("dismissFeedbackSurveyOnScreen = nil, want an error while the survey stays on screen")
		}
		if tui.zerosSent != 2 {
			t.Fatalf("sent the dismiss key %d times, want 2 (one retry): %v", tui.zerosSent, tui.sends)
		}
	})

	t.Run("capture failure is returned", func(t *testing.T) {
		tm := NewTmux()
		tm.exec = &fakeExecutor{err: errors.New("tmux exploded")}

		err := tm.DismissFeedbackSurveyModalIfPresent("agent")

		if err == nil || !strings.Contains(err.Error(), "tmux exploded") {
			t.Fatalf("DismissFeedbackSurveyModalIfPresent = %v, want the capture error", err)
		}
	})
}
