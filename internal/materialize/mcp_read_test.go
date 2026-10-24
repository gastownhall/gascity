package materialize

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// returnsPromptly runs fn and fails the test if it has not returned within
// five seconds. A reader stuck opening fifo is released by opening fifo for
// writing, so a regression fails the test instead of hanging it.
func returnsPromptly(t *testing.T, fifo string, fn func()) {
	t.Helper()
	const limit = 5 * time.Second
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(limit):
	}
	// Release each blocked reader in turn: a non-blocking writer open lets
	// one waiting open return, and its read then sees end of file.
	release := time.NewTicker(20 * time.Millisecond)
	defer release.Stop()
	deadline := time.After(30 * time.Second)
	for {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		select {
		case <-done:
			t.Fatalf("blocked on the named pipe %s for more than %s (released by opening it for writing)", fifo, limit)
		case <-deadline:
			t.Fatalf("still blocked on the named pipe %s", fifo)
		case <-release.C:
		}
	}
}

func makeFIFO(t *testing.T) string {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return fifo
}

// guardedFS passes every call to the real filesystem but never reads the
// guarded paths: it records the attempt and fails it, so a test can show gc
// does not read them, and a regression fails instead of reading /dev/zero
// or a huge file.
type guardedFS struct {
	fsys.OSFS
	guarded map[string]bool
	mu      sync.Mutex
	reads   []string
}

func newGuardedFS(paths ...string) *guardedFS {
	g := &guardedFS{guarded: map[string]bool{}}
	for _, path := range paths {
		g.guarded[filepath.Clean(path)] = true
	}
	return g
}

func (g *guardedFS) ReadFile(name string) ([]byte, error) {
	if g.guarded[filepath.Clean(name)] {
		g.mu.Lock()
		g.reads = append(g.reads, name)
		g.mu.Unlock()
		return nil, errors.New("guarded path read: " + name)
	}
	return g.OSFS.ReadFile(name)
}

func (g *guardedFS) guardedReads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reads...)
}

// sparseFile creates a regular file of size bytes that takes no space.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// gc reads a provider MCP file only when it is a regular file of at most
// MaxMCPFileSize bytes: never through a symlink, never from a named pipe
// (which blocks), a device (/dev/zero never ends), or a directory, and never
// more than the cap.
func TestReadMCPFileReadsOnlyRegularFilesUpToTheCap(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(regular, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if data, err := ReadMCPFile(fsys.OSFS{}, regular); err != nil || string(data) != `{"mcpServers":{}}` {
		t.Fatalf("ReadMCPFile(regular) = %q, %v", data, err)
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	fifo := makeFIFO(t)
	big := filepath.Join(dir, "big.json")
	sparseFile(t, big, MaxMCPFileSize+1)
	for name, tc := range map[string]struct {
		path string
		want string
	}{
		"symlink":    {link, "a symlink, not a regular file"},
		"named pipe": {fifo, "a named pipe, not a regular file"},
		"device":     {"/dev/null", "a device, not a regular file"},
		"directory":  {dir, "a directory, not a regular file"},
		"too large":  {big, "4194305 bytes, more than the 4194304 gc reads"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, fs := range []fsys.FS{fsys.OSFS{}, newGuardedFS(tc.path)} {
				var err error
				returnsPromptly(t, fifo, func() { _, err = ReadMCPFile(fs, tc.path) })
				if err == nil || err.Error() != tc.want {
					t.Fatalf("ReadMCPFile(%s) with %T: err = %v, want %q", tc.path, fs, err, tc.want)
				}
				if g, ok := fs.(*guardedFS); ok && len(g.guardedReads()) != 0 {
					t.Fatalf("ReadMCPFile read %v", g.guardedReads())
				}
			}
		})
	}
}

// A named pipe where the provider file should be is not one gc wrote: gc
// leaves it alone, unread, rather than block on it.
func TestApplyMCPProjectionLeavesANamedPipeAlone(t *testing.T) {
	root := t.TempDir()
	proj := notesProjection(t, MCPProviderClaude, root)
	if err := syscall.Mkfifo(proj.Target, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	var err error
	returnsPromptly(t, proj.Target, func() { err = proj.Apply(fsys.OSFS{}) })
	var refused *MCPAdoptionRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Error(), "gc cannot read it (a named pipe, not a regular file)") {
		t.Fatalf("Apply error = %v, want a refusal for the named pipe", err)
	}
	if got := refused.Remedy(); got != "to let gc manage it, replace it with a regular file or remove it" {
		t.Fatalf("Remedy() = %q", got)
	}
	var plan MCPProjectionPlan
	returnsPromptly(t, proj.Target, func() { plan, err = proj.Plan(fsys.OSFS{}) })
	if err != nil || plan.Refused == nil {
		t.Fatalf("Plan = %+v, %v; want the refusal", plan, err)
	}
}

// An unmanaged provider file larger than gc reads is left alone, and it is
// not read to find that out.
func TestApplyMCPProjectionLeavesAnOversizedFileAloneUnread(t *testing.T) {
	root := t.TempDir()
	proj := notesProjection(t, MCPProviderGemini, root)
	if err := os.MkdirAll(filepath.Dir(proj.Target), 0o755); err != nil {
		t.Fatal(err)
	}
	sparseFile(t, proj.Target, 64<<20)
	guarded := newGuardedFS(proj.Target)
	refused, err := proj.AdoptionRefusal(guarded)
	if err != nil || refused == nil || refused.ReadErr == nil {
		t.Fatalf("AdoptionRefusal = %v, %v; want a refusal for the oversized file", refused, err)
	}
	if !strings.Contains(refused.Reason(), "67108864 bytes, more than the 4194304 gc reads") {
		t.Fatalf("Reason() = %q", refused.Reason())
	}
	if got := refused.Remedy(); got != "to let gc manage it, make it at most 4194304 bytes or remove it" {
		t.Fatalf("Remedy() = %q", got)
	}
	if reads := guarded.guardedReads(); len(reads) != 0 {
		t.Fatalf("the oversized file was read: %v", reads)
	}
}

// The doctor check reads a managed target's adoption snapshots. gc writes
// them as regular files; a symlink to a named pipe or a device, or a file
// larger than gc reads, is skipped unread.
func TestDroppedAtAdoptionSkipsSnapshotsGCDidNotWrite(t *testing.T) {
	root := t.TempDir()
	proj := notesProjection(t, MCPProviderClaude, root)
	markMCPManaged(t, root, MCPProviderClaude)
	snapshotDir := filepath.Join(root, ".gc", "mcp-adopted", MCPProviderClaude)
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "20260101T000000Z.json"), []byte(`{"mcpServers":{"team":{"command":"team"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := makeFIFO(t)
	if err := os.Symlink(fifo, filepath.Join(snapshotDir, "20260102T000000Z.json")); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(snapshotDir, "20260104T000000Z.json")
	sparseFile(t, big, 64<<20)

	var dropped []string
	var err error
	returnsPromptly(t, fifo, func() { dropped, _, err = proj.DroppedAtAdoption(fsys.OSFS{}) })
	if err != nil || !reflect.DeepEqual(dropped, []string{"team"}) {
		t.Fatalf("DroppedAtAdoption = %v, %v; want [team] from the regular snapshot", dropped, err)
	}

	// /dev/zero only behind the guard: a regression must fail, not read it.
	zero := filepath.Join(snapshotDir, "20260103T000000Z.json")
	if err := os.Symlink("/dev/zero", zero); err != nil {
		t.Fatal(err)
	}
	guarded := newGuardedFS(big, zero, filepath.Join(snapshotDir, "20260102T000000Z.json"))
	if dropped, _, err = proj.DroppedAtAdoption(guarded); err != nil || !reflect.DeepEqual(dropped, []string{"team"}) {
		t.Fatalf("DroppedAtAdoption(guarded) = %v, %v", dropped, err)
	}
	if reads := guarded.guardedReads(); len(reads) != 0 {
		t.Fatalf("snapshots gc did not write were read: %v", reads)
	}
}
