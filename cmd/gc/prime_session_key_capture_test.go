package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
