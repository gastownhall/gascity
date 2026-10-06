//go:build integration

package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func buildFeedbackSurveyAgent(t *testing.T, dir, name string) string {
	t.Helper()
	bin := dir + "/" + name
	src := dir + "/" + name + ".go"
	prog := `package main
import ("fmt";"os";"os/exec";"strings";"sync";"time")
func main(){
	stty := exec.Command("stty", "-icanon", "-echo", "-isig", "-ixon", "min", "1")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		panic(err)
	}
	stale := os.Args[1] == "stale"
	keyLog, err := os.Create(os.Args[2])
	if err != nil {
		panic(err)
	}
	survey := "● How is Claude doing this session? (optional)\n  1: Bad    2: Fine   3: Good   0: Dismiss\n\n"
	var mu sync.Mutex
	visible := !stale
	input := ""
	var timer *time.Timer
	composer := func() { fmt.Print("\r\x1b[2K│ ❯ " + input) }
	if stale {
		fmt.Print(survey + "SURVEY_DISMISSED" + strings.Repeat("\n", 40))
	} else {
		fmt.Print("⏺ Done — pushed the branch and replied on the PR.\n\n" + survey)
	}
	composer()
	shown := time.Now()
	accepts := func() bool { return visible && input == "0" && time.Since(shown) >= 600*time.Millisecond }
	dismiss := func() {
		visible = false
		input = ""
		fmt.Print("\x1b[2J\x1b[HSURVEY_DISMISSED\n")
		composer()
	}
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			return
		}
		b := buf[0]
		mu.Lock()
		_, _ = keyLog.Write(buf)
		if timer != nil {
			timer.Stop()
			timer = nil
		}
		switch {
		case b == 0x15:
			input = ""
		case b == '\r' || b == '\n':
			if accepts() {
				dismiss()
				mu.Unlock()
				continue
			}
			fmt.Print("\nSUBMITTED:" + input + "\nesc to interrupt\n")
			input = ""
		case b >= 0x20:
			input += string(b)
		}
		composer()
		if accepts() {
			timer = time.AfterFunc(400*time.Millisecond, func() {
				mu.Lock()
				defer mu.Unlock()
				if visible && input == "0" {
					dismiss()
				}
			})
		}
		mu.Unlock()
	}
}
`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", src, err)
	}
	build := exec.Command("go", "build", "-o", bin, src)
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, string(out))
	}
	return bin
}

func startFeedbackSurveyAgent(t *testing.T, tm *Tmux, mode string) (session, keyLog string) {
	t.Helper()
	dir := t.TempDir()
	fake := buildFeedbackSurveyAgent(t, dir, "fakesurvey")
	keyLog = dir + "/keys.log"
	session = fmt.Sprintf("gt-test-feedback-survey-%d", time.Now().UnixNano()%100000)
	_ = tm.KillSession(session)
	if err := tm.NewSessionWithCommandAndEnv(session, dir, fake+" "+mode+" "+keyLog, map[string]string{
		"GC_PROVIDER": "claude",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })
	ready := map[string]string{"survey": "0: Dismiss", "stale": "SURVEY_DISMISSED"}[mode]
	deadline := time.Now().Add(10 * time.Second)
	for {
		pane, err := tm.CapturePaneAll(session)
		if err == nil && strings.Contains(pane, ready) {
			return session, keyLog
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake survey agent never drew %q (capture err %v):\n%s", ready, err, pane)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readFeedbackSurveyKeyLog(t *testing.T, path string) string {
	t.Helper()
	keys, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(keys)
}

// TestNudgeSessionDismissesFeedbackSurveyBeforeDelivering proves ga-zg7fjq's
// fix end-to-end on real tmux. A pane parked on Claude Code's feedback-survey
// modal reads idle -- documenting why the bug was invisible to WaitForIdle --
// but NudgeSession still dismisses the survey before pasting, so the nudge
// message reaches the composer and gets submitted instead of being corrupted
// by (or vanishing into) the survey's single-digit input handler.
func TestNudgeSessionDismissesFeedbackSurveyBeforeDelivering(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName, keyLog := startFeedbackSurveyAgent(t, tm, "survey")

	// Precondition: the pane shows the feedback survey.
	pre, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(pre, "0: Dismiss") {
		t.Fatalf("precondition: feedback survey not shown:\n%s", pre)
	}

	// The parked pane reads idle -- the root cause ga-zg7fjq fixes: the only
	// dismissal hook lived inside WaitForIdle's err != nil branch, which
	// never executes for a modal that WaitForIdle itself reports as idle.
	if err := tm.WaitForIdle(context.Background(), sessionName, 2*time.Second); err != nil {
		t.Fatalf("WaitForIdle on a survey-parked pane = %v, want nil (a parked pane reads idle)", err)
	}

	if err := tm.NudgeSession(sessionName, "feedback-nudge-message"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "SURVEY_DISMISSED") {
		t.Fatalf("survey was never dismissed; keys %q:\n%s", readFeedbackSurveyKeyLog(t, keyLog), out)
	}
	if !strings.Contains(out, "SUBMITTED:feedback-nudge-message\n") {
		t.Fatalf("expected the nudge message submitted on its own after the survey was dismissed, got:\n%s", out)
	}
	if keys := readFeedbackSurveyKeyLog(t, keyLog); strings.Count(keys, "0") != 1 {
		t.Fatalf("agent received %q, want exactly one dismiss digit", keys)
	}
}

func TestNudgeSessionIgnoresFeedbackSurveyOnlyInScrollback(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName, keyLog := startFeedbackSurveyAgent(t, tm, "stale")

	history, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	visible, err := tm.CaptureVisiblePane(sessionName)
	if err != nil {
		t.Fatalf("CaptureVisiblePane: %v", err)
	}
	if !strings.Contains(history, "0: Dismiss") || strings.Contains(visible, "0: Dismiss") {
		t.Fatalf("precondition: want the survey in scrollback only; history:\n%s\nvisible:\n%s", history, visible)
	}

	if err := tm.NudgeSession(sessionName, "feedback-nudge-message"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	if keys := readFeedbackSurveyKeyLog(t, keyLog); strings.Contains(keys, "0") {
		t.Fatalf("agent received %q, want no dismiss digit for a survey that is only in scrollback", keys)
	}
	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "SUBMITTED:feedback-nudge-message\n") {
		t.Fatalf("expected the nudge message submitted, got:\n%s", out)
	}
}
