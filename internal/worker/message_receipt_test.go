package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

type receiptRuntime struct {
	*runtime.Fake
	store   *beads.MemStore
	deliver func()
	calls   int
	err     error
}

func (r *receiptRuntime) Nudge(_ string, _ []runtime.ContentBlock) error {
	r.calls++
	r.deliver()
	return r.err
}

func (r *receiptRuntime) NudgeNow(name string, content []runtime.ContentBlock) error {
	return r.Nudge(name, content)
}

func newReceiptHandle(t *testing.T) (*SessionHandle, *receiptRuntime, string, []string, string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := &receiptRuntime{Fake: runtime.NewFake(), store: store, err: tmux.ErrNudgeSubmitUnconfirmed}
	manager := sessionpkg.NewManagerWithOptions(store, sp)
	root, workDir := t.TempDir(), t.TempDir()
	h, err := NewSessionHandle(SessionHandleConfig{Manager: manager, SearchPaths: []string{root}, Session: SessionSpec{
		Profile: ProfileClaudeTmuxCLI, Template: "probe", Title: "Probe", Command: "claude", WorkDir: workDir, Provider: "claude",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.SetMetadata(h.sessionID, "session_key", "aac2c6e6-7b31-4b02-ac5a-3b37e49c536f"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, sessionlog.ProjectSlug(workDir))
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "aac2c6e6-7b31-4b02-ac5a-3b37e49c536f.jsonl")
	// Captured Claude 2.1.263 message records, with parent links collapsed
	// across omitted ancillary records: the user input was native at 22:05:27.559,
	// submit was declared unconfirmed at 22:05:30.054, and the answer finished
	// at 22:05:30.332. A sampled terminal spinner cannot prove non-delivery.
	data, err := os.ReadFile("testdata/claude_fast_submit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	writeLines(t, path, lines[:2]...)
	var user struct {
		Message struct{ Content string }
	}
	if err = json.Unmarshal([]byte(lines[2]), &user); err != nil {
		t.Fatal(err)
	}
	return h, sp, path, lines[2:], user.Message.Content
}

func appendReceiptLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("\n" + strings.Join(lines, "\n") + "\n")
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestMessageConfirmsNativeReceiptWithoutBusyIndicator(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "in progress", true: "completed"}[completed], func(t *testing.T) {
			h, sp, path, lines, prompt := newReceiptHandle(t)
			sp.deliver = func() {
				if completed {
					appendReceiptLines(t, path, lines...)
				} else {
					appendReceiptLines(t, path, lines[0])
				}
			}
			result, err := h.Message(context.Background(), MessageRequest{Text: prompt})
			if err != nil || result.Queued || sp.calls != 1 {
				t.Fatalf("native accepted prompt: result=%+v deliveries=%d error=%v", result, sp.calls, err)
			}
			sp.deliver = func() {}
			if _, err = h.Message(context.Background(), MessageRequest{Text: prompt}); !errors.Is(err, tmux.ErrNudgeSubmitUnconfirmed) {
				t.Fatalf("old receipt accepted a later undelivered identical prompt: %v", err)
			}
			if sp.calls != 2 {
				t.Fatalf("deliveries=%d, want exactly one per request", sp.calls)
			}
		})
	}
}

func TestMessagePreservesUnconfirmedWhenNativeProofIsIncomplete(t *testing.T) {
	for _, scenario := range []string{"partial prompt", "extra text block", "extra image block", "duplicate receipt", "malformed tail", "rewritten history", "replaced file", "different conversation", "missing baseline", "other error"} {
		t.Run(scenario, func(t *testing.T) {
			h, sp, path, lines, prompt := newReceiptHandle(t)
			if scenario == "missing baseline" {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
			}
			sp.deliver = func() {
				switch scenario {
				case "partial prompt":
					appendReceiptLines(t, path, `{"uuid":"partial","type":"user","message":{"role":"user","content":"BEGIN_908041b095a8 END_908041b095a8"}}`)
				case "extra text block", "extra image block":
					var entry map[string]any
					if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
						t.Fatal(err)
					}
					extra := map[string]string{"type": "text", "text": "Unintended additional instruction"}
					if scenario == "extra image block" {
						extra = map[string]string{"type": "image", "image_url": "data:image/png;base64,AA=="}
					}
					entry["message"].(map[string]any)["content"] = []map[string]string{
						{"type": "text", "text": prompt}, extra,
					}
					raw, err := json.Marshal(entry)
					if err != nil {
						t.Fatal(err)
					}
					appendReceiptLines(t, path, string(raw))
				case "duplicate receipt":
					appendReceiptLines(t, path, lines[0], strings.ReplaceAll(lines[0], "0f87726d-9ba0-4007-baec-f89f8c5c3945", "duplicate-user"))
				case "malformed tail":
					appendReceiptLines(t, path, lines[0], `{"type":`)
				case "rewritten history":
					writeLines(t, path, lines...)
				case "replaced file":
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					writeLines(t, path+".replacement", string(data), lines[0])
					if err = os.Rename(path+".replacement", path); err != nil {
						t.Fatal(err)
					}
				case "different conversation":
					appendReceiptLines(t, path, lines...)
					if err := sp.store.SetMetadata(h.sessionID, "session_key", "other-native"); err != nil {
						t.Fatal(err)
					}
				case "missing baseline":
					writeLines(t, path, lines...)
				case "other error":
					appendReceiptLines(t, path, lines...)
					sp.err = errors.New("runtime disconnected")
				}
			}
			if _, err := h.Message(context.Background(), MessageRequest{Text: prompt}); !errors.Is(err, sp.err) {
				t.Fatalf("uncertain delivery lost its original error: %v", err)
			}
			if sp.calls != 1 {
				t.Fatalf("deliveries=%d, want one", sp.calls)
			}
		})
	}
}
