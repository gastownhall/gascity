package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

func openManagedDoltLifecycleLock(cityPath string) (*os.File, managedDoltRuntimeLayout, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, err
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		return nil, managedDoltRuntimeLayout{}, fmt.Errorf("create managed dolt lock dir: %w", err)
	}
	f, err := os.OpenFile(layout.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, fmt.Errorf("open managed dolt lock: %w", err)
	}
	return f, layout, nil
}

func tryManagedDoltLifecycleLock(f *os.File) (bool, error) {
	if f == nil {
		return false, fmt.Errorf("nil managed dolt lock file")
	}
	_, err := filelock.Acquire(f, filelock.Options{
		Mode:        filelock.Exclusive,
		NonBlocking: true,
		Label:       f.Name(),
	})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, filelock.ErrBusy) {
		return false, nil
	}
	return false, fmt.Errorf("lock managed dolt lifecycle: %w", err)
}

func releaseManagedDoltLifecycleLock(f *os.File) {
	if f == nil {
		return
	}
	_ = f.Close()
}
