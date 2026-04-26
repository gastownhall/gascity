package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

func writeTestSession(t *testing.T, searchBase, workDir string, lines ...string) {
	t.Helper()
	slug := strings.ReplaceAll(workDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	dir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeNamedTestSession(t *testing.T, searchBase, workDir, fileName string, lines ...string) string {
	t.Helper()
	slug := strings.ReplaceAll(workDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	dir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoSessionLogsBasic(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":{"role":"assistant","content":"world"},"timestamp":"2025-01-01T00:00:01Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 0, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "[USER]") {
		t.Errorf("output should contain [USER], got: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output should contain 'hello', got: %s", out)
	}
	if !strings.Contains(out, "[ASSISTANT]") {
		t.Errorf("output should contain [ASSISTANT], got: %s", out)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("output should contain 'world', got: %s", out)
	}
}

// TestDoSessionLogsTailReturnsLastNEntries verifies the user-facing `--tail N`
// semantics: print the LAST N displayable log entries (matching Unix `tail -n`),
// not the FIRST N, and regardless of compaction boundaries in the transcript.
func TestDoSessionLogsTailReturnsLastNEntries(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	// Six distinct user/assistant entries with no compaction boundaries at all.
	// This mirrors the Gas City 1.0 tutorial case where a fresh mayor session
	// has no compactions yet. Prior behavior ("N compaction segments") returned
	// every entry, printing the FIRST N visible on screen; the corrected
	// semantics must return the LAST N.
	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"first"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":{"role":"assistant","content":"reply-1"},"timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"user","message":{"role":"user","content":"second"},"timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"assistant","message":{"role":"assistant","content":"reply-2"},"timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"5","parentUuid":"4","type":"user","message":{"role":"user","content":"third"},"timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"6","parentUuid":"5","type":"assistant","message":{"role":"assistant","content":"reply-3"},"timestamp":"2025-01-01T00:00:05Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	// --tail 2 must return the LAST 2 entries: "third" + "reply-3".
	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 2, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "third") {
		t.Errorf("tail=2 should include the penultimate entry 'third', got: %s", out)
	}
	if !strings.Contains(out, "reply-3") {
		t.Errorf("tail=2 should include the last entry 'reply-3', got: %s", out)
	}
	// Everything before the last 2 must be absent. In particular, the FIRST
	// entry must not leak through — that was the bug the user reported.
	forbidden := []string{"first", "reply-1", "second", "reply-2"}
	for _, s := range forbidden {
		if strings.Contains(out, s) {
			t.Errorf("tail=2 should not include earlier entry %q (would be 'first N' behavior), got: %s", s, out)
		}
	}
}

// TestDoSessionLogsTailIgnoresCompactBoundaries confirms that `--tail N` counts
// displayable log entries, not compaction segments: a transcript with several
// compact boundaries must still yield exactly N trailing entries.
func TestDoSessionLogsTailIgnoresCompactBoundaries(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"first"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":{"role":"assistant","content":"reply"},"timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"system","subtype":"compact_boundary","message":{"role":"system","content":"compacted 1"},"timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"user","message":{"role":"user","content":"middle"},"timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"5","parentUuid":"4","type":"assistant","message":{"role":"assistant","content":"middle reply"},"timestamp":"2025-01-01T00:00:04Z"}`,
		`{"uuid":"6","parentUuid":"5","type":"system","subtype":"compact_boundary","message":{"role":"system","content":"compacted 2"},"timestamp":"2025-01-01T00:00:05Z"}`,
		`{"uuid":"7","parentUuid":"6","type":"user","message":{"role":"user","content":"second"},"timestamp":"2025-01-01T00:00:06Z"}`,
		`{"uuid":"8","parentUuid":"7","type":"assistant","message":{"role":"assistant","content":"reply2"},"timestamp":"2025-01-01T00:00:07Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	// --tail 1 should print exactly the last entry ("reply2"), not "the last
	// compaction segment."
	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 1, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "reply2") {
		t.Errorf("tail=1 should include final assistant 'reply2', got: %s", out)
	}
	for _, forbidden := range []string{"first", "middle", "second"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("tail=1 should not include earlier entry %q, got: %s", forbidden, out)
		}
	}

	// --tail 0 should print everything, including the two compact boundary
	// dividers.
	stdout.Reset()
	stderr.Reset()
	code = doSessionLogs(path, "", false, 0, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("tail=0 code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out = stdout.String()
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("tail=0 should include all entries, got: %s", out)
	}
	if strings.Count(out, "context compacted") != 2 {
		t.Errorf("tail=0 should render both compact dividers, got: %s", out)
	}
}

func TestDoSessionLogsToolUse(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"read file"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I will read the file."},{"type":"tool_use","name":"Read","id":"t1","input":{}}]},"timestamp":"2025-01-01T00:00:01Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 0, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "I will read the file.") {
		t.Errorf("output should contain text content, got: %s", out)
	}
	if !strings.Contains(out, "tool_use: Read") {
		t.Errorf("output should contain tool_use, got: %s", out)
	}
}

func TestDoSessionLogsStringEncodedMessage(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	// Message content is a JSON-encoded string (double-escaped), as some
	// Claude JSONL files produce.
	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"string encoded\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 0, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "string encoded") {
		t.Errorf("output should contain string-encoded message content, got: %s", out)
	}
}

func TestDoSessionLogsToolResultError(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"permission denied"}]},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 0, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "tool_result: error") {
		t.Errorf("output should contain tool_result error, got: %s", out)
	}
}

func TestResolveSessionLogPathPrefersKeyedTranscriptWhenPresent(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	want := writeNamedTestSession(t, searchBase, workDir, "known-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	got := resolveSessionLogPath([]string{searchBase}, sessionLogContext{
		workDir:    workDir,
		sessionKey: "known-session",
		provider:   "claude",
	})
	if got != want {
		t.Fatalf("resolveSessionLogPath() = %q, want %q", got, want)
	}
}

func TestResolveSessionLogPathFallsBackWhenSessionKeyFileMissing(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	want := writeNamedTestSession(t, searchBase, workDir, "latest-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	got := resolveSessionLogPath([]string{searchBase}, sessionLogContext{
		workDir:    workDir,
		sessionKey: "stale-session-key",
		provider:   "claude",
	})
	if got != want {
		t.Fatalf("resolveSessionLogPath() = %q, want %q", got, want)
	}
}

func TestResolveStoredSessionLogSource_UniqueWorkDirFallsBackBeyondLatestAlias(t *testing.T) {
	skipSlowCmdGCTest(t, "probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage")
	store := beads.NewMemStore()
	workDir := t.TempDir()
	searchBase := t.TempDir()
	want := writeNamedTestSession(t, searchBase, workDir, "actual-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"provider":     "claude",
			"session_key":  "stale-session-key",
			"session_name": "mayor",
			"state":        "asleep",
			"work_dir":     workDir,
		},
	})

	got, provider, ok := resolveStoredSessionLogSource("", nil, store, "mayor", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if provider != "claude" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want %q", provider, "claude")
	}
	if got != want {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want %q", got, want)
	}
}

func TestResolveStoredSessionLogSource_DoesNotCrossAmbiguousWorkDir(t *testing.T) {
	store := beads.NewMemStore()
	workDir := t.TempDir()
	searchBase := t.TempDir()
	writeNamedTestSession(t, searchBase, workDir, "actual-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hello"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"provider":     "claude",
			"session_key":  "stale-session-key",
			"session_name": "mayor",
			"state":        "asleep",
			"work_dir":     workDir,
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "helper",
			"provider":     "claude",
			"session_name": "helper",
			"state":        "asleep",
			"work_dir":     workDir,
		},
	})

	got, provider, ok := resolveStoredSessionLogSource("", nil, store, "mayor", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if provider != "claude" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want %q", provider, "claude")
	}
	if got != "" {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want empty for ambiguous same-workdir transcript", got)
	}
}

func TestDoSessionLogsNegativeTail(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doSessionLogs("/nonexistent", "", false, -1, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for negative tail", code)
	}
	if !strings.Contains(stderr.String(), "--tail must be >= 0") {
		t.Errorf("stderr should mention invalid tail, got: %s", stderr.String())
	}
}

// TestDoSessionLogsFollowTailShowsOnlyNewMessagesOnReadErrorExit exercises the
// real follow path with an initial tail window and exits via the existing
// consecutive-read-error threshold. Older pre-tail entries must be marked seen
// from the initial snapshot so that follow-mode re-reads only emit messages
// that arrived after the command started watching.
func TestDoSessionLogsFollowTailShowsOnlyNewMessagesOnReadErrorExit(t *testing.T) {
	initial := &sessionlog.Session{
		Messages: []*sessionlog.Entry{
			{
				UUID:      "1",
				Type:      "user",
				Message:   jsonMessage(t, "user", "old msg"),
				Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			},
			{
				UUID:      "2",
				Type:      "system",
				Subtype:   "compact_boundary",
				Timestamp: time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC),
			},
			{
				UUID:      "3",
				Type:      "user",
				Message:   jsonMessage(t, "user", "middle msg"),
				Timestamp: time.Date(2025, 1, 1, 0, 0, 2, 0, time.UTC),
			},
			{
				UUID:      "4",
				Type:      "system",
				Subtype:   "compact_boundary",
				Timestamp: time.Date(2025, 1, 1, 0, 0, 3, 0, time.UTC),
			},
			{
				UUID:      "5",
				Type:      "user",
				Message:   jsonMessage(t, "user", "recent msg"),
				Timestamp: time.Date(2025, 1, 1, 0, 0, 4, 0, time.UTC),
			},
		},
	}
	withNew := &sessionlog.Session{
		Messages: append(append([]*sessionlog.Entry{}, initial.Messages...),
			&sessionlog.Entry{
				UUID:      "6",
				Type:      "assistant",
				Message:   jsonMessage(t, "assistant", "fresh msg"),
				Timestamp: time.Date(2025, 1, 1, 0, 0, 5, 0, time.UTC),
			},
		),
	}

	var stdout, stderr bytes.Buffer
	readCount := 0
	code := runSessionLogs(
		nil,
		"",
		"/ignored",
		true,
		1,
		&stdout,
		&stderr,
		func(time.Duration) {},
		func(_ *worker.Factory, _ string, _ string) (*worker.TranscriptSession, error) {
			readCount++
			switch readCount {
			case 1:
				return initial, nil
			case 2:
				return withNew, nil
			default:
				return nil, fmt.Errorf("boom %d", readCount)
			}
		},
	)
	if code != 1 {
		t.Fatalf("code = %d, want 1 after injected consecutive read failures", code)
	}

	out := stdout.String()
	if !strings.Contains(out, "recent msg") {
		t.Fatalf("initial tail window should print the most recent existing entry, got: %s", out)
	}
	if !strings.Contains(out, "fresh msg") {
		t.Fatalf("follow mode should print the new message from the reread, got: %s", out)
	}
	for _, forbidden := range []string{"old msg", "middle msg"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("follow mode should not replay pre-tail entry %q, got: %s", forbidden, out)
		}
	}
	if !strings.Contains(stderr.String(), "5 consecutive read errors") {
		t.Fatalf("stderr should report the injected termination condition, got: %s", stderr.String())
	}
}

func jsonMessage(t *testing.T, role, content string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(`{"role":%q,"content":%q}`, role, content))
}

func TestPrintLogEntryTimestamp(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"hi"},"timestamp":"2025-06-15T14:30:45Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 0, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "14:30:45") {
		t.Errorf("output should contain timestamp, got: %s", out)
	}
}

func TestResolveSessionLogWorkDirByAlias(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":    "worker",
			"template": "myrig/worker",
			"work_dir": "/tmp/myrig",
		},
	})

	got, ok := resolveSessionLogContext("", nil, store, "worker")
	if !ok {
		t.Fatal("resolveSessionLogContext() = not found, want found")
	}
	if got.workDir != "/tmp/myrig" {
		t.Fatalf("resolveSessionLogContext() workDir = %q, want %q", got.workDir, "/tmp/myrig")
	}
}

func TestResolveSessionLogWorkDirBySessionName(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-77",
			"agent_name":   "myrig/worker",
			"work_dir":     "/tmp/myrig",
		},
	})

	got, ok := resolveSessionLogContext("", nil, store, "s-gc-77")
	if !ok {
		t.Fatal("resolveSessionLogContext() = not found, want found")
	}
	if got.workDir != "/tmp/myrig" {
		t.Fatalf("resolveSessionLogContext() workDir = %q, want %q", got.workDir, "/tmp/myrig")
	}
}

func TestResolveSessionLogWorkDirDoesNotUseClosedHistoricalAlias(t *testing.T) {
	store := beads.NewMemStore()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "mayor",
			"work_dir":      "/tmp/myrig",
		},
	})
	_ = store.Close(b.ID)

	if got, ok := resolveSessionLogContext("", nil, store, "mayor"); ok {
		t.Fatalf("resolveSessionLogContext() = %+v, want not found for historical alias", got)
	}
}

func TestResolveConfiguredSessionLogContext_ByExactSingletonAlias(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "demo")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Rigs:      []config.Rig{{Name: "demo", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "witness", Dir: "demo"},
		},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}

	got, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "demo/witness")
	if !ok {
		t.Fatal("resolveConfiguredSessionLogContext() = not found, want found")
	}
	if got != rigPath {
		t.Fatalf("resolveConfiguredSessionLogContext() workDir = %q, want %q", got, rigPath)
	}
}

func TestResolveConfiguredSessionLogContext_RejectsBareRigScopedNamedSession(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "demo")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Rigs:      []config.Rig{{Name: "demo", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "witness", Dir: "demo"},
		},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}

	if got, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "witness"); ok {
		t.Fatalf("resolveConfiguredSessionLogContext() = %q, want not found without qualified target", got)
	}
}

func TestResolveSessionLogContext_ReservedNamedTargetIgnoresClosedHistoricalBead(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "repos", "demo")
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Rigs:      []config.Rig{{Name: "demo", Path: rigPath}},
		Agents: []config.Agent{
			{Name: "witness", Dir: "demo"},
		},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "sky",
			"alias_history": "demo/witness",
			"work_dir":      "/tmp/stale",
		},
	})
	_ = store.Close(b.ID)

	got, ok := resolveSessionLogContext(cityPath, cfg, store, "demo/witness")
	if ok {
		t.Fatalf("resolveSessionLogContext() = %+v, want not found for reserved named target", got)
	}
	configuredWorkDir, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "demo/witness")
	if !ok {
		t.Fatal("resolveConfiguredSessionLogContext() = not found, want configured fallback")
	}
	if configuredWorkDir != rigPath {
		t.Fatalf("resolveConfiguredSessionLogContext() workDir = %q, want %q", configuredWorkDir, rigPath)
	}
}

func TestResolveConfiguredSessionLogContext_RebrandedSingletonUsesTemplateWorkDirIdentity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{{
			Name:              "witness",
			Dir:               "demo",
			WorkDir:           ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
			MaxActiveSessions: intPtr(1),
		}},
		NamedSessions: []config.NamedSession{{
			Name:     "boot",
			Template: "witness",
			Dir:      "demo",
		}},
	}

	got, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "demo/boot")
	if !ok {
		t.Fatal("resolveConfiguredSessionLogContext() = not found, want found")
	}
	want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "witness")
	if got != want {
		t.Fatalf("resolveConfiguredSessionLogContext() workDir = %q, want %q", got, want)
	}
}

func TestResolveConfiguredSessionLogContext_RejectsNonExactOrPoolTargets(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Agents: []config.Agent{
			{Name: "dog", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)},
			{Name: "worker", Dir: "demo"},
		},
	}

	if _, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "dog"); ok {
		t.Fatal("resolveConfiguredSessionLogContext(pool) = found, want not found")
	}
	if _, ok := resolveConfiguredSessionLogContext(cityPath, cfg, "worker"); ok {
		t.Fatal("resolveConfiguredSessionLogContext(non-exact bare name) = found, want not found")
	}
}

func TestResolveAgentWorkDirUsesWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
	}
	agent := config.Agent{
		Name:    "refinery",
		Dir:     "demo",
		WorkDir: ".gc/worktrees/{{.Rig}}/refinery",
	}

	got, err := resolveWorkDir(cityPath, cfg, &agent)
	if err != nil {
		t.Fatalf("resolveWorkDir() error = %v", err)
	}
	want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "refinery")
	if got != want {
		t.Fatalf("resolveWorkDir() = %q, want %q", got, want)
	}
}

func TestResolveSessionLogPath_DoesNotFallbackAcrossSessionsWhenSessionKeyMissing(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	// Another session in the same workdir already has a transcript.
	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"helper"},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	got := resolveSessionLogPath([]string{searchBase}, sessionLogContext{
		workDir:    workDir,
		sessionKey: "missing-session-key",
		provider:   "claude",
	})
	if got != "" {
		t.Fatalf("resolveSessionLogPath() = %q, want empty when keyed lookup misses", got)
	}
}
