package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/platform/livefile"
)

func openAppend(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := livefile.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() }) //nolint:errcheck // test cleanup
	return f
}

// TestAcquireAdvisoryFileHoldsAcrossReplacement covers two writers holding
// different files at one path: one opened before the file was renamed away
// and replaced, one opened after. While the first holds the lock, the second
// must not get it.
func TestAcquireAdvisoryFileHoldsAcrossReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	before := openAppend(t, path)
	held, err := AcquireAdvisoryFile(before, path, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("AcquireAdvisoryFile: %v", err)
	}
	if err := os.Rename(path, path+".rotated"); err != nil {
		t.Fatal(err)
	}
	after := openAppend(t, path)

	contended, err := AcquireAdvisoryFile(after, path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if !errors.Is(err, ErrBusy) {
		if contended != nil {
			_ = contended.Release()
		}
		t.Fatalf("locking the replacement while the original's holder has the lock: err = %v, want %v", err, ErrBusy)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	lock, err := AcquireAdvisoryFile(after, path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("locking the replacement after release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
