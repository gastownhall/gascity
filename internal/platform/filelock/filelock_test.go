package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestTimeoutAcquireIsBoundedByTimeoutNotRetryInterval pins that a timed
// acquisition gives up near its Timeout even when RetryEvery is far longer:
// each retry sleep is capped at the time left before the deadline.
func TestTimeoutAcquireIsBoundedByTimeoutNotRetryInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	holder := openAppend(t, path)
	contender := openAppend(t, path)

	held, err := Acquire(holder, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("acquiring the first lock: %v", err)
	}
	defer held.Release() //nolint:errcheck // test cleanup

	start := time.Now()
	lock, err := Acquire(contender, Options{
		Mode:       Exclusive,
		Timeout:    100 * time.Millisecond,
		RetryEvery: time.Hour,
		Label:      path,
	})
	elapsed := time.Since(start)
	if lock != nil {
		_ = lock.Release()
		t.Fatal("bounded acquisition returned a lock while the lock was held")
	}
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("bounded acquisition error = %v, want a timeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bounded acquisition took %s, want it bounded near the 100ms timeout, not RetryEvery", elapsed)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("releasing the first lock: %v", err)
	}
}
