package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var managedDoltPreflightCleanupFn = preflightManagedDoltCleanup

const managedDoltLsofTimeout = 3 * time.Second

func preflightManagedDoltCleanup(_ string) error {
	return removeStaleManagedDoltSockets()
}

var errManagedDoltOpenStateUnknown = errors.New("managed dolt open-file state unknown")

func removeStaleManagedDoltSockets() error {
	for _, path := range staleManagedDoltSocketPaths() {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSocket == 0 {
			continue
		}
		open, err := fileOpenedByAnyProcess(path)
		if err != nil {
			if errors.Is(err, errManagedDoltOpenStateUnknown) {
				continue
			}
			return err
		}
		if open {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func staleManagedDoltSocketPaths() []string {
	seen := map[string]struct{}{}
	paths := make([]string, 0, 8)
	add := func(path string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	matches, _ := filepath.Glob("/tmp/dolt*.sock")
	for _, match := range matches {
		add(match)
	}
	return paths
}

func fileOpenedByAnyProcess(path string) (bool, error) {
	if open, checked := fileOpenedByAnyProcessFromProc(path); checked {
		return open, nil
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return false, errManagedDoltOpenStateUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltLsofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", path)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return false, errManagedDoltOpenStateUnknown
	}
	if err == nil {
		return true, nil
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("lsof %s: %w: %s", path, err, strings.TrimSpace(string(out)))
}

func fileOpenedByAnyProcessFromProc(path string) (bool, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	socketInodes, _ := unixSocketInodesForPath(path)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			target = strings.TrimSuffix(target, " (deleted)")
			if samePath(target, path) {
				return true, true
			}
			if len(socketInodes) > 0 && strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
				inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
				if _, ok := socketInodes[inode]; ok {
					return true, true
				}
			}
		}
	}
	return false, true
}

func unixSocketInodesForPath(path string) (map[string]struct{}, bool) {
	data, err := os.ReadFile("/proc/net/unix")
	if err != nil {
		return nil, false
	}
	inodes := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 {
			continue
		}
		if !samePath(fields[len(fields)-1], path) {
			continue
		}
		inodes[fields[6]] = struct{}{}
	}
	return inodes, true
}
