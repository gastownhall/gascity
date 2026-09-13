package molecule

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestArtifactDirFor_Path(t *testing.T) {
	got := ArtifactDirFor("/city", "root-1", "step-7")
	want := filepath.Join("/city", ".gc", "molecules", "root-1", "artifacts", "step-7")
	if got != want {
		t.Errorf("ArtifactDirFor = %q, want %q", got, want)
	}
}

func TestDir_Path(t *testing.T) {
	got := Dir("/city", "root-1")
	want := filepath.Join("/city", ".gc", "molecules", "root-1")
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestEnsureArtifactDir_Creates(t *testing.T) {
	fake := fsys.NewFake()
	dir, err := EnsureArtifactDir(fake, "/city", "root-1", "step-7")
	if err != nil {
		t.Fatalf("EnsureArtifactDir: %v", err)
	}

	want := filepath.Join("/city", ".gc", "molecules", "root-1", "artifacts", "step-7")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("call count = %d, want 1", len(fake.Calls))
	}
	if fake.Calls[0].Method != "MkdirAll" {
		t.Errorf("method = %q, want MkdirAll", fake.Calls[0].Method)
	}
	if fake.Calls[0].Path != want {
		t.Errorf("path = %q, want %q", fake.Calls[0].Path, want)
	}
}

func TestEnsureArtifactDir_AlreadyExists(t *testing.T) {
	fake := fsys.NewFake()
	dir := filepath.Join("/city", ".gc", "molecules", "root-1", "artifacts", "step-7")
	fake.Dirs[dir] = true

	got, err := EnsureArtifactDir(fake, "/city", "root-1", "step-7")
	if err != nil {
		t.Fatalf("EnsureArtifactDir: %v", err)
	}
	if got != dir {
		t.Errorf("dir = %q, want %q", got, dir)
	}
}

func TestEnsureArtifactDir_MkdirError(t *testing.T) {
	fake := fsys.NewFake()
	dir := filepath.Join("/city", ".gc", "molecules", "root-1", "artifacts", "step-7")
	fake.Errors[dir] = os.ErrPermission

	_, err := EnsureArtifactDir(fake, "/city", "root-1", "step-7")
	if err == nil {
		t.Fatal("expected error from MkdirAll failure")
	}
	if !strings.Contains(err.Error(), "creating molecule artifact directory") {
		t.Errorf("error should have context, got: %v", err)
	}
}

func TestEnsureArtifactDir_EmptyRootID(t *testing.T) {
	fake := fsys.NewFake()
	_, err := EnsureArtifactDir(fake, "/city", "", "step-7")
	if err == nil {
		t.Fatal("expected error for empty rootID")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("MkdirAll should not be called when rootID is empty, got %d calls", len(fake.Calls))
	}
}

func TestEnsureArtifactDir_EmptyBeadID(t *testing.T) {
	fake := fsys.NewFake()
	_, err := EnsureArtifactDir(fake, "/city", "root-1", "")
	if err == nil {
		t.Fatal("expected error for empty beadID")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("MkdirAll should not be called when beadID is empty, got %d calls", len(fake.Calls))
	}
}

func TestEnsureArtifactDir_PathTraversalRootID(t *testing.T) {
	fake := fsys.NewFake()
	_, err := EnsureArtifactDir(fake, "/city", "../escape", "step-7")
	if err == nil {
		t.Fatal("expected error for path traversal in rootID")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("MkdirAll should not be called for unsafe rootID, got %d calls", len(fake.Calls))
	}
}

func TestEnsureArtifactDir_PathTraversalBeadID(t *testing.T) {
	fake := fsys.NewFake()
	_, err := EnsureArtifactDir(fake, "/city", "root-1", "../escape")
	if err == nil {
		t.Fatal("expected error for path traversal in beadID")
	}
}

func TestRemoveDir_Success(t *testing.T) {
	cityPath := t.TempDir()
	rootID := "root-1"
	dir := filepath.Join(cityPath, ".gc", "molecules", rootID, "artifacts", "step-7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RemoveDir(cityPath, rootID); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}

	if _, err := os.Stat(Dir(cityPath, rootID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("molecule dir still exists: %v", err)
	}
}

func TestRemoveDir_NotExist(t *testing.T) {
	cityPath := t.TempDir()
	// Directory was never created — purge should be a no-op.
	if err := RemoveDir(cityPath, "root-never-existed"); err != nil {
		t.Errorf("RemoveDir on missing dir should be no-op, got: %v", err)
	}
}

func TestRemoveDir_EmptyRootID(t *testing.T) {
	cityPath := t.TempDir()
	// Safety: an empty rootID could otherwise resolve to cityPath/.gc/molecules/
	// and wipe every molecule. Must error.
	if err := RemoveDir(cityPath, ""); err == nil {
		t.Fatal("expected error for empty rootID")
	}
}

func TestRemoveDir_EmptyCityPath(t *testing.T) {
	// Safety: empty cityPath would resolve against the caller's cwd and
	// could target molecule dirs under an unrelated tree. Must error.
	if err := RemoveDir("", "root-1"); err == nil {
		t.Fatal("expected error for empty cityPath")
	}
}

func TestRemoveDir_PathTraversal(t *testing.T) {
	cityPath := t.TempDir()
	// Create a sibling file that must NOT be deleted.
	sibling := filepath.Join(cityPath, "keep-me.txt")
	if err := os.WriteFile(sibling, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := RemoveDir(cityPath, "../")
	if err == nil {
		t.Fatal("expected error for path-traversal rootID")
	}

	// Sibling must still exist.
	if _, statErr := os.Stat(sibling); statErr != nil {
		t.Errorf("sibling file was destroyed by unsafe rootID: %v", statErr)
	}
}

func TestRemoveDir_AbsolutePathRootID(t *testing.T) {
	cityPath := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "do-not-delete.txt")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An absolute path as rootID could escape the containment check if we
	// naively joined. filepath.Join absorbs absolute paths, but we still
	// reject them defensively.
	err := RemoveDir(cityPath, outside)
	if err == nil {
		t.Fatal("expected error for absolute-path rootID")
	}

	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Errorf("sentinel file was destroyed: %v", statErr)
	}
}

// validateIDSegment rejects the literal ".." even when no path
// separator is present, because filepath.Join would otherwise let it
// climb out of the molecules directory. The other path-traversal
// tests pass IDs containing "/" (e.g. "../escape", "../"), which trip
// the separator check first; this one isolates the parent-reference
// branch.
func TestEnsureArtifactDir_LiteralDotDotRootID(t *testing.T) {
	fake := fsys.NewFake()
	_, err := EnsureArtifactDir(fake, "/city", "..", "step-7")
	if err == nil {
		t.Fatal("expected error for literal \"..\" rootID")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("MkdirAll should not be called for unsafe rootID, got %d calls", len(fake.Calls))
	}
}

// RemoveDir's containment check rejects a rootID of "." because
// filepath.Join collapses it, leaving absDir == absRoot (rel = ".")
// which would otherwise wipe the entire molecules root. validateIDSegment
// allows "." (single dot is not a parent reference); the containment
// check is the second line of defense.
func TestRemoveDir_DotRootIDRejectedByContainment(t *testing.T) {
	cityPath := t.TempDir()
	moleculesRoot := filepath.Join(cityPath, ".gc", "molecules", "real-root")
	if err := os.MkdirAll(moleculesRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := RemoveDir(cityPath, "."); err == nil {
		t.Fatal("expected error for \".\" rootID (would wipe molecules root)")
	}

	// The molecules root and its children must still exist.
	if _, err := os.Stat(moleculesRoot); err != nil {
		t.Errorf("molecules root was destroyed by \".\" rootID: %v", err)
	}
}
