//go:build integration

package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// buildBusyOnEnterBinary compiles a fake agent TUI that echoes stdin and, after
// receiving GC_TEST_BUSY_AFTER Enter keystrokes (default 1), prints an
// "esc to interrupt" busy footer — the same signal paneContainsBusyIndicator
// uses to detect a live Claude turn. Each Enter also prints an "ENTER#<n>"
// marker so a test can assert exactly how many submit keystrokes were delivered.
func buildBusyOnEnterBinary(t *testing.T, dir, name string) string {
	t.Helper()
	bin := dir + "/" + name
	src := dir + "/" + name + ".go"
	prog := `package main
import ("bufio";"fmt";"os";"strconv")
func main(){
	busyAfter:=1
	if v:=os.Getenv("GC_TEST_BUSY_AFTER"); v!=""{ if n,err:=strconv.Atoi(v); err==nil && n>0 { busyAfter=n } }
	enters:=0
	r:=bufio.NewReader(os.Stdin)
	for{
		b,err:=r.ReadByte()
		if err!=nil{ return }
		if b=='\r'||b=='\n'{
			enters++
			fmt.Printf("\nENTER#%d\n", enters)
			if enters>=busyAfter { fmt.Print("esc to interrupt\n") }
			continue
		}
		_,_=os.Stdout.Write([]byte{b})
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

// TestNudgeSessionConfirmsSubmitForClaude proves the verified-submit path
// against real tmux: a single Enter that submits drives the fake agent busy,
// NudgeSession confirms it, and does NOT issue a redundant second Enter.
func TestNudgeSessionConfirmsSubmitForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-nudge-confirm-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER":        "claude",
		"GC_TEST_BUSY_AFTER": "1",
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	if err := tm.NudgeSession(sessionName, "hello-confirm"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("pane never reached submitted/busy state:\n%s", out)
	}
	if strings.Contains(out, "ENTER#2") {
		t.Fatalf("issued a redundant Enter after the turn already submitted (double-submit):\n%s", out)
	}
}

// TestNudgeSessionReEntersUntilSubmittedForClaude proves the ga-bwm fix
// end-to-end on real tmux: when the first Enter is dropped (the draft stays in
// the input box), NudgeSession re-sends Enter, and the second submit drives the
// agent busy. Pre-fix, the message would sit "drafted but not submitted".
func TestNudgeSessionReEntersUntilSubmittedForClaude(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	dir := t.TempDir()
	fake := buildBusyOnEnterBinary(t, dir, "fakeclaude")
	sessionName := fmt.Sprintf("gt-test-nudge-reenter-%d", time.Now().UnixNano()%100000)

	_ = tm.KillSession(sessionName)
	if err := tm.NewSessionWithCommandAndEnv(sessionName, dir, fake, map[string]string{
		"GC_PROVIDER":        "claude",
		"GC_TEST_BUSY_AFTER": "2", // drop the first Enter, submit on the second
	}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	defer func() { _ = tm.KillSession(sessionName) }()
	time.Sleep(300 * time.Millisecond)

	if err := tm.NudgeSession(sessionName, "hello-reenter"); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	out, err := tm.CapturePaneAll(sessionName)
	if err != nil {
		t.Fatalf("CapturePaneAll: %v", err)
	}
	if !strings.Contains(out, "ENTER#2") {
		t.Fatalf("did not re-send Enter after the first was dropped:\n%s", out)
	}
	if !strings.Contains(out, "esc to interrupt") {
		t.Fatalf("never reached submitted/busy state after re-send:\n%s", out)
	}
}
