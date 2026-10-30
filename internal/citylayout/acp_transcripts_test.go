package citylayout

import (
	"path/filepath"
	"testing"
)

func TestACPTranscriptsDirIsCityRooted(t *testing.T) {
	got := ACPTranscriptsDir("/city")
	want := filepath.Join("/city", ".gc", "transcripts", "acp")
	if got != want {
		t.Fatalf("ACPTranscriptsDir = %q, want %q", got, want)
	}
	if want != filepath.Join("/city", ACPTranscriptsRoot) {
		t.Fatalf("ACPTranscriptsRoot = %q does not match the directory helper", ACPTranscriptsRoot)
	}
}

func TestACPTranscriptPath(t *testing.T) {
	got, err := ACPTranscriptPath("/city", "s-1", "2")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	want := filepath.Join("/city", ".gc", "transcripts", "acp", "s-1", "2.jsonl")
	if got != want {
		t.Fatalf("ACPTranscriptPath = %q, want %q", got, want)
	}
}

// TestACPTranscriptPathMatchesDirForm binds the city-root form (readers) to
// the transcripts-dir form (the ACP provider, which is configured with the
// directory rather than the city root) so the two cannot drift apart.
func TestACPTranscriptPathMatchesDirForm(t *testing.T) {
	fromCity, err := ACPTranscriptPath("/city", "s-1", "7")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	fromDir, err := ACPTranscriptPathForDir(ACPTranscriptsDir("/city"), "s-1", "7")
	if err != nil {
		t.Fatalf("ACPTranscriptPathForDir: %v", err)
	}
	if fromCity != fromDir {
		t.Fatalf("city form %q != dir form %q", fromCity, fromDir)
	}
}

func TestACPTranscriptPathRejectsUnsafeComponents(t *testing.T) {
	cases := []struct {
		name, sessionID, epoch string
	}{
		{"empty session", "", "1"},
		{"empty epoch", "s-1", ""},
		{"slash in session", "a/b", "1"},
		{"backslash in session", `a\b`, "1"},
		{"slash in epoch", "s-1", "1/2"},
		{"dotdot session", "..", "1"},
		{"dot session", ".", "1"},
		{"dotdot epoch", "s-1", ".."},
		{"nul in session", "s\x00", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ACPTranscriptPath("/city", tc.sessionID, tc.epoch); err == nil {
				t.Fatalf("ACPTranscriptPath(%q, %q) = %q, want error", tc.sessionID, tc.epoch, got)
			}
			if got, err := ACPTranscriptPathForDir("/dir", tc.sessionID, tc.epoch); err == nil {
				t.Fatalf("ACPTranscriptPathForDir(%q, %q) = %q, want error", tc.sessionID, tc.epoch, got)
			}
		})
	}
}

func TestACPTranscriptPathForDirRejectsEmptyDir(t *testing.T) {
	if got, err := ACPTranscriptPathForDir("", "s-1", "1"); err == nil {
		t.Fatalf("ACPTranscriptPathForDir(\"\") = %q, want error", got)
	}
}
