package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCodexTaskCompleteErrorHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	// Native Codex 0.153.4 terminal authentication failure: there is no
	// separate error event or assistant response carrying this message.
	const message = "Your access token could not be refreshed because your refresh token was revoked. Please log out and sign in again."
	const raw = `{"timestamp":"2026-09-07T21:44:45.631Z","ordinal":10,"type":"event_msg","payload":{"type":"task_complete","turn_id":"01a07dd4-8e6c-7440-b27d-49496bfb20a8","last_agent_message":null,"error":{"message":"Your access token could not be refreshed because your refresh token was revoked. Please log out and sign in again.","codex_error_info":"unauthorized"},"started_at":1788817477,"completed_at":1788817485,"duration_ms":8401}}`
	writeLines(t, path,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"01a07dd4-8e6c-7440-b27d-49496bfb20a8"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"test request"}]}}`, raw)
	load := func() *HistorySnapshot {
		t.Helper()
		snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{Provider: "codex", TranscriptPath: path, GCSessionID: "gc-error"})
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := load()
	if len(before.Entries) != 2 {
		t.Fatalf("entries=%d, want user request and native terminal error", len(before.Entries))
	}
	entry := before.Entries[1]
	if entry.Actor != ActorSystem || entry.Kind != "system" || entry.Status != ResultStatusFinal || entry.Text != message {
		t.Fatalf("terminal error is not readable system history: %+v", entry)
	}
	if event := entry.SystemEvent; event == nil || event.Kind != "error" || event.Category != "provider_error" || event.Code != "unauthorized" || event.Message != message {
		t.Fatalf("terminal error lost typed metadata: %+v", event)
	}
	if entry.ID == "" || string(entry.Provenance.Raw) != raw || entry.Timestamp == nil || entry.Timestamp.Format(time.RFC3339Nano) != "2026-09-07T21:44:45.631Z" {
		t.Fatalf("terminal error lost native identity/provenance: %+v", entry)
	}
	if before.TailState.Activity != TailActivityIdle {
		t.Fatalf("terminal error activity=%q, want idle", before.TailState.Activity)
	}

	// Successful completion can omit error or encode null. Neither may
	// fabricate a failure, and reloading must preserve the earlier error.
	for _, completion := range []string{
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"recovery","last_agent_message":"recovered","error":null}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"recovery","last_agent_message":"recovered"}}`,
	} {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString("\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"recovery"}}` + "\n" +
			`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered"}]}}` + "\n" + completion + "\n")
		closeErr := f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		after := load()
		if len(after.Entries) != len(before.Entries)+1 {
			t.Fatalf("successful completion produced extra history: entries=%d, want %d", len(after.Entries), len(before.Entries)+1)
		}
		final := after.Entries[len(after.Entries)-1]
		if final.Actor != ActorAssistant || final.SystemEvent != nil || final.Text != "recovered" || after.TailState.Activity != TailActivityIdle {
			t.Fatalf("successful completion became an error: entry=%+v tail=%+v", final, after.TailState)
		}
		if !reflect.DeepEqual(before.Entries, after.Entries[:len(before.Entries)]) {
			t.Fatal("append/reload changed prior error history")
		}
		before = after
	}
}
