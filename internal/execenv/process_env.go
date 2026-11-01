package execenv

import (
	"os"
	"sync"
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
