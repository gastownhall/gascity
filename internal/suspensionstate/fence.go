package suspensionstate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// FenceFile returns the path of the lock file that orders suspension-state
// writes against a controller acting on the state it read. It sits beside
// the state file; the state file itself is replaced on every write, so it
// cannot carry the lock.
func FenceFile(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "runtime", "suspension-state.lock")
}

// Fence takes the city's suspension fence, an exclusive advisory lock that
// every [Save] through the OS filesystem also takes. A controller that is
// about to act on a scope being suspended — stopping its store — holds the
// fence from re-reading the state until the action is done, so a resume
// written by any process either lands before that re-read (and the action is
// skipped) or waits until the action has finished (and the resumed scope's
// next read finds a store nothing will stop under it).
//
// The lock is released when the returned func is called, or by the kernel if
// the process dies; the lock file itself carries no state. The release func
// is safe to call more than once.
func Fence(cityPath string) (release func(), err error) {
	path := FenceFile(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating suspension fence dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening suspension fence %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking suspension fence %s: %w", path, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unlockFile(f)
			_ = f.Close()
		})
	}, nil
}

// fenceFor takes the fence for a write through fs. Only the OS filesystem is
// shared with other processes; an in-memory filesystem has nothing to order
// against, so its writes take no lock.
func fenceFor(fs fsys.FS, cityPath string) (func(), error) {
	if _, ok := fs.(fsys.OSFS); !ok {
		return func() {}, nil
	}
	return Fence(cityPath)
}
