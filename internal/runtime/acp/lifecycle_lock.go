package acp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lifecycle lock for %q: %w", name, err)
	}
	mode := syscall.LOCK_EX
	if nonblocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: session %q lifecycle operation in progress", runtime.ErrSessionExists, name)
		}
		return nil, fmt.Errorf("locking lifecycle for %q: %w", name, err)
	}
	return f, nil
}
