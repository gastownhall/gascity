package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// loadQuotaState reads and parses the quota state file at path.
// If the file does not exist (first-run), returns an empty QuotaState
// with an initialized Accounts map — not an error.
// If the file contains malformed JSON, returns an error with the file
// path and recovery instructions.
func loadQuotaState(path string) (*config.QuotaState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &config.QuotaState{
			Accounts: make(map[string]config.QuotaAccountState),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading quota state %s: %w", path, err)
	}

	var state config.QuotaState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("quota.json is malformed — run gc quota clear --all --force to reset: %w", err)
	}
	if state.Accounts == nil {
		state.Accounts = make(map[string]config.QuotaAccountState)
	}
	return &state, nil
}

// saveQuotaState writes the quota state to path atomically using
// a temp file + os.Rename pattern per CLAUDE.md conventions.
func saveQuotaState(path string, state *config.QuotaState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating quota state dir: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding quota state: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing temp quota state file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("renaming quota state file: %w", err)
	}
	return nil
}

// withQuotaLock acquires an exclusive flock on quotaPath + ".lock",
// reads the quota state AFTER lock acquisition (TOCTOU prevention),
// calls fn with the loaded state, and if fn returns nil, saves the
// (possibly modified) state atomically. The flock is released on return
// regardless of outcome. If fn returns an error, the state is NOT
// persisted and the error is returned.
//
// If the lock cannot be acquired within timeout, returns an error with
// the exact PRD message.
func withQuotaLock(quotaPath string, timeout time.Duration, fn func(state *config.QuotaState) error) error {
	lockPath := quotaPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("creating lock dir: %w", err)
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening quota lock: %w", err)
	}
	defer f.Close() //nolint:errcheck

	// Attempt LOCK_EX|LOCK_NB in a retry loop with 50ms intervals.
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break // lock acquired
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("error: quota state is locked by another rotation in progress. Try again in a moment.") //nolint:revive,staticcheck // PRD-specified user-facing message
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck

	// Read state AFTER flock acquired (TOCTOU prevention).
	state, err := loadQuotaState(quotaPath)
	if err != nil {
		return err
	}

	// Execute callback.
	if err := fn(state); err != nil {
		return err
	}

	// Persist the modified state.
	return saveQuotaState(quotaPath, state)
}
