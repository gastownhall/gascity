package eventexport

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursorsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")

	want := map[string]uint64{"c1": 42, "c2": 1000}
	if err := SaveCursors(path, want); err != nil {
		t.Fatalf("SaveCursors: %v", err)
	}
	got, err := LoadCursors(path)
	if err != nil {
		t.Fatalf("LoadCursors: %v", err)
	}
	if len(got) != len(want) || got["c1"] != 42 || got["c2"] != 1000 {
		t.Fatalf("round-trip mismatch: got %v want %v", got, want)
	}
}

// TestLoadCursors_MissingFileIsFreshStart pins the one non-error empty case: a
// first run with no cursor file yet floors each city at head, by design.
func TestLoadCursors_MissingFileIsFreshStart(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadCursors(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatalf("missing file must not error (fresh start), got %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("missing file: got %v, want empty non-nil", got)
	}
}

// TestLoadCursors_CorruptFileErrors proves a corrupt cursor file surfaces an
// error instead of silently resetting to empty — resetting would floor every
// tracked city at head and skip events accumulated since the last durable save.
func TestLoadCursors_CorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCursors(corrupt)
	if err == nil {
		t.Fatalf("corrupt file must error, got map %v", got)
	}
	if got != nil {
		t.Fatalf("corrupt file must return a nil map with the error, got %v", got)
	}
}

// TestLoadCursors_NullFileErrors proves a cursor file containing JSON null fails
// closed. json.Unmarshal accepts null for a map[string]uint64 and sets it to nil
// without an error, which would otherwise reach MuxSource as no durable cursors
// and floor every tracked city at head, skipping events accumulated since the
// last durable save.
func TestLoadCursors_NullFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "null.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCursors(path)
	if err == nil {
		t.Fatalf("null cursor file must error, got map %v", got)
	}
	if got != nil {
		t.Fatalf("null cursor file must return a nil map with the error, got %v", got)
	}
}

// TestLoadCursors_UnreadableFileErrors proves an existing-but-unreadable cursor
// file errors (permission), rather than being treated as a fresh start.
func TestLoadCursors_UnreadableFileErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permission checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")
	if err := os.WriteFile(path, []byte(`{"c1":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := LoadCursors(path); err == nil {
		t.Fatal("unreadable file must error, got nil")
	}
}

// TestSaveCursors_UnwritablePathErrors proves a save failure (here, a missing
// parent directory) propagates so the caller can log it rather than lose the
// cursor silently.
func TestSaveCursors_UnwritablePathErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing-subdir", "cursor.json")
	if err := SaveCursors(path, map[string]uint64{"c1": 1}); err == nil {
		t.Fatal("SaveCursors to an unwritable path must error, got nil")
	}
}
