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

// buildFeedbackSurveyAgent compiles a fake agent TUI that renders Claude
// Code's post-turn feedback survey and holds it until it receives the
// dismiss key ("0" then Enter) -- mirroring the bundle-verified onDigit
// contract from ga-zg7fjq: an option only fires once inputValue is exactly
// one digit, and Enter confirms immediately without waiting out the
// debounce. Once dismissed it behaves like a normal composer: it echoes
// stdin back and prints an "esc to interrupt" busy footer on Enter (the same
// signal paneContainsBusyIndicator checks for), so a test can prove a nudge
// message survived the survey instead of being swallowed by it.
func buildFeedbackSurveyAgent(t *testing.T, dir, name string) string {
	t.Helper()
	bin := dir + "/" + name
	src := dir + "/" + name + ".go"
	prog := `package main
import ("bufio";"fmt";"os")
func main(){
	fmt.Println("⏺ Done — pushed the branch and replied on the PR.")
	fmt.Println()
	fmt.Println("● How is Claude doing this session? (optional)")
	fmt.Println("  1: Bad    2: Fine   3: Good   0: Dismiss")
	fmt.Println()
	fmt.Println("╭──────────────────────────────────────────────────────────╮")
	fmt.Println("│ ❯                                                        │")
	fmt.Println("╰──────────────────────────────────────────────────────────╯")
	fmt.Println("  ⏵⏵ bypass permissions on (shift+tab to cycle)")

	dismissed := false
	pendingZero := false
	r := bufio.NewReader(os.Stdin)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		if !dismissed {
			switch b {
			case '0':
				pendingZero = true
			case '\r', '\n':
				if pendingZero {
					dismissed = true
					fmt.Println()
					fmt.Println("SURVEY_DISMISSED")
				}
				pendingZero = false
			}
			continue
		}
		if b == '\r' || b == '\n' {
			fmt.Println()
			fmt.Println("esc to interrupt")
			continue
		}
		_, _ = os.Stdout.Write([]byte{b})
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
	dir := t.TempDir()
	fake := buildFeedbackSurveyAgent(t, dir, "fakesurvey")
	sessionName := fmt.Sprintf("gt-test-feedback-survey-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER": "claude",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

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
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "feedback-nudge-message" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected the nudge message delivered as its own line after the survey was dismissed, got:\n%s", out)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("pane never reached submitted/busy state after the nudge:\n%s", out)
	}
}
