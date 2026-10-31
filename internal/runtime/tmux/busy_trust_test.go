package tmux

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// familyStaticPane is a tmux executor for an agent pane of provider family
// (its GC_PROVIDER; none when family is empty) whose capture is always screen.
// The pane's process is pid, noPaneProcess when empty. envErr, when set, is
// what reading GC_PROVIDER fails with.
type familyStaticPane struct {
	family string
	screen string
	pid    string
	envErr error
}

func (f *familyStaticPane) execute(args []string) (string, error) {
	pid := f.pid
	if pid == "" {
		pid = noPaneProcess
	}
	if row, ok := fakeAgentPaneRowWithPID(args, pid); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "display-message":
		// A resolved pane target reads its process directly (readAgentPane).
		if args[len(args)-1] == "#{pane_pid}" {
			return pid, nil
		}
	case "show-environment":
		if len(args) > 0 && args[len(args)-1] == "GC_PROVIDER" {
			if f.envErr != nil {
				return "", f.envErr
			}
			if f.family != "" {
				return "GC_PROVIDER=" + f.family, nil
			}
		}
		return "", errors.New("unknown variable")
	case "capture-pane":
		return f.screen, nil
	}
	return "", nil
}

func (f *familyStaticPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

// noPaneProcess is a pane pid no process can have (it exceeds every pid_max),
// so a process sniff of the pane matches no family on any host.
const noPaneProcess = "999999999"

// startPaneProcess starts a process named name (sleep, linked under that
// name) for an agent pane to report as its pane_pid, so the leaf's process
// sniff finds a real process of that name, and returns its pid.
func startPaneProcess(t *testing.T, name string) string {
	t.Helper()
	sleep, err := osexec.LookPath("sleep")
	if err != nil {
		t.Fatalf("finding sleep: %v", err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.Symlink(sleep, path); err != nil {
		t.Fatalf("linking sleep as %s: %v", name, err)
	}
	cmd := osexec.Command(path, "300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return strconv.Itoa(cmd.Process.Pid)
}

// errProviderReadFailed is a GC_PROVIDER read that reached no answer: the
// server failed, as opposed to answering that the variable is unset.
var errProviderReadFailed = errors.New("server exited unexpectedly")

const (
	busyTrustBusyPane    = "✻ Thinking… (12s · esc to interrupt)"
	busyTrustIdlePane    = "● Done.\n\n❯ \n  ? for shortcuts"
	busyTrustNeitherPane = "Broadcast message from root@host: DISK ALERT"
	// busyTrustAnsweredApprovalTail follows an answered permission prompt: the
	// tool ran and the idle composer is back underneath the prompt's frame.
	busyTrustAnsweredApprovalTail = "\n\n● Read 3 lines.\n\n❯ \n  ? for shortcuts"
)

// wantReading checks one SnapshotIdle or TurnRunning error against a row: a
// failed read (wantErr) is an error that is not ErrIdleIndeterminate, an
// indeterminate one wraps it, and any other reading has no error.
func wantReading(t *testing.T, err error, wantErr, wantIndeterminate bool) {
	t.Helper()
	switch indeterminate := errors.Is(err, runtime.ErrIdleIndeterminate); {
	case wantErr:
		if err == nil || indeterminate {
			t.Errorf("err = %v, want a failed read that is not ErrIdleIndeterminate", err)
		}
	case wantIndeterminate:
		if !indeterminate {
			t.Errorf("err = %v, want ErrIdleIndeterminate", err)
		}
	case err != nil:
		t.Errorf("err = %v, want nil", err)
	}
}

// The busy reading of the pane scan is policy the leaf owns: only the
// families whose indicator the scan reads (submitVerifyEligibleFamilies:
// claude, codex) may report it, named by the pane's GC_PROVIDER or, without
// one, by the process the pane runs. Every other family, and a pane whose
// family cannot be told, reports the busy-indicator string as
// runtime.ErrIdleIndeterminate, so a caller that holds on busy holds nothing
// there without knowing which families are trusted. A busy capture whose
// GC_PROVIDER read fails is a failed read, which callers hold on, not
// indeterminate. The idle reading and the indeterminate one are untouched for
// every family.
func TestSnapshotIdleUntrustedFamilyIsIndeterminate(t *testing.T) {
	for _, tc := range []struct {
		name              string
		family            string
		process           string
		envErr            error
		screen            string
		wantIdle          bool
		wantIndeterminate bool
		wantErr           bool
	}{
		{name: "claude busy", family: "claude", screen: busyTrustBusyPane},
		{name: "codex busy", family: "codex", screen: busyTrustBusyPane},
		{name: "gemini busy", family: "gemini", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "opencode busy", family: "opencode", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "unidentified pane busy", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "unidentified pane running claude busy", process: "claude", screen: busyTrustBusyPane},
		{name: "unidentified pane running codex busy", process: "codex", screen: busyTrustBusyPane},
		{name: "provider read failed busy", process: "claude", envErr: errProviderReadFailed, screen: busyTrustBusyPane, wantErr: true},
		{name: "provider read failed idle", envErr: errProviderReadFailed, screen: busyTrustIdlePane, wantIdle: true},
		{name: "gemini idle", family: "gemini", screen: busyTrustIdlePane, wantIdle: true},
		{name: "claude idle", family: "claude", screen: busyTrustIdlePane, wantIdle: true},
		{name: "gemini neither", family: "gemini", screen: busyTrustNeitherPane, wantIndeterminate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := &familyStaticPane{family: tc.family, screen: tc.screen, envErr: tc.envErr}
			if tc.process != "" {
				pane.pid = startPaneProcess(t, tc.process)
			}
			tm := NewTmux()
			tm.exec = pane

			idle, err := tm.SnapshotIdle("lab")

			if idle != tc.wantIdle {
				t.Errorf("idle = %t, want %t", idle, tc.wantIdle)
			}
			wantReading(t, err, tc.wantErr, tc.wantIndeterminate)
		})
	}
}

// TurnRunning answers only for a family whose turn the leaf can read, the
// family SnapshotIdle trusts: the pane's GC_PROVIDER or, without one, the
// process the pane runs. For every other family it reports
// runtime.ErrIdleIndeterminate, whatever the screen shows: neither a
// busy-indicator string nor its absence says whether such a family's turn
// runs. A GC_PROVIDER read that fails is a failed read, not indeterminate.
func TestTurnRunningUntrustedFamilyIsIndeterminate(t *testing.T) {
	for _, tc := range []struct {
		name              string
		family            string
		process           string
		envErr            error
		screen            string
		wantRunning       bool
		wantIndeterminate bool
		wantErr           bool
	}{
		{name: "claude busy", family: "claude", screen: busyTrustBusyPane, wantRunning: true},
		{name: "claude idle", family: "claude", screen: busyTrustIdlePane},
		{name: "gemini busy", family: "gemini", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "gemini idle", family: "gemini", screen: busyTrustIdlePane, wantIndeterminate: true},
		{name: "unidentified pane busy", screen: busyTrustBusyPane, wantIndeterminate: true},
		{name: "unidentified pane running claude busy", process: "claude", screen: busyTrustBusyPane, wantRunning: true},
		{name: "unidentified pane running codex busy", process: "codex", screen: busyTrustBusyPane, wantRunning: true},
		{name: "provider read failed", process: "claude", envErr: errProviderReadFailed, screen: busyTrustBusyPane, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pane := &familyStaticPane{family: tc.family, screen: tc.screen, envErr: tc.envErr}
			if tc.process != "" {
				pane.pid = startPaneProcess(t, tc.process)
			}
			tm := NewTmux()
			tm.exec = pane

			running, err := (&Provider{tm: tm}).TurnRunning("lab")

			if running != tc.wantRunning {
				t.Errorf("running = %t, want %t", running, tc.wantRunning)
			}
			wantReading(t, err, tc.wantErr, tc.wantIndeterminate)
		})
	}
}

// The wait-idle pre-read over the tmux leaf (runtime.IdleBoundaryNow): an
// open permission prompt or AskUserQuestion dialog reads indeterminate from
// the pane scan and the leaf's Pending names it, so the pre-read is pending
// and the nudge queues at once. The same dialogs answered, with the idle
// composer back underneath, read at the boundary, a busy claude pane reads
// busy, and a busy pane of a family the leaf does not trust, or a pane
// showing neither a prompt nor a dialog, cannot tell.
func TestIdleBoundaryNowReadsTmuxPane(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family string
		screen string
		want   runtime.IdleBoundary
	}{
		{name: "open approval", family: "claude", screen: approvalPromptPane(), want: runtime.IdleBoundaryPending},
		{name: "open question", family: "claude", screen: askUserQuestionPane, want: runtime.IdleBoundaryPending},
		{name: "answered approval", family: "claude", screen: approvalPromptPane() + busyTrustAnsweredApprovalTail, want: runtime.IdleBoundaryAt},
		{name: "answered question", family: "claude", screen: answeredAskUserQuestionPane, want: runtime.IdleBoundaryAt},
		{name: "claude busy", family: "claude", screen: busyTrustBusyPane, want: runtime.IdleBoundaryBusy},
		{name: "gemini busy", family: "gemini", screen: busyTrustBusyPane, want: runtime.IdleBoundaryUnknown},
		{name: "neither", family: "claude", screen: busyTrustNeitherPane, want: runtime.IdleBoundaryUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := NewTmux()
			tm.exec = &familyStaticPane{family: tc.family, screen: tc.screen}

			if got := runtime.IdleBoundaryNow(&Provider{tm: tm}, "lab"); got != tc.want {
				t.Errorf("IdleBoundaryNow = %v, want %v", got, tc.want)
			}
		})
	}
}
