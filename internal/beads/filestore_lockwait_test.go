package beads_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// lockWaitWriterCount is the number of FileStore methods that write under the
// store lock. lockWaitWriters must list every one of them, and
// TestFileStoreEveryWriterTakesLockForWrite pins the same number against the
// lockForWrite call sites in the non-test sources.
const lockWaitWriterCount = 19

// lockWaitShortWait is the injected lock-wait bound for tests that exercise
// real flock I/O.
const lockWaitShortWait = 50 * time.Millisecond

// lockWaitWriter is one FileStore write path, driven against two seeded beads.
type lockWaitWriter struct {
	name string
	call func(s *beads.FileStore, a, b string) error
}

// lockWaitWriters lists every FileStore writer: the 11 in filestore.go, the 5
// conditional writers, and the three that live in their own files.
func lockWaitWriters() []lockWaitWriter {
	title := func(v string) beads.UpdateOpts { return beads.UpdateOpts{Title: &v} }
	return []lockWaitWriter{
		{"Create", func(s *beads.FileStore, _, _ string) error {
			_, err := s.Create(beads.Bead{Title: "new"})
			return err
		}},
		{"Update", func(s *beads.FileStore, a, _ string) error { return s.Update(a, title("t")) }},
		{"ReleaseIfCurrent", func(s *beads.FileStore, a, _ string) error {
			_, err := s.ReleaseIfCurrent(a, "nobody")
			return err
		}},
		{"Close", func(s *beads.FileStore, a, _ string) error { return s.Close(a) }},
		{"Reopen", func(s *beads.FileStore, a, _ string) error { return s.Reopen(a) }},
		{"Delete", func(s *beads.FileStore, a, _ string) error { return s.Delete(a) }},
		{"CloseAll", func(s *beads.FileStore, a, b string) error {
			_, err := s.CloseAll([]string{a, b}, map[string]string{"k": "v"})
			return err
		}},
		{"SetMetadata", func(s *beads.FileStore, a, _ string) error { return s.SetMetadata(a, "k", "v") }},
		{"SetMetadataBatch", func(s *beads.FileStore, a, _ string) error {
			return s.SetMetadataBatch(a, map[string]string{"k": "v"})
		}},
		{"DepAdd", func(s *beads.FileStore, a, b string) error { return s.DepAdd(a, b, "blocks") }},
		{"DepRemove", func(s *beads.FileStore, a, b string) error { return s.DepRemove(a, b) }},
		{"UpdateIfMatch", func(s *beads.FileStore, a, _ string) error { return s.UpdateIfMatch(a, 1, title("t")) }},
		{"CloseIfMatch", func(s *beads.FileStore, a, _ string) error { return s.CloseIfMatch(a, 1) }},
		{"DeleteIfMatch", func(s *beads.FileStore, a, _ string) error { return s.DeleteIfMatch(a, 1) }},
		{"CloseWithMetadataIfMatch", func(s *beads.FileStore, a, _ string) error {
			_, err := s.CloseWithMetadataIfMatch(a, 1, map[string]string{"k": "v"})
			return err
		}},
		{"CompareAndSetMetadataKey", func(s *beads.FileStore, a, _ string) error {
			_, err := s.CompareAndSetMetadataKey(a, "k", "", "v")
			return err
		}},
		{"CreateWithForeignIDBatch", func(s *beads.FileStore, _, _ string) error {
			return s.CreateWithForeignIDBatch([]beads.Bead{{ID: "fx-1", Title: "foreign"}})
		}},
		{"ApplyGuarded", func(s *beads.FileStore, a, _ string) error {
			_, err := s.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
				{Update: &beads.GuardedUpdate{ID: a, Opts: title("t")}},
			}})
			return err
		}},
		{"ImportVerbatim", func(s *beads.FileStore, _, _ string) error {
			return s.ImportVerbatim([]beads.Bead{{ID: "iv-1", Title: "imported"}})
		}},
	}
}

// conditionalWriters are the five writers that refuse before any lock wait
// when the store has conditional writes disabled.
func conditionalWriters() []lockWaitWriter {
	var out []lockWaitWriter
	for _, w := range lockWaitWriters() {
		switch w.name {
		case "UpdateIfMatch", "CloseIfMatch", "DeleteIfMatch", "CloseWithMetadataIfMatch", "CompareAndSetMetadataKey":
			out = append(out, w)
		}
	}
	return out
}

// openLockWaitStore opens an OS-backed FileStore with the given lock-wait
// bound and seeds two beads. It returns the store, its file path and the two
// seeded ids.
func openLockWaitStore(t *testing.T, opts ...beads.FileStoreOption) (s *beads.FileStore, path, a, b string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "beads.json")
	s, err := beads.OpenFileStore(fsys.OSFS{}, path, opts...)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	first, err := s.Create(beads.Bead{Title: "first"})
	if err != nil {
		t.Fatalf("seed first: %v", err)
	}
	second, err := s.Create(beads.Bead{Title: "second"})
	if err != nil {
		t.Fatalf("seed second: %v", err)
	}
	return s, path, first.ID, second.ID
}

// holdForeignFlock takes the store's flock through a second FileFlock, the way
// another process would, and releases it at test cleanup.
func holdForeignFlock(t *testing.T, path string) (release func()) {
	t.Helper()
	foreign := beads.NewFileFlock(path + ".lock")
	if err := foreign.Lock(context.Background()); err != nil {
		t.Fatalf("foreign Lock: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = foreign.Unlock() }) }
	t.Cleanup(release)
	return release
}

// storeSnapshot reads every row, closed ones included.
func storeSnapshot(t *testing.T, s *beads.FileStore) []beads.Bead {
	t.Helper()
	rows, err := s.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return rows
}

// AT-F1: with a foreign handle holding the flock, every writer gives up after
// the injected wait, reports ErrStoreTimeout (and the ctx deadline), and
// leaves both the file and the in-memory state exactly as they were.
func TestFileStoreWritersTimeOutOnAHeldFlock(t *testing.T) {
	writers := lockWaitWriters()
	if len(writers) != lockWaitWriterCount {
		t.Fatalf("writer table has %d entries, want %d", len(writers), lockWaitWriterCount)
	}
	for _, w := range writers {
		t.Run(w.name, func(t *testing.T) {
			s, path, a, b := openLockWaitStore(t, beads.WithFileStoreLockWait(lockWaitShortWait))
			holdForeignFlock(t, path)
			beforeFile, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeRows := storeSnapshot(t, s)

			err = w.call(s, a, b)
			if !errors.Is(err, beads.ErrStoreTimeout) {
				t.Fatalf("%s err = %v, want ErrStoreTimeout", w.name, err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s err = %v, want context.DeadlineExceeded too", w.name, err)
			}
			afterFile, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterFile) != string(beforeFile) {
				t.Fatalf("%s changed the store file on a timeout", w.name)
			}
			if got := storeSnapshot(t, s); !reflect.DeepEqual(got, beforeRows) {
				t.Fatalf("%s changed in-memory state on a timeout:\n got %+v\nwant %+v", w.name, got, beforeRows)
			}
		})
	}
}

// blockingLocker signals when Lock is entered, then blocks until released or
// its ctx ends. It stands in for a flock another process never lets go of.
type blockingLocker struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingLocker() *blockingLocker {
	return &blockingLocker{entered: make(chan struct{}), release: make(chan struct{})}
}

func (l *blockingLocker) Lock(ctx context.Context) error {
	l.once.Do(func() { close(l.entered) })
	select {
	case <-l.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *blockingLocker) Unlock() error { return nil }

// AT-F2: while a writer waits on the lock, a concurrent Get and List return at
// once instead of queuing behind it. The writer holds nothing readers need.
func TestFileStoreReadersDoNotQueueBehindAWaitingWriter(t *testing.T) {
	s, _, a, _ := openLockWaitStore(t, beads.WithFileStoreLockWait(time.Minute))
	locker := newBlockingLocker()
	s.SetLocker(locker)
	t.Cleanup(func() { close(locker.release) })

	writerDone := make(chan error, 1)
	go func() { writerDone <- s.Update(a, beads.UpdateOpts{Title: ptr("renamed")}) }()
	<-locker.entered

	readsDone := make(chan error, 1)
	go func() {
		if _, err := s.Get(a); err != nil {
			readsDone <- fmt.Errorf("Get: %w", err)
			return
		}
		if _, err := s.List(beads.ListQuery{AllowScan: true}); err != nil {
			readsDone <- fmt.Errorf("List: %w", err)
			return
		}
		readsDone <- nil
	}()
	select {
	case err := <-readsDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Get/List blocked behind a writer that is still waiting for the lock")
	}
	select {
	case err := <-writerDone:
		t.Fatalf("writer returned %v while the lock was still held", err)
	default:
	}
}

// signalLocker wraps a real FileFlock and signals when Lock is entered.
type signalLocker struct {
	inner   *beads.FileFlock
	entered chan struct{}
	once    sync.Once
}

func (l *signalLocker) Lock(ctx context.Context) error {
	l.once.Do(func() { close(l.entered) })
	return l.inner.Lock(ctx)
}

func (l *signalLocker) Unlock() error { return l.inner.Unlock() }

// AT-F3: a foreign flock released inside the budget lets the waiting writer
// through, and its write lands.
func TestFileStoreWriterProceedsWhenTheFlockIsReleasedInBudget(t *testing.T) {
	s, path, a, _ := openLockWaitStore(t, beads.WithFileStoreLockWait(time.Minute))
	locker := &signalLocker{inner: beads.NewFileFlock(path + ".lock"), entered: make(chan struct{})}
	s.SetLocker(locker)
	release := holdForeignFlock(t, path)

	done := make(chan error, 1)
	go func() { done <- s.Update(a, beads.UpdateOpts{Title: ptr("landed")}) }()
	<-locker.entered
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Update after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not proceed after the foreign flock was released")
	}
	got, err := s.Get(a)
	if err != nil || got.Title != "landed" {
		t.Fatalf("Get = %+v, %v, want the write to have landed", got, err)
	}
}

// countingLocker records the most Lock calls ever outstanding at once.
type countingLocker struct {
	cur, max atomic.Int32
}

func (l *countingLocker) Lock(context.Context) error {
	n := l.cur.Add(1)
	for {
		m := l.max.Load()
		if n <= m || l.max.CompareAndSwap(m, n) {
			break
		}
	}
	runtime.Gosched()
	return nil
}

func (l *countingLocker) Unlock() error {
	l.cur.Add(-1)
	return nil
}

// AT-F4: concurrent in-process writers queue on the writer slot, so at most
// one of them is ever inside Locker.Lock, and every write lands.
func TestFileStoreInProcessWritersShareOneLockHolder(t *testing.T) {
	const writers = 16
	s, err := beads.OpenFileStore(fsys.NewFake(), "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}
	locker := &countingLocker{}
	s.SetLocker(locker)

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Create(beads.Bead{Title: fmt.Sprintf("w%d", i)}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Create: %v", err)
	}
	if got := locker.max.Load(); got != 1 {
		t.Fatalf("max outstanding Lock calls = %d, want 1", got)
	}
	if rows := storeSnapshot(t, s); len(rows) != writers {
		t.Fatalf("%d rows landed, want %d (a lost update)", len(rows), writers)
	}
}

// openFDCount counts this process's open descriptors, or reports that the
// platform has no /proc/self/fd.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries)
}

// AT-F5: a timed-out wait leaks no descriptor and leaves the writer slot free:
// after N sequential timeouts, a write on a free lock succeeds.
func TestFileStoreTimedOutWaitsLeakNothing(t *testing.T) {
	const timeouts = 20
	s, path, a, _ := openLockWaitStore(t, beads.WithFileStoreLockWait(20*time.Millisecond))
	release := holdForeignFlock(t, path)

	// One warm-up timeout settles lazily opened descriptors before the count.
	if err := s.Update(a, beads.UpdateOpts{Title: ptr("x")}); !errors.Is(err, beads.ErrStoreTimeout) {
		t.Fatalf("warm-up err = %v, want ErrStoreTimeout", err)
	}
	before := openFDCount(t)
	for i := range timeouts {
		if err := s.Update(a, beads.UpdateOpts{Title: ptr("x")}); !errors.Is(err, beads.ErrStoreTimeout) {
			t.Fatalf("timeout %d err = %v, want ErrStoreTimeout", i, err)
		}
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("open descriptors %d -> %d across %d timeouts, want no leak", before, after, timeouts)
	}

	release()
	if err := s.Update(a, beads.UpdateOpts{Title: ptr("free")}); err != nil {
		t.Fatalf("Update on a free lock after %d timeouts: %v", timeouts, err)
	}
}

// entryLocker records whether Lock was ever entered and blocks until its ctx
// ends when it is.
type entryLocker struct {
	entered atomic.Bool
}

func (l *entryLocker) Lock(ctx context.Context) error {
	l.entered.Store(true)
	<-ctx.Done()
	return ctx.Err()
}

func (l *entryLocker) Unlock() error { return nil }

// AT-F6: with conditional writes disabled, the five conditional writers refuse
// at once, before any lock wait.
func TestFileStoreConditionalWritersRefuseBeforeWaiting(t *testing.T) {
	for _, w := range conditionalWriters() {
		t.Run(w.name, func(t *testing.T) {
			s, _, a, b := openLockWaitStore(t, beads.WithFileStoreLockWait(time.Minute))
			locker := &entryLocker{}
			s.SetLocker(locker)
			s.DisableConditionalWrites = true

			done := make(chan error, 1)
			go func() { done <- w.call(s, a, b) }()
			select {
			case err := <-done:
				if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
					t.Fatalf("%s err = %v, want ErrConditionalWriteUnsupported", w.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s waited for the lock instead of refusing", w.name)
			}
			if locker.entered.Load() {
				t.Fatalf("%s entered Locker.Lock before refusing", w.name)
			}
		})
	}
	if got := len(conditionalWriters()); got != 5 {
		t.Fatalf("conditional writer table has %d entries, want 5", got)
	}
}

// A non-positive lock wait keeps the default rather than meaning "forever" or
// "no wait": the zero value of the option must not turn a free lock into a
// timeout.
func TestFileStoreNonPositiveLockWaitKeepsTheDefault(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		s, _, a, _ := openLockWaitStore(t, beads.WithFileStoreLockWait(d))
		if err := s.Update(a, beads.UpdateOpts{Title: ptr("ok")}); err != nil {
			t.Fatalf("WithFileStoreLockWait(%v): Update on a free lock: %v", d, err)
		}
	}
}

// A Lock error that is not a deadline (the open failed, a non-contention
// errno) is returned as it is today, unmapped to ErrStoreTimeout.
func TestFileStoreLockErrorOtherThanDeadlineIsNotATimeout(t *testing.T) {
	s, err := beads.OpenFileStore(fsys.NewFake(), "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("lock boom")
	s.SetLocker(errLocker{lockErr: boom})
	_, err = s.Create(beads.Bead{Title: "x"})
	if !errors.Is(err, boom) {
		t.Fatalf("Create err = %v, want lock boom", err)
	}
	if errors.Is(err, beads.ErrStoreTimeout) {
		t.Fatalf("Create err = %v, a non-deadline lock error must not map to ErrStoreTimeout", err)
	}
}
