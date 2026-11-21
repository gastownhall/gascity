package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// managedDoltRecoveryStateVersion is the schema version of the recovery
// history record. A record with any other version is discarded.
const managedDoltRecoveryStateVersion = 1

// managedDoltRecoveryStateFileName is the recovery history record kept in the
// managed dolt pack state directory, next to (and separate from) the
// published runtime state.
const managedDoltRecoveryStateFileName = "dolt-recovery-state.json"

// managedDoltServerIdentity names one managed dolt server instance: a PID
// alone can be reused, so the kernel process start time and the port it
// listens on are part of the identity.
type managedDoltServerIdentity struct {
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
	Port      int    `json:"port"`
}

// managedDoltUnresponsiveStreak records consecutive failed SQL observations
// of one live, listening server instance.
type managedDoltUnresponsiveStreak struct {
	managedDoltServerIdentity
	FirstFailedAt time.Time `json:"first_failed_at"`
	LastFailedAt  time.Time `json:"last_failed_at"`
	Failures      int       `json:"failures"`
}

// managedDoltRecoveryState is the cross-invocation recovery history for one
// city's managed dolt. It records past observations only; whether a server
// is running is always queried live. Streak and LastProviderAttemptAt are
// independent: clearing one never erases the other.
type managedDoltRecoveryState struct {
	Version               int                            `json:"version"`
	Streak                *managedDoltUnresponsiveStreak `json:"streak,omitempty"`
	LastProviderAttemptAt *time.Time                     `json:"last_provider_attempt_at,omitempty"`
}

// managedDoltRecoveryStatePath returns the recovery history record path for
// layout.
func managedDoltRecoveryStatePath(layout managedDoltRuntimeLayout) string {
	return filepath.Join(layout.PackStateDir, managedDoltRecoveryStateFileName)
}

// readManagedDoltRecoveryState reads the recovery history at path. A missing
// record reads as empty history. A record that cannot be decoded, has an
// unsupported version, or holds structurally invalid entries is discarded
// (wholly or per entry) and the returned diagnostic says why, so the caller
// starts fresh visibly. Any other read failure is returned as an error.
func readManagedDoltRecoveryState(path string) (managedDoltRecoveryState, string, error) {
	empty := managedDoltRecoveryState{Version: managedDoltRecoveryStateVersion}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, "", nil
	}
	if err != nil {
		return managedDoltRecoveryState{}, "", fmt.Errorf("read managed dolt recovery history %s: %w", path, err)
	}
	var state managedDoltRecoveryState
	if err := json.Unmarshal(data, &state); err != nil {
		return empty, fmt.Sprintf("discarding unreadable managed dolt recovery history %s: %v", path, err), nil
	}
	if state.Version != managedDoltRecoveryStateVersion {
		return empty, fmt.Sprintf("discarding managed dolt recovery history %s: unsupported version %d (want %d)", path, state.Version, managedDoltRecoveryStateVersion), nil
	}
	var problems []string
	if streak := state.Streak; streak != nil && !streak.valid() {
		state.Streak = nil
		problems = append(problems, "invalid unresponsive streak")
	}
	if last := state.LastProviderAttemptAt; last != nil && last.IsZero() {
		state.LastProviderAttemptAt = nil
		problems = append(problems, "invalid provider recovery attempt time")
	}
	if len(problems) > 0 {
		return state, fmt.Sprintf("discarding %s in managed dolt recovery history %s", strings.Join(problems, " and "), path), nil
	}
	return state, "", nil
}

// valid reports whether the streak names a concrete instance and holds at
// least one recorded failure.
func (s managedDoltUnresponsiveStreak) valid() bool {
	return s.PID > 0 && s.Port > 0 && strings.TrimSpace(s.StartTime) != "" &&
		s.Failures > 0 && !s.FirstFailedAt.IsZero() && !s.LastFailedAt.IsZero()
}

// writeManagedDoltRecoveryState atomically replaces the recovery history at
// path. Callers hold the managed dolt lifecycle lock.
func writeManagedDoltRecoveryState(path string, state managedDoltRecoveryState) error {
	state.Version = managedDoltRecoveryStateVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode managed dolt recovery history: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create managed dolt recovery history dir: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write managed dolt recovery history %s: %w", path, err)
	}
	return nil
}
