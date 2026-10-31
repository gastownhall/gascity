package acp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/platform/filelock"
	"github.com/gastownhall/gascity/internal/runtime"
)

// lockLifecycle serializes sidecar and socket mutation across provider instances
// and processes. This is a kernel-held coordination lock, not a liveness record.
// Never unlink it: contenders must lock the same inode across incarnations.
func (p *Provider) lockLifecycle(name string, nonblocking bool) (*os.File, error) {
	if err := runtime.EnsurePrivateDir(p.dir); err != nil {
		return nil, err
	}
	path := filepath.Join(p.dir, metaFilePrefix(name)+".lifecycle-lock")
	f, err := openLifecycleLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening lifecycle lock for %q: %w", name, err)
	}
	lock, err := filelock.Acquire(f, filelock.Options{
		Mode:        filelock.Exclusive,
		NonBlocking: nonblocking,
		Label:       path,
	})
	if err != nil {
		_ = f.Close()
		if errors.Is(err, filelock.ErrBusy) {
			return nil, fmt.Errorf("%w: session %q lifecycle operation in progress", runtime.ErrSessionExists, name)
		}
		return nil, fmt.Errorf("locking lifecycle for %q: %w", name, err)
	}
	_ = lock
	return f, nil
}
