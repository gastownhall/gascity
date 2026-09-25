package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// An ACP agent message grows in place under one entry id while chunks
// arrive. The conversation and raw streams send each entry once, so they hold
// a still-growing message back until it settles instead of sending it
// truncated; the structured stream upserts it by id.
func TestSessionStreamDeliversCompleteGrowingACPMessage(t *testing.T) {
	for _, format := range []string{"", "raw", "structured"} {
		t.Run("format="+format, func(t *testing.T) {
			isolateProviderDiscovery(t)
			fs := newSessionFakeState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)
			srv.sessionLogSearchPaths = []string{t.TempDir()}
			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
				Template:  "myrig/worker",
				Title:     "Chat",
				Command:   "unreal-acp",
				WorkDir:   t.TempDir(),
				Provider:  "unreal-acp",
				Transport: "acp",
				ExtraMeta: map[string]string{"session_origin": "manual"},
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			path, err := citylayout.ACPTranscriptPath(fs.CityPath(), info.ID, "1")
			if err != nil {
				t.Fatalf("ACPTranscriptPath: %v", err)
			}
			chunk := func(text string) string {
				return `{"ts":"2026-09-25T10:00:02Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"` + text + `"}}}}}`
			}
			head := strings.Join([]string{
				`{"gc_acp_capture":1,"ts":"2026-09-25T10:00:00Z","session_id":"` + info.ID + `","runtime_epoch":"1","pid":1}`,
				`{"ts":"2026-09-25T10:00:01Z","dir":"out","msg":{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"What is the codeword?"}]}}}`,
				chunk("ALPHA-"),
			}, "\n") + "\n"
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(path, []byte(head), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			url := cityURL(fs, "/session/") + info.ID + "/stream"
			if format != "" {
				url += "?format=" + format
			}
			rec := newSyncResponseRecorder()
			done := make(chan struct{})
			go func() {
				h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil).WithContext(ctx))
				close(done)
			}()
			first := "What is the codeword?"
			if format == "structured" {
				first = "ALPHA-"
			}
			if body := waitForRecorderSubstring(t, rec, first, 3*time.Second); !strings.Contains(body, first) {
				t.Fatalf("stream never showed %q: %s", first, body)
			}

			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			_, werr := f.WriteString(chunk("BRAVO") + "\n" + `{"ts":"2026-09-25T10:00:03Z","dir":"in","msg":{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}}` + "\n")
			if cerr := f.Close(); werr != nil || cerr != nil {
				t.Fatalf("append: %v %v", werr, cerr)
			}
			body := waitForRecorderSubstring(t, rec, "BRAVO", 3*time.Second)
			cancel()
			<-done
			// Conversation and structured frames carry the reassembled text;
			// a raw frame carries every chunk record of the message.
			complete := strings.Contains(body, "ALPHA-BRAVO")
			if format == "raw" {
				complete = false
				for _, line := range strings.Split(body, "\n") {
					if strings.Contains(line, `"text":"ALPHA-"`) && strings.Contains(line, `"text":"BRAVO"`) {
						complete = true
					}
				}
			}
			if !complete {
				t.Fatalf("format=%q: stream never delivered the complete message:\n%s", format, body)
			}
			if format != "structured" && strings.Count(body, "ALPHA-") != 1 {
				t.Errorf("format=%q: the message was sent %d times, want once, complete:\n%s", format, strings.Count(body, "ALPHA-"), body)
			}
		})
	}
}

// The agent output stream sends each turn once after its entry-id cursor, so
// it too holds a still-growing ACP message back until it settles rather than
// sending it truncated and moving the cursor past it.
func TestAgentOutputStreamHumaDeliversCompleteGrowingACPMessage(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	path, err := citylayout.ACPTranscriptPath(fs.CityPath(), "gc-1", "1")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	chunk := func(text string) string {
		return `{"ts":"2026-09-25T10:00:02Z","dir":"in","msg":{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-1","update":{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"` + text + `"}}}}}`
	}
	head := strings.Join([]string{
		`{"gc_acp_capture":1,"ts":"2026-09-25T10:00:00Z","session_id":"gc-1","runtime_epoch":"1","pid":1}`,
		`{"ts":"2026-09-25T10:00:01Z","dir":"out","msg":{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"acp-1","prompt":[{"type":"text","text":"What is the codeword?"}]}}}`,
		chunk("ALPHA-"),
	}, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(head), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var mu sync.Mutex
	var texts []string
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
	send := sse.Sender(func(msg sse.Message) error {
		if resp, ok := msg.Data.(agentOutputResponse); ok {
			mu.Lock()
			for _, turn := range resp.Turns {
				texts = append(texts, turn.Role+":"+turn.Text)
			}
			mu.Unlock()
		}
		return nil
	})
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			for _, text := range sent() {
				if strings.Contains(text, want) {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("stream never sent %q; sent %q", want, sent())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		srv.streamSessionLogHuma(ctx, send, "myrig/worker", "unreal-acp", path, nil, wake)
		close(done)
	}()
	waitFor("What is the codeword?")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, werr := f.WriteString(chunk("BRAVO") + "\n" + `{"ts":"2026-09-25T10:00:03Z","dir":"in","msg":{"jsonrpc":"2.0","id":2,"result":{"stopReason":"end_turn"}}}` + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		t.Fatalf("append: %v %v", werr, cerr)
	}
	wake <- struct{}{}
	waitFor("BRAVO")
	cancel()
	<-done

	got := sent()
	want := []string{"user:What is the codeword?", "assistant:ALPHA-BRAVO"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("sent turns = %q, want %q: the reply complete and exactly once", got, want)
	}
}

func TestSettledHistorySnapshotDropsPartialEntries(t *testing.T) {
	snapshot := &worker.HistorySnapshot{Entries: []worker.HistoryEntry{
		{ID: "a", Status: worker.ResultStatusFinal},
		{ID: "b", Status: worker.ResultStatusPartial},
	}}
	settled := settledHistorySnapshot(snapshot)
	if len(settled.Entries) != 1 || settled.Entries[0].ID != "a" {
		t.Fatalf("settled entries = %+v, want only the final entry", settled.Entries)
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("input snapshot mutated: %+v", snapshot.Entries)
	}
	final := &worker.HistorySnapshot{Entries: []worker.HistoryEntry{{ID: "a", Status: worker.ResultStatusFinal}}}
	if settledHistorySnapshot(final) != final {
		t.Error("a snapshot with no partial entries was copied, want it returned as is")
	}
	if settledHistorySnapshot(nil) != nil {
		t.Error("nil snapshot, want nil")
	}
}
