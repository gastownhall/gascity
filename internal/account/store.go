package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Path returns the accounts registry file path for a city.
func Path(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "accounts.json")
}

// LockPath returns the accounts registry lock file path.
func LockPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "accounts.lock")
}

// Load reads the account registry from disk. Returns an empty registry if
// the file does not exist.
func Load(cityPath string) (Registry, error) {
	data, err := os.ReadFile(Path(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return Registry{}, nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("read account registry: %w", err)
	}
	if len(data) == 0 {
		return Registry{}, nil
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registry{}, fmt.Errorf("parse account registry: %w", err)
	}
	return reg, nil
}

// WithRegistry locks, loads, mutates, and atomically rewrites the account
// registry. Follows the same flock + atomic write pattern as nudgequeue.
func WithRegistry(cityPath string, fn func(*Registry) error) error {
	dir := filepath.Dir(Path(cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating account registry dir: %w", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening account registry lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking account registry: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	reg, err := Load(cityPath)
	if err != nil {
		return err
	}
	if err := fn(&reg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal account registry: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, Path(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write account registry: %w", err)
	}
	return nil
}
