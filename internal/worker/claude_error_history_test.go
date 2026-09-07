package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestClaudeNativeErrorHistory(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, transient := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%t/transient=%t", native, transient), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "conversation.jsonl")
				user := `{"uuid":"u1","type":"user","timestamp":"2026-09-06T00:00:01Z","message":{"role":"user","content":"test request"}}`
				raw := fmt.Sprintf(`{"uuid":"e1","parentUuid":"u1","type":"assistant","timestamp":"2026-09-06T00:00:02Z","isApiErrorMessage":%t,"apiErrorIsTransient":%t,"error":"authentication_failed","message":{"role":"assistant","model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"API Error: request denied"}]}}`, native, transient)
				writeLines(t, path, user, raw)
				load := func() *HistorySnapshot {
					t.Helper()
					snapshot, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{Provider: "claude/tmux-cli", TranscriptPath: path, GCSessionID: "gc-owned"})
					if err != nil {
						t.Fatal(err)
					}
					return snapshot
				}
				before := load()
				if len(before.Entries) != 2 {
					t.Fatalf("entries=%d", len(before.Entries))
				}
				entry := before.Entries[1]
				if entry.ID != "e1" || entry.Provenance.RawType != "assistant" || string(entry.Provenance.Raw) != raw || entry.Status != ResultStatusFinal {
					t.Fatalf("native identity/provenance changed: %+v", entry)
				}
				if native {
					if entry.Actor != ActorSystem || entry.Kind != "system" || entry.SystemEvent == nil || entry.SystemEvent.Kind != "error" || entry.SystemEvent.Category != "provider_error" || entry.SystemEvent.Code != "authentication_failed" || entry.SystemEvent.Message != "API Error: request denied" {
						t.Fatalf("native error is not a typed system event: %+v", entry)
					}
				} else if entry.Actor != ActorAssistant || entry.SystemEvent != nil {
					t.Fatalf("ordinary model text reclassified: %+v", entry)
				}
				if before.TailState.Activity != TailActivityIdle {
					t.Fatalf("terminal error activity=%q", before.TailState.Activity)
				}
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("\n" + `{"uuid":"u2","parentUuid":"e1","type":"user","timestamp":"2026-09-06T00:00:03Z","message":{"role":"user","content":"later turn"}}` + "\n")
				closeErr := f.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				after := load()
				if !reflect.DeepEqual(before.Entries, after.Entries[:2]) {
					t.Fatal("append/reload rewrote prior error history")
				}
			})
		}
	}
}

func TestClaudeActiveRetryHistoryThenSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	writeLines(t, path,
		`{"uuid":"a1","type":"assistant","timestamp":"2026-09-06T00:00:01Z","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"earlier response"}]}}`,
		`{"uuid":"r1","parentUuid":"a1","type":"system","subtype":"api_error","timestamp":"2026-09-06T00:00:02Z","error":{"status":503,"message":"server unavailable"},"retryInMs":1000,"retryAttempt":1,"maxRetries":3}`)
	load := func() *HistorySnapshot {
		t.Helper()
		s, err := (SessionLogAdapter{}).LoadHistory(LoadRequest{Provider: "claude", TranscriptPath: path, GCSessionID: "gc-owned"})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := load()
	retry := before.Entries[1]
	if retry.Actor != ActorSystem || retry.SystemEvent == nil || retry.SystemEvent.Kind != "retry" || retry.SystemEvent.Category != "provider_retry" {
		t.Fatalf("retry event=%+v", retry)
	}
	if before.TailState.Activity != TailActivityInTurn {
		t.Fatalf("active retry inherited old idle: %q", before.TailState.Activity)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n" + `{"uuid":"a2","parentUuid":"r1","type":"assistant","timestamp":"2026-09-06T00:00:03Z","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"recovered"}]}}` + "\n")
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	after := load()
	if after.TailState.Activity != TailActivityIdle || after.Entries[2].Actor != ActorAssistant || after.Entries[2].SystemEvent != nil {
		t.Fatal("retry prevented normal assistant completion")
	}
	if !reflect.DeepEqual(before.Entries, after.Entries[:2]) {
		t.Fatal("retry metadata changed after recovery")
	}
}
