package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// newPinTestManager creates a manager plus one session per title, all sharing
// workDir, mirroring the pool-worker shape where many short-lived sessions
// recycle through the same agent home.
func newPinTestManager(t *testing.T, workDir string, titles ...string) (*Manager, *Store, []Info) {
	t.Helper()
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(store, runtime.NewFake(), WithCityPath(t.TempDir()))
	infos := make([]Info, 0, len(titles))
	for _, title := range titles {
		info, err := mgr.CreateSession(context.Background(), CreateOptions{
			Template:  "helper",
			Title:     title,
			Command:   "claude",
			WorkDir:   workDir,
			Provider:  "claude",
			Resume:    ProviderResume{},
			Hints:     runtime.Config{},
			ExtraMeta: map[string]string{"session_origin": "manual"},
		})
		if err != nil {
			t.Fatalf("CreateSession %s: %v", title, err)
		}
		infos = append(infos, info)
	}
	return mgr, NewStore(beads.SessionStore{Store: store}), infos
}

// writeTranscript writes a transcript file discoverable by the workdir fallback.
func writeTranscript(t *testing.T, searchBase, workDir, name string) string {
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

// TestResolveTranscriptPinResolvesWhileSessionIsLive is the durability half of
// the fix: a pool worker that dies fast has an unambiguous transcript only
// while its bead is still open, because the ambiguity guard excludes closed
// same-workdir siblings. The pin must be taken in that window.
func TestResolveTranscriptPinResolvesWhileSessionIsLive(t *testing.T) {
	workDir := t.TempDir()
	searchBase := t.TempDir()
	_, front, infos := newPinTestManager(t, workDir, "worker")
	want := writeTranscript(t, searchBase, workDir, "worker.jsonl")

	got, err := front.ResolveTranscriptPin(infos[0].ID, []string{searchBase})
	if err != nil {
		t.Fatalf("ResolveTranscriptPin: %v", err)
	}
	if got != want {
		t.Fatalf("ResolveTranscriptPin = %q, want %q", got, want)
	}
}

// TestResolveTranscriptPinRefusesAmbiguousLiveSiblings keeps the pin honest:
// when two live sessions share a workdir and neither carries a session key, the
// transcript cannot be attributed, so nothing is pinned rather than pinning the
// wrong file onto a closing bead.
func TestResolveTranscriptPinRefusesAmbiguousLiveSiblings(t *testing.T) {
	workDir := t.TempDir()
	searchBase := t.TempDir()
	_, front, infos := newPinTestManager(t, workDir, "one", "two")
	writeTranscript(t, searchBase, workDir, "latest.jsonl")

	got, err := front.ResolveTranscriptPin(infos[1].ID, []string{searchBase})
	if err != nil {
		t.Fatalf("ResolveTranscriptPin: %v", err)
	}
	if got != "" {
		t.Fatalf("ResolveTranscriptPin = %q, want empty for ambiguous live siblings", got)
	}
}

// TestTranscriptPathClassifiedPrefersPinnedPathAfterClose is the diagnosability
// half: once the bead is closed and a successor session has taken the same
// workdir, the ladder can only report ambiguity. The pin taken before
// retirement keeps the dead worker's transcript readable.
func TestTranscriptPathClassifiedPrefersPinnedPathAfterClose(t *testing.T) {
	workDir := t.TempDir()
	searchBase := t.TempDir()
	mgr, front, infos := newPinTestManager(t, workDir, "dead", "successor")
	want := writeTranscript(t, searchBase, workDir, "dead.jsonl")

	if err := front.ApplyPatch(infos[0].ID, TranscriptPinPatch(want)); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if err := mgr.Close(context.Background(), testActor(mgr, ActorOperator), infos[0].ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path, lookup, err := mgr.TranscriptPathClassified(infos[0].ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPathClassified: %v", err)
	}
	if path != want {
		t.Fatalf("path = %q, want pinned %q", path, want)
	}
	if lookup != TranscriptFound {
		t.Fatalf("lookup = %v, want TranscriptFound", lookup)
	}
}

// TestTranscriptPathClassifiedIgnoresPinnedPathThatVanished refuses to hand back
// a path whose file has since been deleted; the ladder runs instead so the
// caller sees the real classification rather than a dangling path.
func TestTranscriptPathClassifiedIgnoresPinnedPathThatVanished(t *testing.T) {
	workDir := t.TempDir()
	searchBase := t.TempDir()
	mgr, front, infos := newPinTestManager(t, workDir, "dead", "successor")
	pinned := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir), "gone.jsonl")

	if err := front.ApplyPatch(infos[0].ID, TranscriptPinPatch(pinned)); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if err := mgr.Close(context.Background(), testActor(mgr, ActorOperator), infos[0].ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path, lookup, err := mgr.TranscriptPathClassified(infos[0].ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPathClassified: %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty when the pinned file is gone", path)
	}
	if lookup != TranscriptAmbiguous {
		t.Fatalf("lookup = %v, want TranscriptAmbiguous", lookup)
	}
}

// TestTranscriptPinPatchClearsOnEmptyPath keeps the patch total over the empty
// string, matching the metadata⇄Info codec contract.
func TestTranscriptPinPatchClearsOnEmptyPath(t *testing.T) {
	patch := TranscriptPinPatch("")
	if got, ok := patch[PinnedTranscriptMetadataKey]; !ok || got != "" {
		t.Fatalf("TranscriptPinPatch(\"\") = %v, want the key cleared", patch)
	}
}
