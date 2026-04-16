package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireManagedDoltLifecycleLock(cityPath string) (*os.File, managedDoltRuntimeLayout, bool, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		return nil, managedDoltRuntimeLayout{}, false, fmt.Errorf("create managed dolt lock dir: %w", err)
	}
	f, err := os.OpenFile(layout.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, false, fmt.Errorf("open managed dolt lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		return f, layout, false, nil
	} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		_ = f.Close()
		return nil, managedDoltRuntimeLayout{}, false, fmt.Errorf("lock managed dolt lifecycle: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, managedDoltRuntimeLayout{}, false, fmt.Errorf("lock managed dolt lifecycle after wait: %w", err)
	}
	return f, layout, true, nil
}

func releaseManagedDoltLifecycleLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
