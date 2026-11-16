//go:build !windows

package filelock

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestAcquireAdvisoryFileExcludesADataFileLocker covers a process that locks
// only the data file itself. AcquireAdvisoryFile must still wait for it, and
// must not keep the sidecar when it gives up.
func TestAcquireAdvisoryFileExcludesADataFileLocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	other, err := Acquire(openAppend(t, path), Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("locking the data file: %v", err)
	}
	writer := openAppend(t, path)

	contended, err := AcquireAdvisoryFile(writer, path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if !errors.Is(err, ErrBusy) {
		if contended != nil {
			_ = contended.Release()
		}
		t.Fatalf("AcquireAdvisoryFile while the data file is locked: err = %v, want %v", err, ErrBusy)
	}
	sidecar, err := acquireSidecar(path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("the sidecar is still held after AcquireAdvisoryFile gave up: %v", err)
	}
	if err := sidecar.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := other.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	lock, err := AcquireAdvisoryFile(writer, path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("AcquireAdvisoryFile after the data file is unlocked: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
