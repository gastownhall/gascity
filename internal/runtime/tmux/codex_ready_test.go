package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type codexReadinessExecutor struct {
	pane     string
	captures [][]string
}

func (f *codexReadinessExecutor) execute(args []string) (string, error) {
	if args[1] == "show-environment" {
		key := args[len(args)-1]
		if key == "GC_PROVIDER" {
			return key + "=codex", nil
		}
		return key + "=› ", nil
	}
	if args[1] == "capture-pane" {
		f.captures = append(f.captures, args)
		return f.pane, nil
	}
	return "", errors.New("unexpected command: " + strings.Join(args, " "))
}

func (f *codexReadinessExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func TestCodexWaitForIdleRejectsStartupComposer(t *testing.T) {
	for _, pane := range []string{
		"│ model: loading /model to change │\n│ directory: loading │\n› Ask Codex to do anything\n? for shortcuts",
		"Hooks need review\n4 hooks are new or changed.\n› 1. Review hooks\n2. Trust all and continue\n3. Continue without trusting (hooks won't run)",
		"• Booting MCP server: codex_apps (1s • esc to interrupt)\n› Ask Codex to do anything",
	} {
		fe := &codexReadinessExecutor{pane: pane}
		tm := NewTmux()
		tm.exec = fe
		ctx, cancel := context.WithTimeout(context.Background(), 450*time.Millisecond)
		err := tm.WaitForIdle(ctx, "custom-codex-worker", time.Second)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("startup pane accepted as idle: %q; err=%v", pane, err)
		}
	}
}

func TestCodexWaitForIdleUsesCurrentViewport(t *testing.T) {
	fe := &codexReadinessExecutor{pane: "• Done\n\n› Ask Codex to do anything\n\n  gpt-5.5 low · /private/tmp/own-workspace"}
	tm := NewTmux()
	tm.exec = fe
	if err := tm.WaitForIdle(context.Background(), "custom-codex-worker", time.Second); err != nil {
		t.Fatal(err)
	}
	for _, args := range fe.captures {
		if args[len(args)-1] != "-0" {
			t.Fatalf("readiness must exclude prior-process scrollback: %v", args)
		}
	}
}
