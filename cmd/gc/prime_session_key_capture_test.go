package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// primeCaptureTestStore stands up a file-backed city store the same way
// bd_env_test.go does, so persistPrimeHookProviderSessionKey — which resolves
// the city from GC_CITY and opens its own store handle — reads and writes the
// same on-disk store the test inspects.
func primeCaptureTestStore(t *testing.T) (cityDir string, store beads.Store) {
	t.Helper()
	cityDir = t.TempDir()
	t.Setenv("GC_BEADS", "file")
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	s, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	return cityDir, s
}

// createCaptureSessionBead creates a session bead for the given provider family
// with an empty session_key and returns its id.
func createCaptureSessionBead(t *testing.T, store beads.Store, providerKind string) string {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title: "session " + providerKind,
		Type:  "session",
		Metadata: map[string]string{
			"provider_kind": providerKind,
			"session_key":   "",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return b.ID
}

// isolateProviderSessionEnv clears the ambient provider-session env so the test
// exercises the hook-stdin capture path deterministically (the live session this
// test may run inside can otherwise leak GC_PROVIDER_SESSION_ID).
func isolateProviderSessionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_PROVIDER_SESSION_ID", "")
	t.Setenv("GEMINI_SESSION_ID", "")
	t.Setenv("GC_PROVIDER_SESSION_ID_REQUIRED", "1")
}

// TestPersistPrimeHookProviderSessionKey_ClaudeHookStdinCaptured is the
// regression guard: a claude session must capture the resume id its
// SessionStart hook delivers on stdin. Without it session_key stays empty,
// wake_mode=resume has nothing to resume, and every recycle starts fresh.
func TestPersistPrimeHookProviderSessionKey_ClaudeHookStdinCaptured(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "claude")
	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)

	const claudeSessionID = "8273e9ca-ff09-4260-a03a-1f8534cc1ba5"
	// The success line is an opt-in operator diagnostic; without GC_DEBUG the
	// hook stays quiet so it cannot land in the agent's input box (#5564).
	t.Setenv("GC_DEBUG", "1")
	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey(claudeSessionID, &stderr)

	got := reloadSessionKey(t, cityDir, id)
	if got != claudeSessionID {
		t.Fatalf("claude session_key = %q, want %q (hook stdin session id must be captured for claude; stderr=%q)", got, claudeSessionID, stderr.String())
	}
	if !strings.Contains(stderr.String(), "persisted resume session_key") {
		t.Errorf("successful capture must be observable, got stderr=%q", stderr.String())
	}
}

// TestPersistPrimeHookProviderSessionKey_CodexHookStdinStillCaptured pins the
// pre-existing codex behavior so the claude fix does not regress it.
func TestPersistPrimeHookProviderSessionKey_CodexHookStdinStillCaptured(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "codex")
	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)

	const codexSessionID = "codex-abc-123"
	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey(codexSessionID, &stderr)

	if got := reloadSessionKey(t, cityDir, id); got != codexSessionID {
		t.Fatalf("codex session_key = %q, want %q", got, codexSessionID)
	}
}

func TestPersistPrimeHookProviderSessionKey_CursorHookStdinCaptured(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "cursor")
	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)

	const cursorSessionID = "cursor-chat-abc-123"
	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey(cursorSessionID, &stderr)

	if got := reloadSessionKey(t, cityDir, id); got != cursorSessionID {
		t.Fatalf("cursor session_key = %q, want %q", got, cursorSessionID)
	}
}

func TestReadPrimeHookContextUsesCursorConversationID(t *testing.T) {
	setPrimeHookStdinJSON(t, map[string]string{
		"conversation_id": "cursor-chat-abc-123",
		"hook_event_name": "sessionStart",
	})

	ctx := readPrimeHookContext()
	if got := ctx.ProviderSessionID; got != "cursor-chat-abc-123" {
		t.Fatalf("ProviderSessionID = %q, want Cursor conversation_id", got)
	}
}

// TestPersistPrimeHookProviderSessionKey_ClaudeDoesNotOverwrite confirms an
// already-captured key is authoritative: a resume-wake's SessionStart hook must
// not clobber the stored key.
func TestPersistPrimeHookProviderSessionKey_ClaudeDoesNotOverwrite(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	b, err := store.Create(beads.Bead{
		Title: "session claude",
		Type:  "session",
		Metadata: map[string]string{
			"provider_kind": "claude",
			"session_key":   "original-uuid",
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Setenv("GC_SESSION_ID", b.ID)
	isolateProviderSessionEnv(t)

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("different-uuid", &stderr)

	if got := reloadSessionKey(t, cityDir, b.ID); got != "original-uuid" {
		t.Fatalf("session_key = %q, want unchanged %q", got, "original-uuid")
	}
}

// TestProviderAcceptsHookStdinSessionID locks the allowlist boundary: only the
// families whose SessionStart hook delivers their authoritative resume id on
// stdin (codex, cursor, claude) are accepted; every other family is not.
func TestProviderAcceptsHookStdinSessionID(t *testing.T) {
	cases := map[string]bool{
		"codex":    true,
		"cursor":   true,
		"claude":   true,
		"gemini":   false,
		"pi":       false,
		"opencode": false,
		"unknown":  false,
		"":         false,
	}
	for family, want := range cases {
		if got := providerAcceptsHookStdinSessionID(family); got != want {
			t.Errorf("providerAcceptsHookStdinSessionID(%q) = %v, want %v", family, got, want)
		}
	}
}

// TestPersistPrimeHookProviderSessionKey_UnsupportedFamilyHookStdinRejected pins
// the safety boundary: a family outside the allowlist must NOT capture a
// hook-stdin session id. Such providers surface their id via env instead, which
// is handled before this gate.
func TestPersistPrimeHookProviderSessionKey_UnsupportedFamilyHookStdinRejected(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "gemini")
	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("11111111-2222-3333-4444-555555555555", &stderr)

	if got := reloadSessionKey(t, cityDir, id); got != "" {
		t.Fatalf("gemini session_key = %q, want empty (hook stdin id must not be captured for non-allowlisted families)", got)
	}
}

// TestPersistPrimeHookProviderSessionKey_ClaudeEnvSessionIDCaptured confirms the
// change is surgical — it touches only the hook-stdin branch. An id delivered
// via GC_PROVIDER_SESSION_ID (fromHookStdin=false) is captured for claude
// regardless of the gate, exactly as before.
func TestPersistPrimeHookProviderSessionKey_ClaudeEnvSessionIDCaptured(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "claude")
	t.Setenv("GC_SESSION_ID", id)
	t.Setenv("GEMINI_SESSION_ID", "")
	t.Setenv("GC_PROVIDER_SESSION_ID_REQUIRED", "1")
	const envSessionID = "env-1a2b3c4d"
	t.Setenv("GC_PROVIDER_SESSION_ID", envSessionID)

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("", &stderr)

	if got := reloadSessionKey(t, cityDir, id); got != envSessionID {
		t.Fatalf("claude env session_key = %q, want %q (env path must be unaffected by the stdin gate)", got, envSessionID)
	}
}

// TestPersistPrimeHookProviderSessionKey_RejectsIDEqualToGCSessionID guards the
// pre-existing collision check for the claude path: a provider id equal to the
// gc session id is never stored as a resume key.
func TestPersistPrimeHookProviderSessionKey_RejectsIDEqualToGCSessionID(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "claude")
	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey(id, &stderr) // hook id == gc session id

	if got := reloadSessionKey(t, cityDir, id); got != "" {
		t.Fatalf("session_key = %q, want empty (provider id equal to GC_SESSION_ID must be rejected)", got)
	}
}

func reloadSessionKey(t *testing.T, cityDir, id string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}
	return strings.TrimSpace(b.Metadata["session_key"])
}

// The resume key must still be captured with GC_DEBUG unset, and nothing may
// reach stderr: provider hooks forward a child's stderr into the agent's
// terminal, so a success announcement lands mid-input-box (#5564).
func TestPersistPrimeHookProviderSessionKey_QuietWithoutDebug(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "claude")
	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)
	t.Setenv("GC_DEBUG", "")

	const claudeSessionID = "8273e9ca-ff09-4260-a03a-1f8534cc1ba5"
	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey(claudeSessionID, &stderr)

	if got := reloadSessionKey(t, cityDir, id); got != claudeSessionID {
		t.Fatalf("session_key = %q, want %q; quieting the diagnostic must not change capture", got, claudeSessionID)
	}
	if out := stderr.String(); out != "" {
		t.Errorf("hook wrote to stderr without GC_DEBUG, which reaches the agent terminal: %q", out)
	}
}

// A caller that already resolved its city hands that path to the provider-key
// write, which must use it without resolving the ambient city again — here
// ambient resolution is made impossible, so only the supplied path can work.
func TestPersistPrimeHookProviderSessionKeyAtCityUsesSuppliedPath(t *testing.T) {
	isolateProviderSessionEnv(t)
	cityDir, store := primeCaptureTestStore(t)
	sessionID := createCaptureSessionBead(t, store, "claude")
	t.Setenv("GC_SESSION_ID", sessionID)
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_DIR", "GC_RIG", "GC_RIG_ROOT"} {
		t.Setenv(key, "")
	}
	if resolved, err := resolveCity(); err == nil {
		t.Fatalf("test requires ambient city resolution to fail, resolved %q", resolved)
	}

	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKeyAtCity("11111111-2222-3333-4444-555555555555", cityDir, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("persist stderr = %q, want empty", stderr.String())
	}

	updated, err := store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(updated.Metadata["session_key"]); got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("session_key = %q, want the hook id persisted through the supplied city path", got)
	}

	// The ambient wrapper still resolves the city itself, so with nothing to
	// resolve it reports that rather than writing anywhere.
	stderr.Reset()
	persistPrimeHookProviderSessionKey("11111111-2222-3333-4444-555555555555", &stderr)
	if !strings.Contains(stderr.String(), "resolving city") {
		t.Fatalf("ambient persist stderr = %q, want a city-resolution diagnostic", stderr.String())
	}
}

// TestPrimeHookCapturesClaudeIDAfterSyncOnLiveKeylessRow kills a sync pass
// that mints a key between a stale-key relaunch's clear and the relaunched
// claude's SessionStart hook (gastownhall/gascity#5439): the set-once capture
// would drop the provider's real id for a key naming no transcript. The
// hook's id must land.
func TestPrimeHookCapturesClaudeIDAfterSyncOnLiveKeylessRow(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	ds := map[string]TemplateParams{"alpha": builtinClaudeTemplate(t, "alpha", t.TempDir())}
	clk := &clock.Fake{Time: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, runtime.NewFake(), allConfiguredDS(ds), nil, clk, &stderr, false)
	rows := allSessionBeads(t, store)
	if len(rows) != 1 || rows[0].Metadata["session_key"] == "" {
		t.Fatalf("first sync: rows %v, want one keyed row; stderr=%s", rows, stderr.String())
	}
	id := rows[0].ID
	// The stale-key relaunch clears the key; its process is then live.
	if err := store.SetMetadataBatch(id, map[string]string{"session_key": "", "state": "active"}); err != nil {
		t.Fatal(err)
	}
	clk.Time = clk.Time.Add(time.Minute)
	syncSessionBeads("", store, ds, runtime.NewFake(), allConfiguredDS(ds), nil, clk, &stderr, false)

	t.Setenv("GC_SESSION_ID", id)
	isolateProviderSessionEnv(t)
	const claudeSessionID = "5b1c7e2a-9d34-4f6b-8a0e-2c71d9f4e3b8"
	persistPrimeHookProviderSessionKey(claudeSessionID, &stderr)
	if got := reloadSessionKey(t, cityDir, id); got != claudeSessionID {
		t.Fatalf("session_key = %q, want the hook's id %q; stderr=%s", got, claudeSessionID, stderr.String())
	}
}

func reloadCopilotTranscript(t *testing.T, cityDir, id string) string {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}
	return strings.TrimSpace(b.Metadata[session.MetadataKeyCopilotTranscript])
}

// copilotCaptureEnv is the environment of the stock sessionStart hook of one
// managed Copilot incarnation. GC_HOOK_EVENT_NAME is deliberately empty: the
// capture must not depend on, or select, the SessionStart prime path.
func copilotCaptureEnv(t *testing.T, id, name, epoch, token string) {
	t.Helper()
	isolateProviderSessionEnv(t)
	t.Setenv("GC_SESSION_ID", id)
	t.Setenv("GC_SESSION_NAME", name)
	t.Setenv("GC_RUNTIME_EPOCH", epoch)
	t.Setenv("GC_INSTANCE_TOKEN", token)
	t.Setenv("GC_HOOK_EVENT_NAME", "")
}

func TestReadPrimeHookContextCopilotSessionID(t *testing.T) {
	t.Setenv("GC_HOOK_EVENT_NAME", "")
	setPrimeHookStdinJSON(t, map[string]string{"sessionId": "copilot-session", "source": "startup"})
	ctx := readPrimeHookContext()
	if ctx.CopilotSessionID != "copilot-session" || ctx.ProviderSessionID != "" || primeHookSessionStart(ctx) {
		t.Fatalf("Copilot context = %#v; want the id kept apart from the provider key and no SessionStart", ctx)
	}
}

func TestReadPrimeHookContextConflictingCopilotAliases(t *testing.T) {
	t.Setenv("GC_HOOK_EVENT_NAME", "")
	setPrimeHookStdinJSON(t, map[string]string{"sessionId": "one", "session_id": "another"})
	ctx := readPrimeHookContext()
	if ctx.CopilotSessionID != "" || ctx.ProviderSessionID != "another" {
		t.Fatalf("conflicting aliases = %#v; want no Copilot id and the generic key untouched", ctx)
	}
}

func TestPersistPrimeHookCopilotTranscript(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", cityDir)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"t\"\n[beads]\nprovider = \"file\"\nconditional_writes = \"auto\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	id := createCaptureSessionBead(t, store, "copilot")
	if err := store.SetMetadataBatch(id, map[string]string{"session_name": "copilot-seat", "state": "creating", "generation": "2", "instance_token": "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	copilotCaptureEnv(t, id, "copilot-seat", "2", "fixture-token")
	// A generic inherited provider id cannot replace the Copilot hook's id.
	t.Setenv("GC_PROVIDER_SESSION_ID", "unrelated-provider-key")
	var stderr bytes.Buffer
	persistPrimeHookCopilotTranscriptAtCity("copilot-session", cityDir, &stderr)
	if got := reloadCopilotTranscript(t, cityDir, id); got != "2:copilot-session" || stderr.Len() != 0 {
		t.Fatalf("captured %q; stderr=%s", got, stderr.String())
	}
	if got := reloadSessionKey(t, cityDir, id); got != "" {
		t.Fatalf("session_key = %q; capturing a transcript identity must not write the launch key", got)
	}
	persistPrimeHookCopilotTranscriptAtCity("conflicting-session", cityDir, &stderr)
	if got := reloadCopilotTranscript(t, cityDir, id); got != "2:copilot-session" {
		t.Fatalf("replaced identity: %q", got)
	}
	if !strings.Contains(stderr.String(), "not recorded") {
		t.Fatalf("a refused capture on a managed session must be diagnosed; stderr=%q", stderr.String())
	}
	t.Setenv("GC_INSTANCE_TOKEN", "stale-token")
	stderr.Reset()
	persistPrimeHookCopilotTranscriptAtCity("stale-session", cityDir, &stderr)
	if got := reloadCopilotTranscript(t, cityDir, id); got != "2:copilot-session" {
		t.Fatalf("stale incarnation replaced identity: %q", got)
	}
	if strings.Contains(stderr.String(), "fixture-token") || strings.Contains(stderr.String(), "stale-token") {
		t.Fatalf("diagnostic leaked a token: %q", stderr.String())
	}
}

// The generic provider-key path must keep refusing a Copilot id from hook
// stdin: Copilot's launch and resume stay keyed off session_key alone.
func TestPersistPrimeHookProviderSessionKeyIgnoresCopilotStdin(t *testing.T) {
	cityDir, store := primeCaptureTestStore(t)
	id := createCaptureSessionBead(t, store, "copilot")
	copilotCaptureEnv(t, id, "copilot-seat", "2", "fixture-token")
	var stderr bytes.Buffer
	persistPrimeHookProviderSessionKey("copilot-session", &stderr)
	if got := reloadSessionKey(t, cityDir, id); got != "" {
		t.Fatalf("generic path wrote session_key %q for Copilot", got)
	}
	if got := reloadCopilotTranscript(t, cityDir, id); got != "" {
		t.Fatalf("generic path wrote copilot_transcript %q", got)
	}
}

// A human-launched Copilot in a directory gc staged carries no gc identity; its
// hook stderr is shown in the provider UI, so the capture must stay silent.
func TestPersistPrimeHookCopilotTranscriptUnmanagedIsSilent(t *testing.T) {
	isolateProviderSessionEnv(t)
	for _, name := range append([]string{"GC_SESSION_NAME", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN"}, hookIdentityEnv...) {
		t.Setenv(name, "")
	}
	var stderr bytes.Buffer
	persistPrimeHookCopilotTranscriptAtCity("copilot-session", t.TempDir(), &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("unmanaged invocation wrote diagnostics: %q", stderr.String())
	}
}

func TestPrimeHookCopilotRelocatedSessionStore(t *testing.T) {
	cityDir, work := primeCaptureTestStore(t)
	binding := beads.NewMemStore()
	if err := beads.StampOpenedStore(binding, "MemStore", gate.Auto, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	seedSplitRoutes(t, cityDir, binding)
	id := createCaptureSessionBead(t, binding, "copilot")
	if err := binding.SetMetadataBatch(id, map[string]string{"session_name": "copilot-seat", "state": "active", "generation": "2", "instance_token": "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	copilotCaptureEnv(t, id, "copilot-seat", "2", "fixture-token")
	var stderr bytes.Buffer
	persistPrimeHookCopilotTranscriptAtCity("copilot-session", cityDir, &stderr)
	got, err := binding.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[session.MetadataKeyCopilotTranscript] != "2:copilot-session" || got.Metadata["session_key"] != "" {
		t.Fatalf("relocated capture = %q (session_key %q); stderr=%s", got.Metadata[session.MetadataKeyCopilotTranscript], got.Metadata["session_key"], stderr.String())
	}
	if _, err := work.Get(id); err == nil {
		t.Fatal("capture created a work-store copy")
	}
}
