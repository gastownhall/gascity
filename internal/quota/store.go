package quota

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

// StatePath returns the quota state file path for a city.
func StatePath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "quota.json")
}

// LockPath returns the quota state lock file path.
func LockPath(cityPath string) string {
	return citylayout.RuntimePath(cityPath, "quota.lock")
}

// LoadState reads the quota state from disk. Returns an empty state if
// the file does not exist.
func LoadState(cityPath string) (QuotaState, error) {
	data, err := os.ReadFile(StatePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return QuotaState{}, nil
	}
	if err != nil {
		return QuotaState{}, fmt.Errorf("read quota state: %w", err)
	}
	if len(data) == 0 {
		return QuotaState{}, nil
	}
	var state QuotaState
	if err := json.Unmarshal(data, &state); err != nil {
		return QuotaState{}, fmt.Errorf("parse quota state: %w", err)
	}
	return state, nil
}

// WithState locks, loads, mutates, and atomically rewrites the quota state.
func WithState(cityPath string, fn func(*QuotaState) error) error {
	dir := filepath.Dir(StatePath(cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating quota state dir: %w", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening quota state lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking quota state: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	state, err := LoadState(cityPath)
	if err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal quota state: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, StatePath(cityPath), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write quota state: %w", err)
	}
	return nil
}
