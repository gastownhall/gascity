package cityinit

import (
	"context"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ScaffoldFS extends [fsys.FS] with tree-walking, symlink, and
// recursive-remove operations needed by scaffold rollback.
type ScaffoldFS interface {
	fsys.FS
	Walk(root string, fn filepath.WalkFunc) error
	Readlink(name string) (string, error)
	Symlink(oldname, newname string) error
	RemoveAll(path string) error
}

// Registry manages the supervisor city registry.
type Registry interface {
	Register(ctx context.Context, dir, nameOverride string) error
	Find(ctx context.Context, name string) (RegisteredCity, error)
	Unregister(ctx context.Context, city RegisteredCity) error
}

// SupervisorReloader triggers supervisor configuration reloads.
type SupervisorReloader interface {
	Reload() error
	ReloadAfterUnregister() error
}

// Initializer performs the scaffold and finalize steps of city
// initialization. Implementations live at the process edge (CLI/API).
type Initializer interface {
	Scaffold(ctx context.Context, req InitRequest) error
	Finalize(ctx context.Context, req InitRequest) error
}
