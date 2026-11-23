//go:build windows

package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// rawLaunchTyped reports whether the fake's call log shows a raw launch typed
// into pane %5 the way launchRaw types one on this platform.
func rawLaunchTyped(calls string) bool {
	return strings.Contains(calls, "pane run %5 "+promptLaunchPrefix)
}

// assertTypedLaunchRuns checks that the raw path typed the runtime's own
// launch of command: the statement's encoded script is exactly the one
// runtime.ShellCommandPrompt renders for it. What that script holds around the
// command is the runtime's to pin.
func assertTypedLaunchRuns(t *testing.T, state, command string) {
	t.Helper()
	want, err := runtime.ShellCommandPrompt(command)
	if err != nil {
		t.Fatalf("ShellCommandPrompt: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(state, "rawtoken")); string(got) != want.EncodedCommand {
		script, _ := os.ReadFile(filepath.Join(state, "rawcmd"))
		t.Errorf("the typed launch runs %q, want the runtime's launch of %q", script, command)
	}
}

// TestStartRawPathTypesAPowerShellLaunch pins what the raw path types on
// Windows: the runtime's PowerShell launch of the command, run by the
// interpreter the runtime resolves. Start returns once that interpreter shows
// up under the pane's shell, not after rawLaunchWait.
func TestStartRawPathTypesAPowerShellLaunch(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	const command = `python3 worker.py --queue "main" C:\work\`
	started := time.Now()
	if err := p.Start(context.Background(), "gastown__worker", runtime.Config{Command: command}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if took := time.Since(started); took >= rawLaunchWait {
		t.Errorf("Start took %s; the launched interpreter was never seen, only waited out", took)
	}
	calls := fakeCalls(t, state)
	if !rawLaunchTyped(calls) {
		t.Fatalf("Start did not type a PowerShell launch into the pane:\n%s", calls)
	}
	assertTypedLaunchRuns(t, state, command)
	want, err := runtime.ShellCommandPrompt("exit 0")
	if err != nil {
		t.Fatalf("ShellCommandPrompt: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(state, "rawinterp")); string(got) != want.Interpreter {
		t.Errorf("the typed launch starts %q, want the runtime's interpreter %q", got, want.Interpreter)
	}
}

// TestStartRawPathRefusesANonPowerShellPane pins the refusal: the statement is
// PowerShell, and typed into another shell it would fail in a way the screen
// can make look like output. Nothing is typed.
func TestStartRawPathRefusesANonPowerShellPane(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "shell_cmd")

	err := p.Start(context.Background(), "gastown__worker", runtime.Config{Command: "python3 worker.py"})
	if err == nil || !strings.Contains(err.Error(), "cmd.exe") || !strings.Contains(err.Error(), "PowerShell") {
		t.Fatalf("Start in a cmd.exe pane = %v; want a refusal naming the shell and PowerShell", err)
	}
	if calls := fakeCalls(t, state); strings.Contains(calls, "pane run") {
		t.Fatalf("a launch was typed into a cmd.exe pane:\n%s", calls)
	}
}

// TestStartRawPathWaitsForThePaneShellToBeListed pins the race a live run
// found: a fresh pane's process-info can report the shell's PID before it lists
// the shell (measured, herdr 0.9.1: about 130 ms in one of three panes), and a
// launch that decided from that first read refused the pane as an unidentified
// shell.
func TestStartRawPathWaitsForThePaneShellToBeListed(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "shell_late")

	const command = "python3 worker.py"
	if err := p.Start(context.Background(), "gastown__worker", runtime.Config{Command: command}); err != nil {
		t.Fatalf("Start in a pane whose shell is listed late: %v", err)
	}
	assertTypedLaunchRuns(t, state, command)
}

// TestStartRawPathToleratesAFailedShellRead pins that reading the pane's shell
// retries a read that fails, as the Unix launch, which types without reading
// first, never fails on one. Only a pane that is gone ends the wait early.
func TestStartRawPathToleratesAFailedShellRead(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	setState(t, state, "processinfo_flaky")

	const command = "python3 worker.py"
	if err := p.Start(context.Background(), "gastown__worker", runtime.Config{Command: command}); err != nil {
		t.Fatalf("Start after two failed shell reads: %v", err)
	}
	assertTypedLaunchRuns(t, state, command)

	gone, goneState := newFakeHerdrProvider(t)
	listenHerdrSocket(t, gone)
	setState(t, goneState, "pane_gone")
	started := time.Now()
	if _, err := gone.awaitPaneShell(context.Background(), "%5"); err == nil || !paneGone(err) {
		t.Fatalf("awaitPaneShell on a gone pane = %v, want herdr's not-found error", err)
	}
	if took := time.Since(started); took >= paneShellReadyWait {
		t.Errorf("a gone pane took %s to report; it should not wait out paneShellReadyWait", took)
	}
}

// TestLaunchLandedNeedsTheLaunchesOwnCommandLine pins what counts as the
// launch having landed: a child of the launch's interpreter image whose
// command line carries the launch's encoded command. An interpreter the shell
// started for itself has the right image and may be newer than the launch, and
// it satisfied the image-and-time rule this replaced (measured).
func TestLaunchLandedNeedsTheLaunchesOwnCommandLine(t *testing.T) {
	launch := runtime.PromptLaunch{Interpreter: `C:\Program Files\PowerShell\7\pwsh.exe`, EncodedCommand: "ZQB4AGkAdAAgADAA"}
	job := proctable.ChildProcess{PID: 10, Name: "pwsh.exe", Created: time.Unix(0, 1)}
	unreadable := proctable.ChildProcess{PID: 11, Name: "pwsh.exe", Created: time.Unix(0, 2)}
	lookalike := proctable.ChildProcess{PID: 12, Name: "cmd.exe", Created: time.Unix(0, 3)}
	ours := proctable.ChildProcess{PID: 13, Name: "PWSH.EXE", Created: time.Unix(0, 4)}
	lines := map[int]string{
		job.PID:       `"C:\Program Files\PowerShell\7\pwsh.exe" -s -NoLogo -NoProfile`,
		lookalike.PID: `cmd.exe /c echo -EncodedCommand ` + launch.EncodedCommand,
		ours.PID:      `"C:\Program Files\PowerShell\7\pwsh.exe" -NoProfile -EncodedCommand ` + launch.EncodedCommand,
	}
	reads := map[int]int{}
	commandLine := func(child proctable.ChildProcess) (string, error) {
		reads[child.PID]++
		if line, ok := lines[child.PID]; ok {
			return line, nil
		}
		return "", errors.New("access is denied")
	}
	attached := func(children ...proctable.ChildProcess) []paneChild {
		var out []paneChild
		for _, child := range children {
			out = append(out, paneChild{ChildProcess: child, Attached: true})
		}
		return out
	}

	rejected := map[launchCandidate]bool{}
	if launchLanded(nil, launch, commandLine, rejected) {
		t.Error("no children counted as the launch")
	}
	if launchLanded(attached(job, unreadable, lookalike), launch, commandLine, rejected) {
		t.Error("a background interpreter, an unreadable one, or another program carrying the token counted as the launch")
	}
	if reads[lookalike.PID] != 0 {
		t.Error("the command line of a child of another image was read")
	}
	if !launchLanded(attached(job, unreadable, ours), launch, commandLine, rejected) {
		t.Error("the interpreter carrying the launch's encoded command did not count as the launch")
	}
	if reads[job.PID] != 1 {
		t.Errorf("the background interpreter's command line was read %d times; once should settle it", reads[job.PID])
	}
	if reads[unreadable.PID] != 2 {
		t.Errorf("an unreadable command line was read %d times; it should be tried again on each poll", reads[unreadable.PID])
	}
}

// TestWaitPaneLaunchedIgnoresAnUnrelatedInterpreter drives the wait itself: an
// interpreter of the launch's own image that the shell started for itself does
// not end it, and the launch's interpreter does.
func TestWaitPaneLaunchedIgnoresAnUnrelatedInterpreter(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	launch, err := runtime.ShellCommandPrompt("python3 worker.py")
	if err != nil {
		t.Fatalf("ShellCommandPrompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(state, "detached_child"), []byte(filepath.Base(launch.Interpreter)), 0o644); err != nil {
		t.Fatal(err)
	}
	setState(t, state, "launch_pending")
	ctx := context.Background()
	if err := p.c.paneRunCommand(ctx, "%5", launch.Statement); err != nil {
		t.Fatalf("typing the launch: %v", err)
	}

	const hold = 1500 * time.Millisecond
	waitCtx, cancel := context.WithTimeout(ctx, hold)
	defer cancel()
	started := time.Now()
	p.waitPaneLaunched(waitCtx, "%5", launch)
	if took := time.Since(started); took < hold-100*time.Millisecond {
		t.Fatalf("the wait ended after %s, before the launch's interpreter started; an unrelated %s ended it", took, filepath.Base(launch.Interpreter))
	}

	if err := os.Remove(filepath.Join(state, "launch_pending")); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	p.waitPaneLaunched(ctx, "%5", launch)
	if took := time.Since(started); took >= 5*time.Second {
		t.Fatalf("the wait took %s after the launch's interpreter started", took)
	}
}
