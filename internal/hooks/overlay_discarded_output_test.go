package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDiscardedOutputOverlaysRunNoConsumptiveHooks expresses the copilot and
// cursor half of ga-m4xy49. gc treats a successful stdout write from
// `gc nudge drain` and `gc mail check` as delivery: it terminalizes the
// drained nudges and archives auto-handoff mail right after the write.
// Copilot CLI ignores the output of its userPromptSubmitted command hooks, and
// Cursor's beforeSubmitPrompt can only return continue/user_message (its hook
// output must be JSON and gc prints plain text), so on both the write reaches
// no model and every nudge or handoff those hooks drained is lost.
//
// Dropping `--inject` is not enough: the plain `nudge drain` form also acks
// the items it prints (ackQueuedNudges), and a plain `mail check` prints a
// count nobody sees. The overlays therefore run neither command from any
// hook event, and the supervisor's own delivery (the conversation lane host
// and the nudge poller) carries those messages instead.
func TestDiscardedOutputOverlaysRunNoConsumptiveHooks(t *testing.T) {
	overlays := []struct {
		provider string
		path     string
	}{
		{"copilot", "overlay/per-provider/copilot/.github/hooks/gascity.json"},
		{"cursor", "overlay/per-provider/cursor/.cursor/hooks.json"},
	}
	consumptive := []string{"nudge drain", "mail check"}

	for _, overlay := range overlays {
		t.Run(overlay.provider, func(t *testing.T) {
			events := overlayHookEventCommands(t, overlay.path)

			for event, commands := range events {
				for _, command := range commands {
					for _, banned := range consumptive {
						if strings.Contains(command, banned) {
							t.Errorf("%s %s runs %q, which terminalizes or archives on a stdout write the provider discards:\n  %s",
								overlay.provider, event, banned, command)
						}
					}
				}
			}

			// The overlay must keep its session-start prime and its
			// pre-compaction handoff: removing the consumptive hooks is not a
			// permission to empty the file.
			assertEventRuns(t, overlay.provider, events, "sessionStart", "prime --hook")
			assertEventRuns(t, overlay.provider, events, "preCompact", "handoff --auto")
		})
	}
}

// overlayHookEventCommands parses a copilot- or cursor-style hooks file,
// {"hooks": {"<event>": [{"bash"|"command": "..."}]}}, into event -> commands.
func overlayHookEventCommands(t *testing.T, relPath string) map[string][]string {
	t.Helper()
	data := readOverlayFile(t, relPath)

	var parsed struct {
		Hooks map[string][]struct {
			Bash    string `json:"bash"`
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing %s: %v", relPath, err)
	}

	events := make(map[string][]string, len(parsed.Hooks))
	for event, entries := range parsed.Hooks {
		for _, entry := range entries {
			command := entry.Bash
			if command == "" {
				command = entry.Command
			}
			events[event] = append(events[event], command)
		}
	}
	return events
}

func assertEventRuns(t *testing.T, provider string, events map[string][]string, event, want string) {
	t.Helper()
	for _, command := range events[event] {
		if strings.Contains(command, want) {
			return
		}
	}
	t.Errorf("%s %s must still run %q, got %q", provider, event, want, events[event])
}
