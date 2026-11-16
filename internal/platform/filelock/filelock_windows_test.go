//go:build windows

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// openLockTarget creates path and returns an independent read/write handle on
// it. Each call is a separate handle, which is what makes the contention below
// real: Windows byte-range locks conflict per handle, so two handles in one
// process contend exactly as two processes would.
func openLockTarget(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening lock target %q: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestExclusiveLockExcludesASecondHandle is the core mutual-exclusion claim.
// Without it every caller believes it holds the lock and the critical section
// protects nothing.
func TestExclusiveLockExcludesASecondHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	holderFile := openLockTarget(t, path)
	contenderFile := openLockTarget(t, path)

	holder, err := Acquire(holderFile, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("acquiring the first lock: %v", err)
	}

	second, err := Acquire(contenderFile, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if second != nil {
		_ = second.Release()
		t.Fatal("a second handle acquired the same exclusive lock; the lock excludes nothing")
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("contended nonblocking acquisition error = %v, want ErrBusy", err)
	}

	// Once released, the same contender must be able to take it.
	if err := holder.Release(); err != nil {
		t.Fatalf("releasing the first lock: %v", err)
	}
	third, err := Acquire(contenderFile, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("acquiring after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("releasing the second lock: %v", err)
	}
}

// TestClosingTheHandleReleasesTheLock pins the crash-recovery path. A process
// that dies holding a lock never calls Release, so the kernel dropping the
// lock with the handle is the only thing that stops the file being locked
// forever.
func TestClosingTheHandleReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	holderFile, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening holder: %v", err)
	}
	if _, err := Acquire(holderFile, Options{Mode: Exclusive, Label: path}); err != nil {
		t.Fatalf("acquiring: %v", err)
	}

	contenderFile := openLockTarget(t, path)
	if _, err := Acquire(contenderFile, Options{Mode: Exclusive, NonBlocking: true, Label: path}); !errors.Is(err, ErrBusy) {
		t.Fatalf("precondition: contended acquisition = %v, want ErrBusy", err)
	}

	// Close without releasing, exactly as an abrupt exit would.
	if err := holderFile.Close(); err != nil {
		t.Fatalf("closing holder: %v", err)
	}

	lock, err := Acquire(contenderFile, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("acquiring after the holder's handle closed: %v; a lock that outlives its handle can never be recovered", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("releasing: %v", err)
	}
}

// TestTimeoutAcquisitionReportsContentionRatherThanBlockingForever pins the
// bounded-wait form callers use on a hot path: it must give up and say why.
func TestTimeoutAcquisitionReportsContentionRatherThanBlockingForever(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	holderFile := openLockTarget(t, path)
	contenderFile := openLockTarget(t, path)

	holder, err := Acquire(holderFile, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("acquiring the first lock: %v", err)
	}
	defer holder.Release() //nolint:errcheck

	start := time.Now()
	lock, err := Acquire(contenderFile, Options{
		Mode:       Exclusive,
		Timeout:    200 * time.Millisecond,
		RetryEvery: 10 * time.Millisecond,
		Label:      path,
	})
	elapsed := time.Since(start)
	if lock != nil {
		_ = lock.Release()
		t.Fatal("bounded acquisition succeeded while the lock was held")
	}
	if err == nil {
		t.Fatal("bounded acquisition returned no error on timeout")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), path) {
		t.Fatalf("timeout error = %v, want it to say it timed out and name %q", err, path)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("bounded acquisition gave up after %s, want it to wait out the full 200ms timeout", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bounded acquisition took %s, want it bounded near the 200ms timeout", elapsed)
	}
}

// TestSecondReleaseIsANoOp pins the idempotence callers rely on when a
// deferred release runs after an explicit one.
func TestSecondReleaseIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	f := openLockTarget(t, path)
	lock, err := Acquire(f, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second release: %v, want a no-op", err)
	}
}

func TestBlockingAcquireDrainsOverlappedRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlapped.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	holderFile, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer holderFile.Close() //nolint:errcheck
	holder, err := Acquire(holderFile, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		t.Fatalf("open overlapped contender: %v", err)
	}
	contenderFile := os.NewFile(uintptr(handle), path)
	if contenderFile == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("os.NewFile returned nil")
	}
	defer contenderFile.Close() //nolint:errcheck

	type result struct {
		lock *Lock
		err  error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	go func() {
		close(started)
		lock, err := Acquire(contenderFile, Options{Mode: Exclusive, Label: path})
		done <- result{lock: lock, err: err}
	}()
	<-started

	select {
	case got := <-done:
		if got.lock != nil {
			_ = got.lock.Release()
		}
		t.Fatalf("blocking acquire returned before release: %v", got.err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := holder.Release(); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("blocking acquire after release: %v", got.err)
		}
		if got.lock == nil {
			t.Fatal("blocking acquire returned a nil lock")
		}
		if err := got.lock.Release(); err != nil {
			t.Fatalf("release contender: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = windows.CancelIoEx(handle, nil)
		t.Fatal("blocking overlapped acquire did not complete after release")
	}
}

func TestAcquireAdvisoryFileLeavesDataReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("event\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer writer.Close() //nolint:errcheck

	lock, err := AcquireAdvisoryFile(writer, path, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("AcquireAdvisoryFile: %v", err)
	}
	defer lock.Release() //nolint:errcheck

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile while advisory lock held: %v", err)
	}
	if string(data) != "event\n" {
		t.Fatalf("ReadFile = %q, want %q", data, "event\n")
	}
}

// TestAdvisoryFileLockStillExcludesASecondWriter is the other half of the
// advisory contract, and the one that makes the readability above worth
// having. Moving the lock off the data file so readers are not blocked must
// not quietly turn it into a no-op: a second writer taking the same advisory
// lock must still be refused.
func TestAdvisoryFileLockStillExcludesASecondWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("event\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	first, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open first writer: %v", err)
	}
	defer first.Close() //nolint:errcheck
	held, err := AcquireAdvisoryFile(first, path, Options{Mode: Exclusive, Label: path})
	if err != nil {
		t.Fatalf("AcquireAdvisoryFile: %v", err)
	}

	second, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open second writer: %v", err)
	}
	defer second.Close() //nolint:errcheck
	contended, err := AcquireAdvisoryFile(second, path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if contended != nil {
		_ = contended.Release()
		t.Fatal("a second writer took the advisory lock while it was held; serialization is not happening")
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("contended advisory acquisition error = %v, want ErrBusy", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("releasing the advisory lock: %v", err)
	}
	after, err := AcquireAdvisoryFile(second, path, Options{Mode: Exclusive, NonBlocking: true, Label: path})
	if err != nil {
		t.Fatalf("acquiring the advisory lock after release: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatalf("releasing: %v", err)
	}
}

func TestOpenDirectoryLockUsesStableExternalIdentity(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	t.Setenv("LOCALAPPDATA", cache)
	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	firstFile, firstLabel, err := OpenDirectoryLock(store)
	if err != nil {
		t.Fatalf("OpenDirectoryLock first: %v", err)
	}
	defer firstFile.Close() //nolint:errcheck
	secondFile, secondLabel, err := OpenDirectoryLock(strings.ToUpper(store))
	if err != nil {
		t.Fatalf("OpenDirectoryLock second: %v", err)
	}
	defer secondFile.Close() //nolint:errcheck

	if firstLabel != secondLabel {
		t.Fatalf("lock identities differ: %q != %q", firstLabel, secondLabel)
	}
	if strings.HasPrefix(strings.ToUpper(firstLabel), strings.ToUpper(store)+string(os.PathSeparator)) {
		t.Fatalf("lock file %q was created inside censused store %q", firstLabel, store)
	}

	firstLock, err := Acquire(firstFile, Options{Mode: Exclusive, Label: firstLabel})
	if err != nil {
		t.Fatalf("acquire first: %v", err)
	}
	defer firstLock.Release() //nolint:errcheck
	secondLock, err := Acquire(secondFile, Options{
		Mode:        Exclusive,
		NonBlocking: true,
		Label:       secondLabel,
	})
	if secondLock != nil {
		_ = secondLock.Release()
		t.Fatal("contended acquisition returned a lock")
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("contended acquisition error = %v, want ErrBusy", err)
	}
}

func TestOpenDirectoryForSyncSupportsFlush(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenDirectoryForSync(dir)
	if err != nil {
		t.Fatalf("OpenDirectoryForSync: %v", err)
	}
	defer f.Close() //nolint:errcheck
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}
