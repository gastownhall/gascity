package tmux

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Captured through the exact-owned d3aae819b supervisor API, not provider files
// or a simulated successful turn. Pre-stop receipt SHA256:
// 6cd1723b2e5be7442842ea0744be95b7f41e6b5979c428561065adb908d6e36f.
func reconnectScreen(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/codex-fatal-reconnect-d3a.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func observeAvailabilityFixture(t *testing.T, screen string, family string, change func(*fenceModel)) *runtime.ProviderAvailability {
	t.Helper()
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = func(*fenceModel) string { return screen }
	if change != nil {
		change(m)
	}
	p := fenceModelProvider(m)
	got := runtime.ObserveProviderAvailability(context.Background(), p, fenceModelSession, runtime.ProviderAvailabilityRequest{Family: family, Fence: testFence})
	bare, events, buffers := m.snapshot()
	if len(bare) != 0 || len(events) != 0 || buffers != 0 {
		t.Fatalf("availability read applied input: bare=%v events=%v buffers=%d", bare, events, buffers)
	}
	for _, call := range m.calls {
		if len(call) == 0 {
			continue
		}
		switch call[0] {
		case "list-panes", "display-message":
		case "capture-pane":
			if strings.Contains(strings.Join(call, " "), "-S") {
				t.Fatalf("availability imported scrollback: %v", call)
			}
		default:
			t.Fatalf("availability used a non-observation command: %v", call)
		}
	}
	return got
}

func TestProviderAvailabilityCapturedCodexReconnectFailure(t *testing.T) {
	for _, screen := range []string{reconnectScreen(t), "\x1b[31m" + reconnectScreen(t) + "\x1b[0m"} {
		got := observeAvailabilityFixture(t, screen, "codex", nil)
		if got == nil || got.State != "unavailable" || got.Code != "connection_restore_failed" || got.Source != "native_ui" {
			t.Fatalf("availability=%+v, want current native reconnect failure", got)
		}
		if got.RuntimeGeneration != testFence.Digest() || got.ObservedAt.IsZero() {
			t.Fatalf("availability lost current runtime evidence: %+v", got)
		}
	}
}

func TestProviderAvailabilityRejectsQuotedHistoricalOrTransientText(t *testing.T) {
	screen := reconnectScreen(t)
	start := strings.Index(screen, "■ Automatic reconnect")
	if start < 0 {
		t.Fatal("captured fixture is missing the native reconnect widget")
	}
	widget := screen[start:]
	for name, text := range map[string]string{
		"quoted user input":       "> " + strings.ReplaceAll(widget, "\n", "\n> "),
		"tool code block":         "```text\n" + widget + "\n```\n› Summarize that example\n  GPT model · /project\n  ⌃c quit\n",
		"indented tool result":    "  " + strings.ReplaceAll(widget, "\n", "\n  "),
		"later native output":     strings.Replace(widget, "› blocker", "• Completed the next turn successfully\n\n› blocker", 1),
		"transient retry":         strings.Replace(widget, "Reconnect failed — check the endpoint, then relaunch", "Reconnecting... attempt 2/5", 1),
		"separated native blocks": strings.Replace(widget, "• Reconnect failed", "Unrelated intervening assistant output\n\n• Reconnect failed", 1),
		"missing native heading":  strings.Replace(widget, "■ Automatic reconnect could not restore this session.", "The tool reported a failure.", 1),
		"healthy composer":        "› Explain reconnect failures\n  GPT model · /project\n  ⌃c quit\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := observeAvailabilityFixture(t, text, "codex", nil)
			if got != nil && (got.State != "unknown" || got.Code != "") {
				t.Fatalf("noncurrent failure text became unavailable: %+v", got)
			}
		})
	}
}

func TestProviderAvailabilityUnsupportedFamilyIsOmitted(t *testing.T) {
	for _, family := range []string{"claude", "my-codex-alias", ""} {
		if got := observeAvailabilityFixture(t, reconnectScreen(t), family, nil); got != nil {
			t.Fatalf("unresolved/unsupported family %q produced availability=%+v", family, got)
		}
	}
}

func TestProviderAvailabilityDoesNotReuseChangedPaneOrIncarnation(t *testing.T) {
	for name, change := range map[string]func(*fenceModel){
		"changed instance": func(m *fenceModel) {
			m.beforeRead = func(m *fenceModel, n int) {
				if n == 2 {
					m.own = "different-digest." + m.paneID
				}
			}
		},
		"changed pane": func(m *fenceModel) {
			m.beforeRead = func(m *fenceModel, n int) {
				if n == 2 {
					m.paneID = "%2"
				}
			}
		},
		"dead pane":           func(m *fenceModel) { m.dead = true },
		"missing own fence":   func(m *fenceModel) { m.own = "" },
		"capture unavailable": func(m *fenceModel) { m.noServer = true },
		"capture failure":     func(m *fenceModel) { m.inner = &fakeExecutor{err: errors.New("capture failed")} },
	} {
		t.Run(name, func(t *testing.T) {
			got := observeAvailabilityFixture(t, reconnectScreen(t), "codex", change)
			if got == nil || got.State != "unknown" || got.Code != "" {
				t.Fatalf("unproven current runtime produced availability=%+v, want unknown", got)
			}
		})
	}
}

func TestProviderAvailabilityCanceledReadStaysUnknown(t *testing.T) {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.screen = func(*fenceModel) string { return reconnectScreen(t) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := runtime.ObserveProviderAvailability(ctx, fenceModelProvider(m), fenceModelSession, runtime.ProviderAvailabilityRequest{Family: "codex", Fence: testFence})
	if got == nil || got.State != "unknown" || got.Code != "" {
		t.Fatalf("canceled read=%+v, want unknown", got)
	}
}
