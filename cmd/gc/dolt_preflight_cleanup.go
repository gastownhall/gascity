package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var managedDoltPreflightCleanupFn = preflightManagedDoltCleanup

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
		manifest := filepath.Join(doltDir, "noms", "manifest")
		if _, err := os.Stat(manifest); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
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
		fmt.Fprintf(os.Stderr, "gc dolt preflight: quarantined phantom database %s -> %s\n", dbDir, dest) //nolint:errcheck // best-effort warning
	}
	return nil
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
	if _, err := exec.LookPath("lsof"); err != nil {
		return false, errManagedDoltOpenStateUnknown
	}
	out, err := exec.Command("lsof", path).CombinedOutput()
	if err == nil {
		return true, nil
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("lsof %s: %w: %s", path, err, strings.TrimSpace(string(out)))
}
