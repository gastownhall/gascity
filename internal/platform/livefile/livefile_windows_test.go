//go:build windows

package livefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtendedLengthPath(t *testing.T) {
	long := `C:\` + strings.Repeat(`segment\`, 40) + "events.jsonl"
	longUNC := `\\server\share\` + strings.Repeat(`segment\`, 40) + "events.jsonl"
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "short absolute", path: `C:\city\.gc\events.jsonl`, want: `C:\city\.gc\events.jsonl`},
		{name: "long absolute", path: long, want: `\\?\` + long},
		{name: "long with forward slashes", path: filepath.ToSlash(long), want: `\\?\` + long},
		{name: "long with dot segments", path: `C:\x\..\` + long[3:], want: `\\?\` + long},
		{name: "long UNC", path: longUNC, want: `\\?\UNC\` + longUNC[2:]},
		{name: "already extended", path: `\\?\` + long, want: `\\?\` + long},
		{name: "device namespace", path: `\\.\pipe\` + strings.Repeat("p", 300), want: `\\.\pipe\` + strings.Repeat("p", 300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extendedLengthPath(tc.path); got != tc.want {
				t.Fatalf("extendedLengthPath(%q)\n got %q\nwant %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestLongPath opens and stats a file whose path is longer than MAX_PATH
// through every livefile entry point.
func TestLongPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", 100), strings.Repeat("e", 100), strings.Repeat("f", 100))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if len(path) <= 260 {
		t.Fatalf("test path is only %d characters, want more than MAX_PATH", len(path))
	}

	w, err := OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := w.Write([]byte("one\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	info, err := Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 4 {
		t.Fatalf("size = %d, want 4", info.Size())
	}
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close() //nolint:errcheck // test cleanup
	data := make([]byte, 8)
	n, _ := r.Read(data)
	if string(data[:n]) != "one\n" {
		t.Fatalf("read %q, want %q", data[:n], "one\n")
	}
}
