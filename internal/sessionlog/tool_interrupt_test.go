package sessionlog

import (
	"encoding/json"
	"testing"
)

func TestToolUseInterruptActivity(t *testing.T) {
	for _, marker := range []string{"[Request interrupted by user]", "[Request interrupted by user for tool use]", "  [Request interrupted by user for tool use]\n"} {
		for _, form := range []string{"string", "blocks", "wrapped"} {
			t.Run(marker+"/"+form, func(t *testing.T) {
				var content any = marker
				if form != "string" {
					content = []map[string]string{{"type": "text", "text": marker}}
				}
				message, err := json.Marshal(map[string]any{"role": "user", "content": content})
				if err != nil {
					t.Fatal(err)
				}
				if form == "wrapped" {
					message, err = json.Marshal(string(message))
					if err != nil {
						t.Fatal(err)
					}
				}
				if got := InferActivity("user", "", message); got != "idle" {
					t.Fatalf("interrupt activity = %q, want idle", got)
				}
				entries := []*Entry{
					{Type: "assistant", Message: json.RawMessage(`{"role":"assistant","stop_reason":"tool_use"}`)},
					{Type: "user", Message: message},
				}
				if got := InferActivityFromEntries(entries); got != "idle" {
					t.Fatalf("interrupted stream activity = %q, want idle", got)
				}
			})
		}
	}
	for _, text := range []string{"Request interrupted by user for tool use", "[Request interrupted by user for tool use", "[Request interrupted by user for another reason]", "Please interrupt the tool", "Explain [Request interrupted by user for tool use]", "[Request interrupted by user for tool use] and continue"} {
		message, err := json.Marshal(map[string]any{"role": "user", "content": text})
		if err != nil {
			t.Fatal(err)
		}
		if got := InferActivity("user", "", message); got != "in-turn" {
			t.Errorf("ordinary user content %q activity = %q", text, got)
		}
	}
}
