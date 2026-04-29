package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	managedDoltPreflightCleanupFn     = preflightManagedDoltCleanup
	retiredManagedDoltDatabasePattern = regexp.MustCompile(`^.+\.replaced-[0-9]{8}T[0-9]{6}Z$`)
)

const managedDoltLsofTimeout = 3 * time.Second

func preflightManagedDoltCleanup(cityPath string) error {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return err
	}
	if err := removeStaleManagedDoltSockets(); err != nil {
		return err
	}
	if err := quarantinePhantomManagedDoltDatabases(layout.DataDir, time.Now().UTC()); err != nil {
		return err
	}
	if err := removeStaleManagedDoltLocks(layout.DataDir); err != nil {
		return err
	}
	return nil
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

func quarantinePhantomManagedDoltDatabases(dataDir string, now time.Time) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	quarantineRoot := filepath.Join(dataDir, ".quarantine")
	stamp := now.UTC().Format("20060102T150405")
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dbDir := filepath.Join(dataDir, entry.Name())
		doltDir := filepath.Join(dbDir, ".dolt")
		info, err := os.Stat(doltDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.IsDir() {
			continue
		}
		reason := "retired replacement"
		if !retiredManagedDoltDatabaseName(entry.Name()) {
			reason = "missing noms/manifest"
			manifest := filepath.Join(doltDir, "noms", "manifest")
			if _, err := os.Stat(manifest); err == nil {
				continue
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.MkdirAll(quarantineRoot, 0o755); err != nil {
			return err
		}
		dest, err := uniqueQuarantineDestination(quarantineRoot, stamp, entry.Name())
		if err != nil {
			return err
		}
		if err := os.Rename(dbDir, dest); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "gc dolt preflight: quarantined unservable database (%s) %s -> %s\n", reason, dbDir, dest) //nolint:errcheck // best-effort warning
	}
	return nil
}

func retiredManagedDoltDatabaseName(name string) bool {
	name = strings.TrimSpace(name)
	return retiredManagedDoltDatabasePattern.MatchString(name)
}

func uniqueQuarantineDestination(root, stamp, name string) (string, error) {
	base := filepath.Join(root, stamp+"-"+name)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base, nil
	} else if err != nil {
		return "", err
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique quarantine destination for %s", name)
}

func removeStaleManagedDoltLocks(dataDir string) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		lockFile := filepath.Join(dataDir, entry.Name(), ".dolt", "noms", "LOCK")
		if _, err := os.Stat(lockFile); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		open, err := fileOpenedByAnyProcess(lockFile)
		if err != nil {
			if errors.Is(err, errManagedDoltOpenStateUnknown) {
				continue
			}
			return err
		}
		if open {
			continue
		}
		if err := os.Remove(lockFile); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
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
