package sessionlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadProviderFilePaginationMatrix(t *testing.T) {
	t.Parallel()

	providers := []string{
		"claude/tmux-cli",
		"auggie/tmux-cli",
		"amp/tmux-cli",
		"codex/tmux-cli",
		"copilot/tmux-cli",
		"cursor/tmux-cli",
		"grok/tmux-cli",
		"kiro/tmux-cli",
		"gemini/tmux-cli",
		"kimi/tmux-cli",
		"mimocode/tmux-cli",
		"opencode/tmux-cli",
		"pi/tmux-cli",
		"antigravity/tmux-cli",
	}

	for _, provider := range providers {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			path := writePaginationProviderFixture(t, ProviderFamily(provider))

			for _, raw := range []bool{false, true} {
				raw := raw
				t.Run(fmt.Sprintf("raw=%t", raw), func(t *testing.T) {
					all, err := readPaginationProviderFile(provider, path, raw)
					if err != nil {
						t.Fatalf("read full provider transcript: %v", err)
					}
					if len(all.Messages) != 3 {
						t.Fatalf("full message IDs = %v, want exactly three fixture entries", paginationEntryIDs(all.Messages))
					}
					cursor := all.Messages[1].UUID
					if strings.TrimSpace(cursor) == "" {
						t.Fatal("middle fixture entry has an empty cursor ID")
					}

					older, err := readPaginationProviderPage(provider, path, raw, cursor, "")
					if err != nil {
						t.Fatalf("read older page: %v", err)
					}
					assertPaginationPage(t, older, paginationEntryIDs(all.Messages[:1]), 3, false, true)

					newer, err := readPaginationProviderPage(provider, path, raw, "", cursor)
					if err != nil {
						t.Fatalf("read newer page: %v", err)
					}
					assertPaginationPage(t, newer, paginationEntryIDs(all.Messages[2:]), 3, true, false)

					for _, direction := range []struct {
						before        string
						after         string
						wantDirection CursorDirection
					}{
						{before: "missing-entry", wantDirection: CursorDirectionBefore},
						{after: "missing-entry", wantDirection: CursorDirectionAfter},
					} {
						page, pageErr := readPaginationProviderPage(provider, path, raw, direction.before, direction.after)
						if pageErr == nil {
							t.Fatalf("unknown cursor returned IDs %v, want an error", paginationEntryIDs(page.Messages))
						}
						if !errors.Is(pageErr, ErrCursorNotFound) {
							t.Fatalf("unknown cursor error = %v, want ErrCursorNotFound", pageErr)
						}
						var cursorErr *CursorNotFoundError
						if !errors.As(pageErr, &cursorErr) {
							t.Fatalf("unknown cursor error type = %T, want *CursorNotFoundError", pageErr)
						}
						if cursorErr.EntryID != "missing-entry" || cursorErr.Direction != direction.wantDirection {
							t.Fatalf("cursor error = %+v, want entry missing-entry direction %s", cursorErr, direction.wantDirection)
						}
					}
				})
			}
		})
	}
}

func TestPaginationInfoHasNewerMessagesWireCompatibility(t *testing.T) {
	withoutNewer, err := json.Marshal(PaginationInfo{})
	if err != nil {
		t.Fatalf("marshal pagination without newer messages: %v", err)
	}
	if strings.Contains(string(withoutNewer), `"has_newer_messages"`) {
		t.Fatalf("false has_newer_messages must be omitted to preserve existing no-cursor JSON: %s", withoutNewer)
	}

	withNewer, err := json.Marshal(PaginationInfo{HasNewerMessages: true})
	if err != nil {
		t.Fatalf("marshal pagination with newer messages: %v", err)
	}
	if !strings.Contains(string(withNewer), `"has_newer_messages":true`) {
		t.Fatalf("true has_newer_messages missing from pagination JSON: %s", withNewer)
	}
}

func TestReadProviderFileEmptyCursorPreservesNoCursorReads(t *testing.T) {
	t.Parallel()

	providers := []string{
		"claude/tmux-cli",
		"auggie/tmux-cli",
		"amp/tmux-cli",
		"codex/tmux-cli",
		"copilot/tmux-cli",
		"cursor/tmux-cli",
		"grok/tmux-cli",
		"kiro/tmux-cli",
		"gemini/tmux-cli",
		"kimi/tmux-cli",
		"mimocode/tmux-cli",
		"opencode/tmux-cli",
		"pi/tmux-cli",
		"antigravity/tmux-cli",
	}

	for _, provider := range providers {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			path := writePaginationProviderFixture(t, ProviderFamily(provider))

			for _, raw := range []bool{false, true} {
				for _, tailCompactions := range []int{0, 1} {
					var (
						want *Session
						err  error
					)
					if raw {
						want, err = ReadProviderFileRaw(provider, path, tailCompactions)
					} else {
						want, err = ReadProviderFile(provider, path, tailCompactions)
					}
					if err != nil {
						t.Fatalf("read no-cursor control: %v", err)
					}
					for _, read := range []struct {
						name string
						fn   func() (*Session, error)
					}{
						{
							name: "older",
							fn: func() (*Session, error) {
								if raw {
									return ReadProviderFileRawOlder(provider, path, tailCompactions, "")
								}
								return ReadProviderFileOlder(provider, path, tailCompactions, "")
							},
						},
						{
							name: "newer",
							fn: func() (*Session, error) {
								if raw {
									return ReadProviderFileRawNewer(provider, path, tailCompactions, "")
								}
								return ReadProviderFileNewer(provider, path, tailCompactions, "")
							},
						},
					} {
						got, err := read.fn()
						if err != nil {
							t.Fatalf("%s empty-cursor read: %v", read.name, err)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("%s empty-cursor read with tail=%d changed no-cursor result\n got: %#v\nwant: %#v", read.name, tailCompactions, got, want)
						}
					}
				}
			}
		})
	}
}

func TestReadProviderFilePageRejectsDuplicateEntryIDs(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "events.jsonl")
	body := strings.Join([]string{
		`{"type":"user.message","data":{"content":"zero"},"id":"duplicate"}`,
		`{"type":"assistant.message","data":{"content":"one"},"id":"duplicate"}`,
		`{"type":"user.message","data":{"content":"two"},"id":"copilot-2"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write duplicate fixture: %v", err)
	}
	for _, read := range []struct {
		name string
		fn   func() (*Session, error)
	}{
		{name: "conversation snapshot", fn: func() (*Session, error) {
			return ReadProviderFile("copilot/tmux-cli", path, 0)
		}},
		{name: "raw snapshot", fn: func() (*Session, error) {
			return ReadProviderFileRaw("copilot/tmux-cli", path, 0)
		}},
	} {
		if snapshot, err := read.fn(); !errors.Is(err, ErrDuplicateEntryID) {
			t.Fatalf("%s = IDs %v, error %v; want ErrDuplicateEntryID", read.name, paginationEntryIDs(snapshot.Messages), err)
		}
	}

	page, err := ReadProviderFileNewer("copilot/tmux-cli", path, 0, "duplicate")
	if err == nil {
		t.Fatalf("duplicate cursor returned IDs %v, want an error", paginationEntryIDs(page.Messages))
	}
	if !errors.Is(err, ErrDuplicateEntryID) {
		t.Fatalf("duplicate cursor error = %v, want ErrDuplicateEntryID", err)
	}
	var duplicateErr *DuplicateEntryIDError
	if !errors.As(err, &duplicateErr) {
		t.Fatalf("duplicate cursor error type = %T, want *DuplicateEntryIDError", err)
	}
	if duplicateErr.EntryID != "duplicate" {
		t.Fatalf("duplicate cursor entry ID = %q, want duplicate", duplicateErr.EntryID)
	}
}

func TestProviderSpecificPageReadersRejectUnknownCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		read func(string) (*Session, error)
	}{
		{
			name: "claude conversation",
			read: func(path string) (*Session, error) {
				return ReadFileNewer(path, 0, "missing-entry")
			},
		},
		{
			name: "claude raw",
			read: func(path string) (*Session, error) {
				return ReadFileRawOlder(path, 0, "missing-entry")
			},
		},
		{
			name: "kimi",
			read: func(path string) (*Session, error) {
				return ReadKimiFilePage(path, 0, "", "missing-entry")
			},
		},
		{
			name: "antigravity conversation",
			read: func(path string) (*Session, error) {
				return ReadAntigravityFilePage(path, 0, "missing-entry", "")
			},
		},
		{
			name: "antigravity raw",
			read: func(path string) (*Session, error) {
				return ReadAntigravityFileRawPage(path, 0, "", "missing-entry")
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			family := strings.Fields(tt.name)[0]
			if family == "claude" {
				family = "claude/tmux-cli"
			}
			path := writePaginationProviderFixture(t, family)
			page, err := tt.read(path)
			if err == nil {
				t.Fatalf("unknown cursor returned IDs %v, want ErrCursorNotFound", paginationEntryIDs(page.Messages))
			}
			if !errors.Is(err, ErrCursorNotFound) {
				t.Fatalf("unknown cursor error = %v, want ErrCursorNotFound", err)
			}
		})
	}
}

func TestReadCodexFileRetainedEntryIDsStayStableWhenResponseItemsAppear(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	initial := strings.Join([]string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"inspect the tree"}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"event_msg","payload":{"type":"agent_message","message":"I will inspect it"}}`,
		`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write initial Codex fixture: %v", err)
	}

	before, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("read initial Codex fixture: %v", err)
	}
	beforeIDs := codexEntryIDsByRaw(before.Messages)
	if len(beforeIDs) != 3 {
		t.Fatalf("initial Codex entries = %d, want 3", len(beforeIDs))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open Codex fixture for append: %v", err)
	}
	appended := `{"timestamp":"2026-01-01T00:00:03Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the tree"}]}}` + "\n"
	if _, err := f.WriteString(appended); err != nil {
		_ = f.Close()
		t.Fatalf("append Codex response item: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close Codex fixture: %v", err)
	}

	after, err := ReadCodexFile(path, 0)
	if err != nil {
		t.Fatalf("read appended Codex fixture: %v", err)
	}
	afterIDs := codexEntryIDsByRaw(after.Messages)
	for raw, beforeID := range beforeIDs {
		afterID, retained := afterIDs[raw]
		if !retained {
			continue
		}
		if afterID != beforeID {
			t.Fatalf("retained Codex entry ID changed from %q to %q after append; raw=%s", beforeID, afterID, raw)
		}
	}
}

func TestSyntheticCursorInvalidatesInsteadOfAliasingAfterRewrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		fileName string
	}{
		{provider: "auggie/tmux-cli", fileName: "events.jsonl"},
		{provider: "amp/tmux-cli", fileName: "stream.jsonl"},
		{provider: "codex/tmux-cli", fileName: "rollout.jsonl"},
		{provider: "copilot/tmux-cli", fileName: "events.jsonl"},
		{provider: "cursor/tmux-cli", fileName: "stream.jsonl"},
		{provider: "grok/tmux-cli", fileName: "events.jsonl"},
		{provider: "kiro/tmux-cli", fileName: "events.jsonl"},
		{provider: "gemini/tmux-cli", fileName: "session.json"},
		{provider: "kimi/tmux-cli", fileName: "context.jsonl"},
		{provider: "mimocode/tmux-cli", fileName: "session.json"},
		{provider: "opencode/tmux-cli", fileName: "session.json"},
		{provider: "antigravity/tmux-cli", fileName: "trajectory.jsonl"},
	}
	initialRecords := []syntheticCursorRecord{
		{ordinal: 0, text: "zero"},
		{ordinal: 1, text: "one"},
		{ordinal: 2, text: "two"},
	}
	replacementRecords := []syntheticCursorRecord{
		{ordinal: 0, text: "replacement zero"},
		{ordinal: 1, text: "replacement one"},
		{ordinal: 2, text: "replacement two"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(ProviderFamily(tt.provider), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), tt.fileName)
			for _, raw := range []bool{false, true} {
				raw := raw
				t.Run(fmt.Sprintf("raw=%t", raw), func(t *testing.T) {
					writeSyntheticCursorFixture(t, path, ProviderFamily(tt.provider), initialRecords)
					initial, err := readPaginationProviderFile(tt.provider, path, raw)
					if err != nil {
						t.Fatalf("read initial fixture: %v", err)
					}
					initialIDs := paginationEntryIDs(initial.Messages)
					if len(initialIDs) != 3 {
						t.Fatalf("initial IDs = %v, want three entries", initialIDs)
					}

					writeSyntheticCursorFixture(t, path, ProviderFamily(tt.provider), initialRecords[1:])
					truncated, err := readPaginationProviderFile(tt.provider, path, raw)
					if err != nil {
						t.Fatalf("read prefix-truncated fixture: %v", err)
					}
					if got, want := paginationEntryIDs(truncated.Messages), initialIDs[1:]; !reflect.DeepEqual(got, want) {
						t.Fatalf("retained IDs changed after prefix truncation: got %v, want %v", got, want)
					}
					page, err := readPaginationProviderPage(tt.provider, path, raw, "", initialIDs[1])
					if err != nil {
						t.Fatalf("read page after retained cursor: %v", err)
					}
					if got, want := paginationEntryIDs(page.Messages), initialIDs[2:]; !reflect.DeepEqual(got, want) {
						t.Fatalf("retained-cursor page IDs = %v, want %v", got, want)
					}

					writeSyntheticCursorFixture(t, path, ProviderFamily(tt.provider), replacementRecords)
					page, err = readPaginationProviderPage(tt.provider, path, raw, "", initialIDs[1])
					if err == nil {
						t.Fatalf("stale cursor returned IDs %v after rewrite, want ErrCursorNotFound", paginationEntryIDs(page.Messages))
					}
					if !errors.Is(err, ErrCursorNotFound) {
						t.Fatalf("stale cursor error = %v, want ErrCursorNotFound", err)
					}
				})
			}
		})
	}
}

type syntheticCursorRecord struct {
	ordinal int
	text    string
}

func writeSyntheticCursorFixture(t *testing.T, path, family string, records []syntheticCursorRecord) {
	t.Helper()
	if family == "gemini" {
		messages := make([]string, 0, len(records))
		for _, record := range records {
			messages = append(messages, fmt.Sprintf(`{"type":"gemini","content":%q}`, record.text))
		}
		body := `{"sessionId":"gemini-session","messages":[` + strings.Join(messages, ",") + `]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write Gemini fixture: %v", err)
		}
		return
	}
	if family == "mimocode" || family == "opencode" {
		messages := make([]string, 0, len(records))
		for _, record := range records {
			messages = append(messages, fmt.Sprintf(`{"info":{"role":"assistant"},"parts":[{"type":"text","text":%q}]}`, record.text))
		}
		body := `{"info":{"id":"opencode-session","directory":"/tmp/project"},"messages":[` + strings.Join(messages, ",") + `]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s fixture: %v", family, err)
		}
		return
	}

	lines := make([]string, 0, len(records))
	for _, record := range records {
		switch family {
		case "auggie", "grok", "kiro":
			lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":%q}}}}`, record.text))
		case "amp":
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},"session_id":"amp-session"}`, record.text))
		case "codex":
			lines = append(lines, fmt.Sprintf(`{"timestamp":"2026-01-01T00:00:%02dZ","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, record.ordinal, record.text))
		case "copilot":
			lines = append(lines, fmt.Sprintf(`{"type":"assistant.message","data":{"content":%q},"sessionId":"copilot-session"}`, record.text))
		case "cursor":
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":%q},"session_id":"cursor-session"}`, record.text))
		case "kimi":
			lines = append(lines, fmt.Sprintf(`{"role":"assistant","content":%q}`, record.text))
		case "antigravity":
			lines = append(lines, fmt.Sprintf(`{"step_index":%d,"type":"PLANNER_RESPONSE","content":%q}`, record.ordinal, record.text))
		default:
			t.Fatalf("no synthetic cursor fixture for provider family %q", family)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write %s fixture: %v", family, err)
	}
}

func codexEntryIDsByRaw(entries []*Entry) map[string]string {
	ids := make(map[string]string, len(entries))
	for _, entry := range entries {
		ids[string(entry.Raw)] = entry.UUID
	}
	return ids
}

func readPaginationProviderFile(provider, path string, raw bool) (*Session, error) {
	if raw {
		return ReadProviderFileRaw(provider, path, 0)
	}
	return ReadProviderFile(provider, path, 0)
}

func readPaginationProviderPage(provider, path string, raw bool, before, after string) (*Session, error) {
	switch {
	case raw && before != "":
		return ReadProviderFileRawOlder(provider, path, 0, before)
	case raw:
		return ReadProviderFileRawNewer(provider, path, 0, after)
	case before != "":
		return ReadProviderFileOlder(provider, path, 0, before)
	default:
		return ReadProviderFileNewer(provider, path, 0, after)
	}
}

func assertPaginationPage(t *testing.T, session *Session, wantIDs []string, total int, wantOlder, wantNewer bool) {
	t.Helper()
	if got := paginationEntryIDs(session.Messages); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("page IDs = %v, want %v", got, wantIDs)
	}
	if session.Pagination == nil {
		t.Fatal("page pagination metadata is nil")
	}
	if session.Pagination.TotalMessageCount != total || session.Pagination.ReturnedMessageCount != len(wantIDs) {
		t.Fatalf("pagination = %+v, want total=%d returned=%d", session.Pagination, total, len(wantIDs))
	}
	wire, err := json.Marshal(session.Pagination)
	if err != nil {
		t.Fatalf("marshal pagination: %v", err)
	}
	var flags struct {
		HasOlderMessages bool `json:"has_older_messages"`
		HasNewerMessages bool `json:"has_newer_messages"`
	}
	if err := json.Unmarshal(wire, &flags); err != nil {
		t.Fatalf("decode pagination flags: %v", err)
	}
	if flags.HasOlderMessages != wantOlder || flags.HasNewerMessages != wantNewer {
		t.Fatalf("pagination flags = older:%t newer:%t, want older:%t newer:%t; wire=%s", flags.HasOlderMessages, flags.HasNewerMessages, wantOlder, wantNewer, wire)
	}
}

func paginationEntryIDs(entries []*Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.UUID)
	}
	return ids
}

func writePaginationProviderFixture(t *testing.T, family string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	var body string

	switch family {
	case "claude/tmux-cli":
		body = strings.Join([]string{
			`{"uuid":"claude-0","type":"user","message":{"role":"user","content":"zero"}}`,
			`{"uuid":"claude-1","parentUuid":"claude-0","type":"assistant","message":{"role":"assistant","content":"one"}}`,
			`{"uuid":"claude-2","parentUuid":"claude-1","type":"user","message":{"role":"user","content":"two"}}`,
		}, "\n") + "\n"
	case "auggie", "grok", "kiro":
		body = strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"zero"}}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"one"}}}}`,
			`{"jsonrpc":"2.0","id":3,"method":"session/update","params":{"sessionId":"acp-session","update":{"sessionUpdate":"agent_message_chunk","content":{"text":"two"}}}}`,
		}, "\n") + "\n"
	case "amp":
		body = strings.Join([]string{
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"zero"}]},"session_id":"amp-session"}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"}]},"session_id":"amp-session"}`,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]},"session_id":"amp-session"}`,
		}, "\n") + "\n"
	case "codex":
		body = strings.Join([]string{
			`{"timestamp":"2026-01-01T00:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"zero"}]}}`,
			`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one"}]}}`,
			`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}}`,
		}, "\n") + "\n"
	case "copilot":
		body = strings.Join([]string{
			`{"type":"user.message","data":{"content":"zero"},"id":"copilot-0"}`,
			`{"type":"assistant.message","data":{"content":"one"},"id":"copilot-1"}`,
			`{"type":"user.message","data":{"content":"two"},"id":"copilot-2"}`,
		}, "\n") + "\n"
	case "cursor":
		body = strings.Join([]string{
			`{"type":"user","message":{"role":"user","content":"zero"},"session_id":"cursor-session"}`,
			`{"type":"assistant","message":{"role":"assistant","content":"one"},"session_id":"cursor-session"}`,
			`{"type":"user","message":{"role":"user","content":"two"},"session_id":"cursor-session"}`,
		}, "\n") + "\n"
	case "gemini":
		path = filepath.Join(dir, "session.json")
		body = `{"sessionId":"gemini-session","messages":[` +
			`{"id":"gemini-0","type":"user","content":"zero"},` +
			`{"id":"gemini-1","type":"gemini","content":"one"},` +
			`{"id":"gemini-2","type":"user","content":"two"}` +
			`]}`
	case "kimi":
		body = strings.Join([]string{
			`{"role":"user","content":"zero"}`,
			`{"role":"assistant","content":"one"}`,
			`{"role":"user","content":"two"}`,
		}, "\n") + "\n"
	case "mimocode", "opencode":
		path = filepath.Join(dir, "session.json")
		body = `{"info":{"id":"opencode-session","directory":"/tmp/project"},"messages":[` +
			`{"info":{"id":"opencode-0","role":"user"},"parts":[{"type":"text","text":"zero"}]},` +
			`{"info":{"id":"opencode-1","role":"assistant"},"parts":[{"type":"text","text":"one"}]},` +
			`{"info":{"id":"opencode-2","role":"user"},"parts":[{"type":"text","text":"two"}]}` +
			`]}`
	case "pi":
		body = strings.Join([]string{
			`{"type":"session","version":3,"id":"pi-session","cwd":"/tmp/project"}`,
			`{"type":"message","id":"pi-0","parentId":null,"message":{"role":"user","content":"zero"}}`,
			`{"type":"message","id":"pi-1","parentId":"pi-0","message":{"role":"assistant","content":"one"}}`,
			`{"type":"message","id":"pi-2","parentId":"pi-1","message":{"role":"user","content":"two"}}`,
		}, "\n") + "\n"
	case "antigravity":
		body = strings.Join([]string{
			`{"step_index":0,"type":"USER_INPUT","content":"zero"}`,
			`{"step_index":1,"type":"PLANNER_RESPONSE","content":"one"}`,
			`{"step_index":2,"type":"USER_INPUT","content":"two"}`,
		}, "\n") + "\n"
	default:
		t.Fatalf("no pagination fixture for provider family %q", family)
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write pagination fixture: %v", err)
	}
	return path
}
