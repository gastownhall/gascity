package tmux

import (
	"errors"
	"strings"
	"testing"
)

// newSliceTestTmux returns a Tmux backed by a fakeExecutor with the agent
// slice probe stubbed to succeed, plus the executor for argv inspection.
func newSliceTestTmux(t *testing.T) (*Tmux, *fakeExecutor) {
	t.Helper()
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	tm.agentSlice.probe = func(string) error { return nil }
	tm.agentSlice.warn = &strings.Builder{}
	return tm, exec
}

func TestAgentSliceWrapsNewSessionWithCommand(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	if err := tm.NewSessionWithCommand("gc-test-slice", "/work", "exec env GT_ROLE=crew claude"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	want := "systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'exec env GT_ROLE=crew claude'"
	if got != want {
		t.Fatalf("pane command = %q, want %q", got, want)
	}
}

func TestAgentSliceWrapsNewSessionWithCommandAndEnv(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	env := map[string]string{"LANG": "en_US.UTF-8", "LC_ALL": ""}
	if err := tm.NewSessionWithCommandAndEnv("gc-test-slice-env", "/work", "claude", env); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	if len(exec.calls) == 0 {
		t.Fatal("no tmux calls recorded")
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	// The env -u prefix must end up INSIDE the scope wrapper so the unset
	// still applies to the agent process.
	want := "systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'env -u LC_ALL claude'"
	if got != want {
		t.Fatalf("pane command = %q, want %q", got, want)
	}
	// The -e session env flags must survive wrapping.
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "\x00-e\x00LANG=en_US.UTF-8\x00") {
		t.Fatalf("new-session args missing LANG -e flag: %v", args)
	}
}

func TestAgentSliceWrapsRespawnPane(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	if err := tm.RespawnPane("%0", "claude --resume"); err != nil {
		t.Fatalf("RespawnPane: %v", err)
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	want := "systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'claude --resume'"
	if got != want {
		t.Fatalf("respawn command = %q, want %q", got, want)
	}

	if err := tm.RespawnPaneWithWorkDir("%0", "/work", "claude --resume"); err != nil {
		t.Fatalf("RespawnPaneWithWorkDir: %v", err)
	}
	args = exec.calls[1]
	if got := args[len(args)-1]; got != want {
		t.Fatalf("respawn-with-workdir command = %q, want %q", got, want)
	}
}

func TestAgentSliceUnsetLeavesCommandPlain(t *testing.T) {
	t.Setenv(AgentSliceEnv, "")
	tm, exec := newSliceTestTmux(t)

	if err := tm.NewSessionWithCommand("gc-test-plain", "/work", "claude"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	args := exec.calls[0]
	if got := args[len(args)-1]; got != "claude" {
		t.Fatalf("pane command = %q, want plain %q", got, "claude")
	}
}

func TestAgentSliceProbeFailureFallsBackPlainWithWarning(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	probeCalls := 0
	exec := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = exec
	var warnings strings.Builder
	tm.agentSlice.probe = func(string) error {
		probeCalls++
		return errors.New("user manager not responding")
	}
	tm.agentSlice.warn = &warnings

	if err := tm.NewSessionWithCommand("gc-test-fallback", "/work", "claude"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if err := tm.NewSessionWithCommand("gc-test-fallback2", "/work", "claude"); err != nil {
		t.Fatalf("NewSessionWithCommand (second): %v", err)
	}

	for i, call := range exec.calls[:2] {
		if call[len(call)-1] != "claude" && call[0] == "new-session" {
			t.Fatalf("call %d pane command = %q, want plain %q", i, call[len(call)-1], "claude")
		}
	}
	if probeCalls != 1 {
		t.Fatalf("probe called %d times, want 1 (result must be cached)", probeCalls)
	}
	if !strings.Contains(warnings.String(), "user manager not responding") {
		t.Fatalf("warning output missing probe error: %q", warnings.String())
	}
	if !strings.Contains(warnings.String(), AgentSliceEnv) {
		t.Fatalf("warning output missing env var name: %q", warnings.String())
	}
}

func TestAgentSliceEmptyCommandNotWrapped(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	// Empty command + env-only session must keep the empty trailing arg so
	// tmux still starts the default shell.
	if err := tm.NewSessionWithCommandAndEnv("gc-test-empty", "/work", "", map[string]string{"LANG": "C"}); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	args := exec.calls[0]
	if got := args[len(args)-1]; got != "" {
		t.Fatalf("pane command = %q, want empty", got)
	}
}

func TestAgentSliceQuotesEmbeddedSingleQuotes(t *testing.T) {
	t.Setenv(AgentSliceEnv, "gascity-agents.slice")
	tm, exec := newSliceTestTmux(t)

	if err := tm.NewSessionWithCommand("gc-test-quote", "/work", "claude --msg 'hi there'"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	args := exec.calls[0]
	got := args[len(args)-1]
	want := `systemd-run --user --scope --slice=gascity-agents.slice --collect --quiet -- sh -c 'claude --msg '\''hi there'\'''`
	if got != want {
		t.Fatalf("pane command = %q, want %q", got, want)
	}
}
