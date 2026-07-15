package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestPromptDelivery(t *testing.T) {
	const prompt = "do the work"
	quoted := shellquote.Quote(prompt)

	tests := []struct {
		name  string
		promp string
		isACP bool
		rp    *config.ResolvedProvider
		nudge string
		want  promptDeliveryResult
	}{
		{
			name:  "empty prompt delivers nothing",
			promp: "",
			nudge: "wake",
			want:  promptDeliveryResult{Nudge: "wake"},
		},
		{
			name:  "acp prepends to nudge",
			promp: prompt,
			isACP: true,
			nudge: "wake",
			want: promptDeliveryResult{
				Nudge:     prependStartupPromptToNudge(prompt, "wake"),
				Delivered: true,
			},
		},
		{
			name:  "prompt-mode none prepends to nudge",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "none"},
			nudge: "",
			want: promptDeliveryResult{
				Nudge:     prependStartupPromptToNudge(prompt, ""),
				Delivered: true,
			},
		},
		{
			name:  "default arg mode uses quoted suffix",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "arg"},
			nudge: "wake",
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				Nudge:        "wake",
				Delivered:    true,
			},
		},
		{
			name:  "nil provider defaults to quoted suffix",
			promp: prompt,
			rp:    nil,
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				Delivered:    true,
			},
		},
		{
			name:  "flag mode with flag sets both suffix and flag",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "flag", PromptFlag: "--prompt"},
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				PromptFlag:   "--prompt",
				Delivered:    true,
			},
		},
		{
			name:  "flag mode without flag is not delivered",
			promp: prompt,
			rp:    &config.ResolvedProvider{PromptMode: "flag"},
			want: promptDeliveryResult{
				PromptSuffix: quoted,
				Delivered:    false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := promptDelivery(tt.promp, tt.isACP, tt.rp, tt.nudge)
			if got != tt.want {
				t.Errorf("promptDelivery() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
