package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

type noLabelScanSessionLogStore struct {
	*beads.MemStore
}

func (s *noLabelScanSessionLogStore) ListByLabel(label string, _ int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, fmt.Errorf("unexpected label scan for %q", label)
}

func writeCodexTestSession(t *testing.T, searchBase, workDir, fileName string, lines ...string) string {
	t.Helper()
	dayDir := filepath.Join(searchBase, "2026", "05", "04")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, fileName)
	allLines := append([]string{
		fmt.Sprintf(`{"timestamp":"2026-05-04T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
	}, lines...)
	if err := os.WriteFile(path, []byte(strings.Join(allLines, "\n")+"\n"), 0o644); err != nil {
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
	// entry must not leak through; that was the bug the user reported.
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

// TestDoSessionLogsTailNeverRendersEmpty pins the invariant that every tail-slot
// entry renders at least one line. A block-array entry whose blocks are all
// non-rendering (a non-error tool_result, or an unknown/thinking block) used to
// print nothing and return before the raw fallback, so a `--tail N` window
// landing on such entries produced empty stdout with exit 0 -- the
// TestTutorial03Sessions "session logs --tail 2 output is empty" RC flake.
func TestDoSessionLogsTailNeverRendersEmpty(t *testing.T) {
	cases := []struct {
		name      string
		lastEntry string
		wantSub   string
	}{
		{
			name:      "non_error_tool_result",
			lastEntry: `{"uuid":"2","parentUuid":"1","type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":false,"content":"ok"}]},"timestamp":"2025-01-01T00:00:01Z"}`,
			wantSub:   "tool_result: ok",
		},
		{
			name:      "unknown_block_type",
			lastEntry: `{"uuid":"2","parentUuid":"1","type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]},"timestamp":"2025-01-01T00:00:01Z"}`,
			wantSub:   "no displayable content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			searchBase := t.TempDir()
			workDir := t.TempDir()
			writeTestSession(t, searchBase, workDir,
				`{"uuid":"1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]},"timestamp":"2025-01-01T00:00:00Z"}`,
				tc.lastEntry,
			)
			path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
			if path == "" {
				t.Fatal("session file not found")
			}

			// --tail 1 lands entirely on the non-rendering last entry.
			var stdout, stderr bytes.Buffer
			code := doSessionLogs(path, "", false, 1, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
			}
			if strings.TrimSpace(stdout.String()) == "" {
				t.Fatalf("`--tail 1` produced empty output; every tail-slot entry must render at least one line")
			}
			if !strings.Contains(stdout.String(), tc.wantSub) {
				t.Errorf("output = %q, want it to contain %q", stdout.String(), tc.wantSub)
			}
		})
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

	got, provider, ok, diagnostic := resolveStoredSessionLogSource("", nil, sessionFrontDoor(store), "mayor", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if diagnostic != "" {
		t.Fatalf("resolveStoredSessionLogSource() diagnostic = %q, want empty", diagnostic)
	}
	if provider != "claude" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want %q", provider, "claude")
	}
	if got != want {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want %q", got, want)
	}
}

func TestResolveStoredSessionLogSource_ProviderConstructionFailureReturnsDiagnostic(t *testing.T) {
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_SESSION", "broken")
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return nil, errors.New("injected provider failure")
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"provider":     "claude",
			"session_name": "mayor",
			"state":        "asleep",
			"work_dir":     t.TempDir(),
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	path, provider, ok, diagnostic := resolveStoredSessionLogSource("", nil, sessionFrontDoor(store), "mayor", []string{t.TempDir()})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want provider failure diagnostic")
	}
	if path != "" {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want empty", path)
	}
	if provider != "claude" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want %q", provider, "claude")
	}
	if got, want := diagnostic, "constructing session provider: injected provider failure"; got != want {
		t.Fatalf("resolveStoredSessionLogSource() diagnostic = %q, want %q", got, want)
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

	got, provider, ok, diagnostic := resolveStoredSessionLogSource("", nil, sessionFrontDoor(store), "mayor", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if provider != "claude" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want %q", provider, "claude")
	}
	if got != "" {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want empty for ambiguous same-workdir transcript", got)
	}
	if !strings.Contains(diagnostic, "ambiguous") {
		t.Fatalf("resolveStoredSessionLogSource() diagnostic = %q, want ambiguous", diagnostic)
	}
}

func TestResolveStoredSessionLogSource_CodexDoesNotUseAmbiguousWorkDirFallback(t *testing.T) {
	store := beads.NewMemStore()
	workDir := t.TempDir()
	searchBase := t.TempDir()
	writeCodexTestSession(t, searchBase, workDir, "rollout-current.jsonl",
		`{"timestamp":"2026-05-04T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"wrong session"}]}}`,
	)

	for _, name := range []string{"workflows__codex-max-mc-one", "workflows__codex-max-mc-two"} {
		b, _ := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias":         name,
				"provider":      "codex",
				"provider_kind": "codex",
				"session_key":   "019df2fd-078f-7cb2-93c8-5c649a15eabe",
				"session_name":  name,
				"state":         "gc_swept",
				"work_dir":      workDir,
			},
		})
		_ = store.Close(b.ID)
	}

	got, provider, ok, diagnostic := resolveStoredSessionLogSource("", nil, sessionFrontDoor(store), "workflows__codex-max-mc-one", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if provider != "codex" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want codex", provider)
	}
	if got != "" {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want empty for ambiguous codex workdir", got)
	}
	if !strings.Contains(diagnostic, "ambiguous") {
		t.Fatalf("resolveStoredSessionLogSource() diagnostic = %q, want ambiguous", diagnostic)
	}
}

func TestResolveStoredSessionLogSource_CodexAmbiguousWorkDirUsesStartOrder(t *testing.T) {
	workDir := t.TempDir()
	firstStarted := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	secondStarted := firstStarted.Add(2 * time.Minute)
	store := beads.NewMemStoreFrom(2, []beads.Bead{
		{
			ID:        "gc-1",
			Type:      session.BeadType,
			Status:    "open",
			Labels:    []string{session.LabelSession},
			CreatedAt: firstStarted.Add(-time.Hour),
			UpdatedAt: firstStarted,
			Metadata: map[string]string{
				"alias":         "workflows__codex-max-mc-one",
				"provider":      "codex",
				"provider_kind": "codex",
				"session_name":  "workflows__codex-max-mc-one",
				"state":         "awake",
				"last_woke_at":  firstStarted.Format(time.RFC3339),
				"work_dir":      workDir,
			},
		},
		{
			ID:        "gc-2",
			Type:      session.BeadType,
			Status:    "open",
			Labels:    []string{session.LabelSession},
			CreatedAt: secondStarted.Add(-time.Hour),
			UpdatedAt: secondStarted,
			Metadata: map[string]string{
				"alias":         "workflows__codex-max-mc-two",
				"provider":      "codex",
				"provider_kind": "codex",
				"session_name":  "workflows__codex-max-mc-two",
				"state":         "awake",
				"last_woke_at":  secondStarted.Format(time.RFC3339),
				"work_dir":      workDir,
			},
		},
	}, nil)

	searchBase := t.TempDir()
	dayDir := filepath.Join(searchBase, "2026", "05", "04")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(dayDir, "rollout-first.jsonl")
	if err := os.WriteFile(firstPath, []byte(fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"cwd":%q}}`+"\n", firstStarted.Format(time.RFC3339), workDir)), 0o644); err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(dayDir, "rollout-second.jsonl")
	if err := os.WriteFile(secondPath, []byte(fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"cwd":%q}}`+"\n", secondStarted.Format(time.RFC3339), workDir)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, provider, ok, diagnostic := resolveStoredSessionLogSource("", nil, sessionFrontDoor(store), "workflows__codex-max-mc-two", []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if diagnostic != "" {
		t.Fatalf("resolveStoredSessionLogSource() diagnostic = %q, want empty", diagnostic)
	}
	if provider != "codex" {
		t.Fatalf("resolveStoredSessionLogSource() provider = %q, want codex", provider)
	}
	if got != secondPath {
		t.Fatalf("resolveStoredSessionLogSource() path = %q, want %q", got, secondPath)
	}
}

func TestCanFallbackStoredSessionLogByWorkDirUsesTargetedLookup(t *testing.T) {
	store := &noLabelScanSessionLogStore{MemStore: beads.NewMemStore()}
	workDir := t.TempDir()
	b, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"provider":     "codex",
			"session_name": "worker",
			"state":        "awake",
			"work_dir":     workDir,
		},
	})

	ok := canFallbackStoredSessionLogByWorkDir(sessionFrontDoor(store), sessionLogContext{
		sessionID: b.ID,
		workDir:   workDir,
		provider:  "codex",
	})
	if !ok {
		t.Fatal("canFallbackStoredSessionLogByWorkDir() = false, want true")
	}
}

func TestCanFallbackStoredSessionLogByWorkDirIgnoresAsleepPeersForLiveTarget(t *testing.T) {
	store := beads.NewMemStore()
	workDir := t.TempDir()
	target, _ := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "worker",
			"provider":     "codex",
			"session_name": "worker",
			"state":        "awake",
			"work_dir":     workDir,
		},
	})
	_, _ = store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "old-worker",
			"provider":     "codex",
			"session_name": "old-worker",
			"state":        "asleep",
			"work_dir":     workDir,
		},
	})

	ok := canFallbackStoredSessionLogByWorkDir(sessionFrontDoor(store), sessionLogContext{
		sessionID: target.ID,
		workDir:   workDir,
		provider:  "codex",
	})
	if !ok {
		t.Fatal("canFallbackStoredSessionLogByWorkDir() = false, want true")
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

func TestCmdSessionLogsJSONSuccessIsJSONOnly(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(fmt.Sprintf(`[workspace]

[daemon]
observe_paths = [%q]
`, searchBase)), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte("workspace_name = \"test\"\n"), 0o644); err != nil {
		t.Fatalf("write site.toml: %v", err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "json logs session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "runtime-session",
			"session_key":  "known-session",
			"provider":     "claude",
			"template":     "worker",
			"state":        "asleep",
			"work_dir":     workDir,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session): %v", err)
	}
	writeNamedTestSession(t, searchBase, workDir, "known-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"older"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":{"role":"assistant","content":"newer"},"timestamp":"2025-01-01T00:00:01Z"}`,
	)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionLogs([]string{b.ID}, false, 1, true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionLogs(--json) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1 JSONL record: %q", len(lines), stdout.String())
	}
	var got sessionLogsJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || got.Target != b.ID || got.Tail != 1 || got.EntryCount != 1 {
		t.Fatalf("logs JSON metadata = %+v", got)
	}
	if len(got.Entries) != 1 || got.Entries[0].Text != "newer" || got.Entries[0].Role != "assistant" {
		t.Fatalf("logs JSON entries = %+v", got.Entries)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "logs"}, stdout.Bytes())
}

func TestCmdSessionLogsJSONBlocksValidateDeclaredSchema(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(fmt.Sprintf(`[workspace]
name = "test"

[daemon]
observe_paths = [%q]
`, searchBase)), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "json block logs session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "runtime-session",
			"session_key":  "known-session",
			"provider":     "claude",
			"template":     "worker",
			"state":        "asleep",
			"work_dir":     workDir,
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session): %v", err)
	}
	writeNamedTestSession(t, searchBase, workDir, "known-session.jsonl",
		`{"uuid":"1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"shell","input":{"cmd":"date"}}]},"timestamp":"2025-01-01T00:00:00Z"}`,
	)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionLogs([]string{b.ID}, false, 1, true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionLogs(--json) = %d, want 0; stderr=%s", code, stderr.String())
	}
	var got sessionLogsJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, stdout.String())
	}
	if len(got.Entries) != 1 || len(got.Entries[0].Blocks) != 1 {
		t.Fatalf("logs JSON blocks = %+v", got.Entries)
	}
	block := got.Entries[0].Blocks[0]
	if block.Type != "tool_use" || block.ID != "tool-1" || block.Name != "shell" {
		t.Fatalf("block = %+v, want tool_use shell block", block)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "logs"}, stdout.Bytes())
}

func TestDoSessionLogsJSONRejectsFollow(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doSessionLogsJSON("/ignored", "claude", "worker", true, 10, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSessionLogsJSON(follow) = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--json is only supported for bounded snapshots") {
		t.Fatalf("stderr = %q, want bounded snapshot diagnostic", stderr.String())
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

	got, ok := resolveSessionLogContext("", nil, sessionFrontDoor(store), "worker")
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

	got, ok := resolveSessionLogContext("", nil, sessionFrontDoor(store), "s-gc-77")
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

	if got, ok := resolveSessionLogContext("", nil, sessionFrontDoor(store), "mayor"); ok {
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

	got, ok := resolveSessionLogContext(cityPath, cfg, sessionFrontDoor(store), "demo/witness")
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
