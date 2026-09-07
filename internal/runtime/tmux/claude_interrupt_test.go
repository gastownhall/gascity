package tmux

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type claudeInterruptExecutor struct {
	panes      []string
	captureErr error
	captures   [][]string
	keys       []string
}

func (f *claudeInterruptExecutor) execute(args []string) (string, error) {
	switch args[1] {
	case "show-environment":
		key := args[len(args)-1]
		if key == "GC_PROVIDER" {
			return key + "=claude", nil
		}
		return key + "=❯ ", nil
	case "capture-pane":
		f.captures = append(f.captures, args)
		if f.captureErr != nil {
			return "", f.captureErr
		}
		pane := f.panes[0]
		if len(f.panes) > 1 {
			f.panes = f.panes[1:]
		}
		return pane, nil
	case "send-keys":
		f.keys = append(f.keys, args[len(args)-1])
		return "", nil
	default:
		return "", errors.New("unexpected command: " + strings.Join(args, " "))
	}
}

func (f *claudeInterruptExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestClaudeInterruptCancelsApprovalWithoutSelectingPermission(t *testing.T) {
	for _, tc := range []struct {
		name, pane, want string
	}{
		{"approval", currentClaudeApprovalPane(t), "Escape"},
		{"ordinary turn", "Working (esc to interrupt)\n❯ ", "C-c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &claudeInterruptExecutor{panes: []string{tc.pane}}
			tm := NewTmux()
			tm.exec = fe
			if err := (&Provider{tm: tm}).Interrupt("custom-claude-worker"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fe.keys, []string{tc.want}) {
				t.Fatalf("interrupt keys = %q, want exactly %q", fe.keys, tc.want)
			}
		})
	}
}

func TestClaudeInterruptCaptureFailureSendsNoKeys(t *testing.T) {
	want := errors.New("capture unavailable")
	fe := &claudeInterruptExecutor{captureErr: want}
	tm := NewTmux()
	tm.exec = fe
	if err := (&Provider{tm: tm}).Interrupt("custom-claude-worker"); !errors.Is(err, want) {
		t.Fatalf("Interrupt = %v, want capture error", err)
	}
	if len(fe.keys) != 0 {
		t.Fatalf("sent keys without observing the current pane: %q", fe.keys)
	}
}

func TestClaudeWaitForIdleRejectsApprovalAndMenu(t *testing.T) {
	for _, pane := range []string{currentClaudeApprovalPane(t), "Earlier input\n❯ \nChoose an action\n❯ 1. Continue\n  2. Cancel"} {
		fe := &claudeInterruptExecutor{panes: []string{pane}}
		tm := NewTmux()
		tm.exec = fe
		ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
		err := tm.WaitForIdle(ctx, "custom-claude-worker", time.Second)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("modal accepted as idle: %v", err)
		}
	}
}

func TestClaudeInterruptSettlesOnlyAfterCurrentComposerReturns(t *testing.T) {
	fe := &claudeInterruptExecutor{panes: []string{
		currentClaudeApprovalPane(t), currentClaudeApprovalPane(t),
		"Processing (esc to interrupt)\n❯ ", "Interrupted\n❯ ", "Interrupted\n❯ ",
	}}
	tm := NewTmux()
	tm.exec = fe
	if err := (&Provider{tm: tm}).Interrupt("custom-claude-worker"); err != nil {
		t.Fatal(err)
	}
	if err := tm.WaitForIdle(context.Background(), "custom-claude-worker", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(fe.captures) != 5 || !reflect.DeepEqual(fe.keys, []string{"Escape"}) {
		t.Fatalf("interrupt settled too early or sent more keys: captures=%d keys=%q", len(fe.captures), fe.keys)
	}
	for _, args := range fe.captures {
		if args[len(args)-1] != "-0" {
			t.Fatalf("observed history instead of current viewport: %q", args)
		}
	}
}
