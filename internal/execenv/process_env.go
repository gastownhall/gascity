package execenv

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
)

// ProcessEnvLock serializes transient changes to this process's environment
// with the reads child environments are built from, the way syscall.ForkLock
// serializes descriptor creation with fork.
//
// A caller that projects variables with os.Setenv and restores them afterwards
// holds it for the whole projection: the native beads store open does, because
// the linked beads library configures itself from BEADS_* variables at open
// time. Environ holds it for one read. Without it, a child spawned while a
// projection is in flight inherits the projection: a session would keep
// another scope's database selector, and lose the BD_* variables the
// projection withholds, for its whole life.
var ProcessEnvLock sync.Mutex

// Environ returns os.Environ() taken while no transient projection holds
// ProcessEnvLock. Build a child process's environment from it, never from a
// bare os.Environ(), in any process that may also open a native beads store.
func Environ() []string {
	ProcessEnvLock.Lock()
	defer ProcessEnvLock.Unlock()
	return os.Environ()
}

// errProjectionWindowNotImplemented marks the RED-commit stubs below: they
// compile so the tests that pin the lock-free readers and the projection window
// fail at runtime, and the commit's lint gate passes without being skipped.
var errProjectionWindowNotImplemented = errors.New("execenv: projection window not implemented")

var (
	projectionSeq    atomic.Uint64
	projectionToken  = func() chan struct{} { c := make(chan struct{}, 1); c <- struct{}{}; return c }()
	cleanEnvSnapshot atomic.Pointer[[]string]
)

// EnterProjectionWindow is a RED-commit stub.
func EnterProjectionWindow(_ context.Context) (exit func(), err error) {
	return nil, errProjectionWindowNotImplemented
}

// LookupEnv is a RED-commit stub.
func LookupEnv(_ string) (string, bool) { return "", false }

// Getenv is a RED-commit stub.
func Getenv(_ string) string { return "" }

func lookupSnapshot(_ []string, _ string) (string, bool) { return "", false }
