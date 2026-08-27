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

	"github.com/gastownhall/gascity/internal/runtime"
)

// buildModelSwitchModalAgent compiles a fake agent that prints the Codex/GPT
// "approaching rate limits — switch model?" modal, then treats the next Enter as
// confirming "Keep current model" by printing a MODEL_KEPT marker. Everything
// typed before that Enter is echoed back as MODAL_INPUT and everything typed
// after it as PROMPT_INPUT, so a test can tell which side of the modal a nudge
// landed on. It stays alive so the tmux pane persists for capture.
func buildModelSwitchModalAgent(t *testing.T, dir, name string) string {
	t.Helper()
	bin := dir + "/" + name
	src := dir + "/" + name + ".go"
	prog := `package main
import ("bufio";"fmt";"os";"strings")
func main(){
	fmt.Println("Approaching rate limits")
	fmt.Println("Switch to gpt-5.4-mini for lower credit usage?")
	fmt.Println("  1. Switch to gpt-5.4-mini")
	fmt.Println("  2. Keep current model")
	fmt.Println("Press enter to confirm or esc to go back")
	r:=bufio.NewReader(os.Stdin)
	var typed strings.Builder
	modal:=true
	for{
		b,err:=r.ReadByte()
		if err!=nil{ return }
		if b=='\r'||b=='\n'{
			if modal{
				fmt.Println("MODAL_INPUT:"+typed.String())
				fmt.Println("MODEL_KEPT modal dismissed keeping current model")
				// Redraw like a real TUI: the modal leaves the screen once it is
				// dismissed, so a later observer sees the prompt, not the dialog.
				fmt.Print("\033[2J\033[H")
				fmt.Println("> ")
				modal=false
			}else{
				fmt.Println("PROMPT_INPUT:"+typed.String())
			}
			typed.Reset()
			continue
		}
		if b>=0x20&&b<0x7f{ typed.WriteByte(b) }
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

// waitForPaneText polls capture until it contains want, so these tests observe a
// real pane's state instead of guessing at a fixed delay. Returns the capture
// that matched.
func waitForPaneText(t *testing.T, capture func() (string, error), want, what string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		content, err := capture()
		if err != nil {
			t.Fatalf("capture while waiting for %s: %v", what, err)
		}
		if strings.Contains(content, want) {
			return content
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s:\n%s", what, content)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestClearModelSwitchModalOnRealTmux proves ga-3syh's fix end-to-end on real
// tmux: a session showing the mid-session model-switch modal is dismissed to
// "Keep current model" (Down+Enter) so it stops hanging.
func TestClearModelSwitchModalOnRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildModelSwitchModalAgent(t, dir, "fakecodex")
	sessionName := fmt.Sprintf("gt-test-modal-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER": "codex",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()

	capture := func() (string, error) { return tm.CapturePaneAll(sessionName) }
	waitForPaneText(t, capture, "Keep current model", "the model-switch modal")

	if err := tm.clearModelSwitchModal(sessionName, ""); err != nil {
		t.Fatalf("clearModelSwitchModal: %v", err)
	}
	waitForPaneText(t, capture, "MODEL_KEPT", "the modal to be dismissed")
}

// TestNudgeNeverTypesIntoAModelSwitchModalOnRealTmux is the #3916 / ga-3syh
// production signature end-to-end: tmux cannot see a TUI modal, so a nudge
// delivered while one is up pastes its text into the dialog and turns its own
// Enter into a confirmed model switch. The nudge path has to clear the modal
// first, then deliver — the payload must land on the prompt, never on the modal.
func TestNudgeNeverTypesIntoAModelSwitchModalOnRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	cfg := DefaultConfig()
	cfg.SocketName = fmt.Sprintf("gctest-modal-nudge-%d-%d", os.Getpid(), time.Now().UnixNano())
	cfg.NudgeIdleTimeout = 0
	p := NewProviderWithConfig(cfg)
	t.Cleanup(func() { _ = p.TeardownServer() })

	dir := t.TempDir()
	fake := buildModelSwitchModalAgent(t, dir, "fakecodex")
	name := "modal-nudge"
	if err := p.Start(context.Background(), name, runtime.Config{
		Command:      fake,
		ProviderName: "codex",
		WorkDir:      dir,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	payload := fmt.Sprintf("claim-your-work-%d", time.Now().UnixNano())
	capture := func() (string, error) { return p.tm.CapturePaneAll(name) }
	waitForPaneText(t, capture, "Keep current model", "the model-switch modal")

	if err := p.Nudge(name, runtime.TextContent(payload)); err != nil {
		t.Fatalf("Nudge against a model-switch modal: %v", err)
	}
	pane := waitForPaneText(t, capture, "PROMPT_INPUT:", "the nudge to reach the prompt")
	for _, line := range strings.Split(pane, "\n") {
		if strings.HasPrefix(line, "MODAL_INPUT:") && strings.Contains(line, payload) {
			t.Fatalf("nudge payload was typed into the modal: %q\n%s", line, pane)
		}
	}
	if !strings.Contains(pane, "MODEL_KEPT") {
		t.Fatalf("modal was never dismissed:\n%s", pane)
	}
	delivered := false
	for _, line := range strings.Split(pane, "\n") {
		if strings.HasPrefix(line, "PROMPT_INPUT:") && strings.Contains(line, payload) {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("nudge payload never landed on the prompt:\n%s", pane)
	}
}
