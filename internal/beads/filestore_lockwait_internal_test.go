package beads

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// lockForWriteSiteCount is the number of FileStore writers. It matches
// lockWaitWriterCount in filestore_lockwait_test.go, which lists them.
const lockForWriteSiteCount = 19

// scriptedFlock replaces flockAcquire for one test: each call consumes the
// next scripted error (nil acquires) and records the options it was given.
type scriptedFlock struct {
	errs  []error
	calls []filelock.Options
}

func (s *scriptedFlock) acquire(_ *os.File, opts filelock.Options) (*filelock.Lock, error) {
	s.calls = append(s.calls, opts)
	if len(s.errs) == 0 {
		return nil, nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return nil, err
}

func withScriptedFlock(t *testing.T, s *scriptedFlock) {
	t.Helper()
	prev := flockAcquire
	flockAcquire = s.acquire
	t.Cleanup(func() { flockAcquire = prev })
}

// AT-F7: a free lock acquires even when the ctx is already done: the ctx
// bounds only the wait.
func TestAcquireFlockFreeLockWithDoneCtxAcquires(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "x.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test file
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lock, err := acquireFlock(ctx, f, "x.lock")
	if err != nil {
		t.Fatalf("acquireFlock on a free lock with a done ctx: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// AT-F7: a ctx that can never end keeps the plain blocking acquisition.
func TestAcquireFlockNonCancellableCtxBlocks(t *testing.T) {
	script := &scriptedFlock{}
	withScriptedFlock(t, script)
	if _, err := acquireFlock(context.Background(), &os.File{}, "x.lock"); err != nil {
		t.Fatal(err)
	}
	if len(script.calls) != 1 || script.calls[0].NonBlocking {
		t.Fatalf("calls = %+v, want one blocking call", script.calls)
	}
	if script.calls[0].Mode != filelock.Exclusive {
		t.Fatalf("mode = %v, want Exclusive", script.calls[0].Mode)
	}
}

// AT-F7: only contention polls; each retry waits one poll interval of the
// clock, and a cancellable ctx uses non-blocking attempts.
func TestAcquireFlockPollsOnlyOnContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &scriptedFlock{errs: []error{filelock.ErrBusy, filelock.ErrBusy, filelock.ErrBusy, nil}}
		withScriptedFlock(t, script)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		if _, err := acquireFlock(ctx, &os.File{}, "x.lock"); err != nil {
			t.Fatal(err)
		}
		if got, want := time.Since(start), 3*flockPollInterval; got != want {
			t.Fatalf("waited %v, want %v (three polls)", got, want)
		}
		if flockPollInterval != 10*time.Millisecond {
			t.Fatalf("flockPollInterval = %v, want 10ms", flockPollInterval)
		}
		for i, c := range script.calls {
			if !c.NonBlocking {
				t.Fatalf("call %d blocked, want non-blocking attempts under a cancellable ctx", i)
			}
		}
	})
}

// AT-F7: an error other than contention returns at once, wrapped with %w.
func TestAcquireFlockOtherErrorReturnsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &scriptedFlock{errs: []error{fmt.Errorf("flock: %w", syscall.EBADF), nil}}
		withScriptedFlock(t, script)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		_, err := acquireFlock(ctx, &os.File{}, "x.lock")
		if !errors.Is(err, syscall.EBADF) {
			t.Fatalf("err = %v, want EBADF", err)
		}
		if len(script.calls) != 1 || time.Since(start) != 0 {
			t.Fatalf("%d calls over %v, want one call and no wait", len(script.calls), time.Since(start))
		}
	})
}

// AT-F7: when the ctx ends during contention the helper returns ctx.Err() and
// leaves ErrStoreTimeout to its callers.
func TestAcquireFlockReturnsCtxErrWhenTheWaitEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &scriptedFlock{errs: []error{filelock.ErrBusy}}
		withScriptedFlock(t, script)
		// Every attempt after the scripted one is contended too.
		flockAcquire = func(_ *os.File, _ filelock.Options) (*filelock.Lock, error) {
			return nil, filelock.ErrBusy
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
		defer cancel()
		_, err := acquireFlock(ctx, &os.File{}, "x.lock")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if errors.Is(err, ErrStoreTimeout) {
			t.Fatalf("err = %v: the helper must not add ErrStoreTimeout, its callers do", err)
		}
	})
}

// AT-F5 (internal half): a failed Lock leaves fl.f untouched and the local
// descriptor closed, so a later Unlock has nothing to release.
func TestFileFlockFailedLockLeavesNoDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	foreign := NewFileFlock(path)
	if err := foreign.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer foreign.Unlock() //nolint:errcheck // test cleanup

	fl := NewFileFlock(path)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := fl.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock err = %v, want context.DeadlineExceeded", err)
	}
	if fl.f != nil || fl.lock != nil {
		t.Fatalf("failed Lock stored f=%v lock=%v, want both nil", fl.f, fl.lock)
	}
}

// AT-F8 (guard G-1): in the non-test sources, locker.Lock( appears only inside
// lockForWrite, and lockForWrite( has exactly one call site per writer. The
// sources are parsed, so a comment or a string cannot satisfy or break it.
func TestFileStoreEveryWriterTakesLockForWrite(t *testing.T) {
	dir := "."
	if root := bazeltest.OverrideRoot(); root != "" {
		dir = filepath.Join(root, "internal", "beads")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var lockCallsInside, lockCallsOutside, sites int
	var outside []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inside := fn.Name.Name == "lockForWrite"
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case sel.Sel.Name == "lockForWrite":
					sites++
				case sel.Sel.Name == "Lock" && isLockerSelector(sel.X):
					if inside {
						lockCallsInside++
					} else {
						lockCallsOutside++
						outside = append(outside, fmt.Sprintf("%s: %s", name, fn.Name.Name))
					}
				}
				return true
			})
		}
	}
	if lockCallsOutside != 0 {
		t.Errorf("locker.Lock( called outside lockForWrite in: %v", outside)
	}
	if lockCallsInside != 1 {
		t.Errorf("locker.Lock( appears %d times inside lockForWrite, want 1", lockCallsInside)
	}
	if sites != lockForWriteSiteCount {
		t.Errorf("lockForWrite( has %d call sites, want %d (one per writer)", sites, lockForWriteSiteCount)
	}
}

// isLockerSelector reports whether x is a selector ending in .locker.
func isLockerSelector(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "locker"
}

// A zero-value FileStore (no OpenFileStore) still locks for write: the writer
// slot is created lazily, so the bare literal other tests build works.
func TestLockForWriteZeroValueStore(t *testing.T) {
	fs := &FileStore{MemStore: NewMemStore(), locker: nopLocker{}}
	unlock, err := fs.lockForWrite()
	if err != nil {
		t.Fatalf("lockForWrite on a zero-value store: %v", err)
	}
	unlock()
	unlock2, err := fs.lockForWrite()
	if err != nil {
		t.Fatalf("second lockForWrite after unlock: %v", err)
	}
	unlock2()
}

// lockForWrite returns without taking fmu on failure: after a timeout a reader
// can still take it.
func TestLockForWriteTimeoutLeavesFmuAndSlotFree(t *testing.T) {
	fs := &FileStore{MemStore: NewMemStore(), path: "beads.json", lockWait: 10 * time.Millisecond, locker: waitForCtx{}}
	if _, err := fs.lockForWrite(); !errors.Is(err, ErrStoreTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockForWrite err = %v, want ErrStoreTimeout wrapping context.DeadlineExceeded", err)
	}
	fs.locker = nopLocker{}
	unlock, err := fs.lockForWrite()
	if err != nil {
		t.Fatalf("lockForWrite after a timeout: %v", err)
	}
	unlock()
}

// waitForCtx is a Locker that never grants the lock and ends with its ctx.
type waitForCtx struct{}

func (waitForCtx) Lock(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
func (waitForCtx) Unlock() error                  { return nil }
