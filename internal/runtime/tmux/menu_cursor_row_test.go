package tmux

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Codex marks the selected option of a startup dialog with its prompt glyph, so
// the row matches ready_prompt_prefix "› " while the pane still waits for an
// answer. The frames are tmux captures of codex-cli 0.157.0: the folder-trust
// dialog, the hook review that follows it, and the background-server notice.
const (
	codexTrustDialogPane = `
  Folder access
  /home/dev/city

  Trust this folder? Codex can read, edit, and run files here, subject to your permission settings. Folder settings
  can run code automatically, even without a model request. Continue only if you trust these files. Your trust
  decision will be saved.

› 1. Trust and continue
  2. Quit

  enter continue · esc quit
`
	codexHookReviewPane = `
  Hooks need review
  8 hooks are new or changed

› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting
`
	codexDaemonNoticePane = `
  Background server has incompatible feature settings
  This session requires api_key_model_discovery to be disabled

  1. Run without daemon this time
  2. Restart with these settings
› 3. Cancel
`
)

// Kills: a readiness or idle scan that takes any line starting with the prompt
// glyph as the composer, and a guard that rejects the whole frame instead of
// the menu row (the composer below a numbered transcript line must still read
// as a prompt).
func TestMenuCursorRowIsNotAReadyPrompt(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{"codex folder-trust dialog", codexTrustDialogPane, false},
		{"codex hook review dialog", codexHookReviewPane, false},
		{"codex background-server notice, cursor on the last row", codexDaemonNoticePane, false},
		{"boxed menu", "│ › 1. Trust and continue\n│   2. Quit", false},
		{"composer", "› ", true},
		{"composer with placeholder", "› Ask Codex to do anything", true},
		{"composer draft that starts with a number", "› 1. fix the parser\n\n  gpt-5 · ~/city", true},
		{"composer below a numbered message in the transcript", "› 1. fix the parser\n  2. add a test\n\n• Done.\n\n› \n\n  gpt-5 · ~/city", true},
		{"numbers that are not neighbors", "› 1. fix the parser\n  3. unrelated", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := strings.Split(tt.pane, "\n")
			if got := PaneShowsIdlePrompt(lines, "› "); got != tt.want {
				t.Errorf("PaneShowsIdlePrompt = %v, want %v", got, tt.want)
			}
			tm := &Tmux{cfg: DefaultConfig(), exec: &paneExecutor{pane: tt.pane}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := tm.WaitForRuntimeReady(ctx, "worker", &RuntimeConfig{
				Tmux: &RuntimeTmuxConfig{ReadyPromptPrefix: "› "},
			}, 300*time.Millisecond)
			if got := err == nil; got != tt.want {
				t.Errorf("WaitForRuntimeReady ready = %v (err %v), want %v", got, err, tt.want)
			}
		})
	}
}

// paneExecutor answers every tmux call with one fixed pane capture.
type paneExecutor struct {
	pane string
}

func (p *paneExecutor) execute([]string) (string, error) {
	return p.pane, nil
}

func (p *paneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return p.execute(args)
}
