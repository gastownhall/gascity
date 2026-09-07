package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Appending a Codex turn must not remove or move older usage/model metadata,
// even after its records leave the telemetry tail and a new reader is created.
func TestCodexHistoryMetadataStableAcrossAppendAndReload(t *testing.T) {
	for _, mode := range []string{"outside tail window", "duplicate after model switch"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			token := `{"timestamp":"2026-01-02T00:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":150},"last_token_usage":{"input_tokens":110,"cached_input_tokens":10,"output_tokens":40,"total_tokens":150},"model_context_window":258400}}}`
			writeLines(t, path,
				`{"timestamp":"2026-01-02T00:00:01Z","type":"turn_context","payload":{"model":"gpt-5.5"}}`,
				`{"timestamp":"2026-01-02T00:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first answer"}]}}`, token)
			load := func() *HistorySnapshot {
				t.Helper()
				// No retained handle/cache may be required for canonical history.
				got, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{Provider: "codex", TranscriptPath: path, GCSessionID: "gc-stable"})
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			before := load()
			if len(before.Entries) != 1 || before.Entries[0].Usage == nil || before.Entries[0].Model != "gpt-5.5" {
				t.Fatalf("missing initial metadata: %+v", before.Entries)
			}
			var appendLines []string
			if mode == "outside tail window" {
				padding, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]string{"type": "test_padding", "text": strings.Repeat("x", 70*1024)}})
				if err != nil {
					t.Fatal(err)
				}
				appendLines = append(appendLines, string(padding))
			}
			appendLines = append(appendLines,
				`{"timestamp":"2026-01-02T00:00:04Z","type":"turn_context","payload":{"model":"gpt-new"}}`,
				`{"timestamp":"2026-01-02T00:00:05Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second answer"}]}}`)
			if mode == "duplicate after model switch" {
				appendLines = append(appendLines, strings.Replace(token, "00:00:03Z", "00:00:06Z", 1))
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString(strings.Join(appendLines, "\n") + "\n")
			closeErr := f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			after := load()
			if len(after.Entries) != 2 {
				t.Fatalf("entries=%d", len(after.Entries))
			}
			if !reflect.DeepEqual(before.Entries[0], after.Entries[0]) {
				t.Fatalf("append rewrote earlier metadata: before model=%q usage=%+v; after model=%q usage=%+v", before.Entries[0].Model, before.Entries[0].Usage, after.Entries[0].Model, after.Entries[0].Usage)
			}
			page, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{Provider: "codex", TranscriptPath: path, GCSessionID: "gc-stable", BeforeEntryID: after.Entries[1].ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Entries) != 1 || !reflect.DeepEqual(page.Entries[0], before.Entries[0]) {
				t.Fatal("older page lost its own invocation metadata")
			}
			if after.Entries[1].Usage != nil {
				t.Fatalf("old usage moved to new assistant: %+v", after.Entries[1].Usage)
			}
		})
	}
}
