package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
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

func TestDoSessionLogsCompactBoundary(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	// Two compact boundaries so tail=1 returns only the last segment.
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

	// tail=1 should show only from the last compact boundary.
	var stdout, stderr bytes.Buffer
	code := doSessionLogs(path, "", false, 1, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "first") {
		t.Errorf("tail=1 should not include messages before compact boundary, got: %s", out)
	}
	if !strings.Contains(out, "context compacted") {
		t.Errorf("output should contain compact divider, got: %s", out)
	}
	if !strings.Contains(out, "second") {
		t.Errorf("output should contain 'second', got: %s", out)
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

// TestDoSessionLogsFollowSeeding verifies that follow mode seeds the 'seen' map
// with ALL existing messages (not just the tail window) before entering the
// poll loop. Without this, switching from tail=N to tail=0 re-reads would
// replay messages from before the compaction boundary.
func TestDoSessionLogsFollowSeeding(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	// Session with two compact boundaries: tail=1 shows only the last segment.
	// Need 2 boundaries so sliceAtCompactBoundaries actually trims.
	writeTestSession(t, searchBase, workDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":{"role":"user","content":"old msg"},"timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"system","subtype":"compact_boundary","message":{"role":"system","content":"compacted 1"},"timestamp":"2025-01-01T00:00:01Z"}`,
		`{"uuid":"3","parentUuid":"2","type":"user","message":{"role":"user","content":"middle msg"},"timestamp":"2025-01-01T00:00:02Z"}`,
		`{"uuid":"4","parentUuid":"3","type":"system","subtype":"compact_boundary","message":{"role":"system","content":"compacted 2"},"timestamp":"2025-01-01T00:00:03Z"}`,
		`{"uuid":"5","parentUuid":"4","type":"user","message":{"role":"user","content":"recent msg"},"timestamp":"2025-01-01T00:00:04Z"}`,
	)

	path := sessionlog.FindSessionFile([]string{searchBase}, workDir)
	if path == "" {
		t.Fatal("session file not found")
	}

	// Simulate what doSessionLogs does: initial tail=1 read, then seed seen
	// map with tail=0. Verify "old msg" uuid is in seen even though it
	// wasn't printed.
	sess, err := sessionlog.ReadFile(path, 1)
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	for _, msg := range sess.Messages {
		seen[msg.UUID] = true
	}

	// At this point, seen should NOT contain uuid "1" (it's before the boundary).
	if seen["1"] {
		t.Fatal("tail=1 should not include pre-boundary messages")
	}

	// Now simulate the seeding step (what doSessionLogs does before follow loop).
	full, err := sessionlog.ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range full.Messages {
		seen[msg.UUID] = true
	}

	// After seeding, "1" should be in seen — preventing replay on re-read.
	if !seen["1"] {
		t.Error("after follow-mode seeding, pre-boundary message UUID should be in seen")
	}

	// Simulate a follow-mode re-read: no unseen messages should exist.
	reread, err := sessionlog.ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var unseen []string
	for _, msg := range reread.Messages {
		if !seen[msg.UUID] {
			unseen = append(unseen, msg.UUID)
		}
	}
	if len(unseen) > 0 {
		t.Errorf("follow-mode re-read found unseen messages (would be replayed): %v", unseen)
	}
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
