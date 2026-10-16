package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// writePoolWorkerTranscript writes a claude transcript discoverable for workDir
// under searchBase and returns its path.
func writePoolWorkerTranscript(t *testing.T, searchBase, workDir, name string) string {
	t.Helper()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(slugDir, name)
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// newPinTestSessionBead creates an open pool-worker session bead on workDir.
func newPinTestSessionBead(t *testing.T, store beads.Store, title, workDir string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  title,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":  title,
			"template":      "worker",
			"work_dir":      workDir,
			"provider":      "claude",
			"provider_kind": "claude",
			"pool_managed":  "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead %s: %v", title, err)
	}
	return b
}

// TestCloseBeadPinsTranscriptBeforeRetirement is the regression for a pooled
// worker that dies within seconds: it never captures a provider session key, so
// once its bead closes and a successor recycles the same agent home, the
// workdir fallback can no longer attribute a transcript and the worker becomes
// undiagnosable. Retirement must pin the transcript while it is still knowable.
func TestCloseBeadPinsTranscriptBeforeRetirement(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 8, 26, 0, 48, 10, 0, time.UTC)
	workDir := t.TempDir()
	searchBase := t.TempDir()

	sessionBead := newPinTestSessionBead(t, store, "worker-fast", workDir)
	want := writePoolWorkerTranscript(t, searchBase, workDir, "fast-worker.jsonl")

	if !closeBeadWithTranscriptSearchPaths(store, testSeatWork("", nil, store, nil), decidedSessionInfo(store, sessionBead.ID), "dead-runtime", now, []string{searchBase}, ioDiscard{}) {
		t.Fatal("closeBeadWithTranscriptSearchPaths returned false, want true")
	}

	got, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("session status = %q, want closed", got.Status)
	}
	if got.Metadata[session.PinnedTranscriptMetadataKey] != want {
		t.Fatalf("pinned transcript = %q, want %q", got.Metadata[session.PinnedTranscriptMetadataKey], want)
	}
}

// TestCloseBeadPinIsAttributableAcrossSuccessor pins the point of the fix: the
// dead worker's transcript stays resolvable after a successor session takes the
// same workdir, which is precisely when the unpinned fallback goes ambiguous.
func TestCloseBeadPinIsAttributableAcrossSuccessor(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 8, 26, 0, 48, 10, 0, time.UTC)
	workDir := t.TempDir()
	searchBase := t.TempDir()

	dead := newPinTestSessionBead(t, store, "worker-dead", workDir)
	want := writePoolWorkerTranscript(t, searchBase, workDir, "dead-worker.jsonl")

	if !closeBeadWithTranscriptSearchPaths(store, testSeatWork("", nil, store, nil), decidedSessionInfo(store, dead.ID), "drained", now, []string{searchBase}, ioDiscard{}) {
		t.Fatal("closeBeadWithTranscriptSearchPaths returned false, want true")
	}
	newPinTestSessionBead(t, store, "worker-successor", workDir)

	front := session.NewStore(beads.SessionStore{Store: store})
	info, err := front.Get(dead.ID)
	if err != nil {
		t.Fatalf("front.Get: %v", err)
	}
	if got := session.PinnedTranscriptPath(info); got != want {
		t.Fatalf("PinnedTranscriptPath = %q, want %q", got, want)
	}
}

// TestCloseBeadPinsNothingWhenTranscriptIsUnattributable keeps retirement
// honest: two live pool workers on one workdir with no session key cannot be
// told apart, so the closing bead records no transcript rather than a guess.
func TestCloseBeadPinsNothingWhenTranscriptIsUnattributable(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, 8, 26, 0, 48, 10, 0, time.UTC)
	workDir := t.TempDir()
	searchBase := t.TempDir()

	first := newPinTestSessionBead(t, store, "worker-one", workDir)
	newPinTestSessionBead(t, store, "worker-two", workDir)
	writePoolWorkerTranscript(t, searchBase, workDir, "latest.jsonl")

	if !closeBeadWithTranscriptSearchPaths(store, testSeatWork("", nil, store, nil), decidedSessionInfo(store, first.ID), "drained", now, []string{searchBase}, ioDiscard{}) {
		t.Fatal("closeBeadWithTranscriptSearchPaths returned false, want true")
	}

	got, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("get session bead: %v", err)
	}
	if pinned := got.Metadata[session.PinnedTranscriptMetadataKey]; pinned != "" {
		t.Fatalf("pinned transcript = %q, want empty when ambiguous", pinned)
	}
}

// TestResolveStoredSessionLogSourcePrefersPinnedTranscript closes the loop for
// `gc session logs`: a retired pool worker whose workdir a successor has taken
// resolves to its pinned transcript instead of reporting nothing found.
func TestResolveStoredSessionLogSourcePrefersPinnedTranscript(t *testing.T) {
	store := beads.NewMemStore()
	workDir := t.TempDir()
	searchBase := t.TempDir()
	want := writePoolWorkerTranscript(t, searchBase, workDir, "retired-worker.jsonl")

	dead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":                             "worker-1",
			"provider":                          "claude",
			"session_name":                      "worker-1",
			"state":                             "closed",
			"work_dir":                          workDir,
			session.PinnedTranscriptMetadataKey: want,
		},
	})
	if err != nil {
		t.Fatalf("create retired session bead: %v", err)
	}
	if err := store.Close(dead.ID); err != nil {
		t.Fatalf("close retired session bead: %v", err)
	}
	// The successor is what makes the unpinned workdir fallback ambiguous.
	newPinTestSessionBead(t, store, "worker-2", workDir)

	got, provider, ok, diagnostic := resolveStoredSessionLogSource("", nil, sessionFrontDoor(store), dead.ID, []string{searchBase})
	if !ok {
		t.Fatal("resolveStoredSessionLogSource() = not found, want found")
	}
	if diagnostic != "" {
		t.Fatalf("diagnostic = %q, want empty", diagnostic)
	}
	if provider != "claude" {
		t.Fatalf("provider = %q, want claude", provider)
	}
	if got != want {
		t.Fatalf("path = %q, want pinned %q", got, want)
	}
}
