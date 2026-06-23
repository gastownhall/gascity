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
	got := LoadCursors(path)
	if len(got) != len(want) || got["c1"] != 42 || got["c2"] != 1000 {
		t.Fatalf("round-trip mismatch: got %v want %v", got, want)
	}

	// Missing file -> empty, non-nil map.
	if m := LoadCursors(filepath.Join(dir, "nope.json")); m == nil || len(m) != 0 {
		t.Fatalf("missing file: got %v, want empty non-nil", m)
	}

	// Corrupt file -> empty, non-nil map (resets rather than crashing).
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m := LoadCursors(corrupt); m == nil || len(m) != 0 {
		t.Fatalf("corrupt file: got %v, want empty non-nil", m)
	}
}
