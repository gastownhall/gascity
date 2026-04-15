package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// ---------------------------------------------------------------------------
// loadQuotaState tests
// ---------------------------------------------------------------------------

// TestLoadQuotaState_MissingFile verifies that loading from a non-existent file
// returns an empty QuotaState (not an error). This supports first-run scenarios
// where quota.json does not yet exist.
func TestLoadQuotaState_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.json")

	state, err := loadQuotaState(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == nil {
		t.Fatal("expected non-nil state for missing file")
	}
	if len(state.Accounts) != 0 {
		t.Errorf("expected empty Accounts map, got %d entries", len(state.Accounts))
	}
}

// TestLoadQuotaState_ValidRoundTrip verifies that saving then loading a
// QuotaState produces an identical result (JSON round-trip).
func TestLoadQuotaState_ValidRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.json")

	original := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T14:00:00Z",
				ResetsAt:  "2026-04-07T15:00:00Z",
			},
			"work2": {
				Status:   config.QuotaStatusAvailable,
				LastUsed: "2026-04-07T13:00:00Z",
			},
		},
	}

	if err := saveQuotaState(path, original); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	loaded, err := loadQuotaState(path)
	if err != nil {
		t.Fatalf("loadQuotaState: %v", err)
	}

	// Compare JSON representations for deep equality.
	origJSON, _ := json.Marshal(original)
	loadJSON, _ := json.Marshal(loaded)
	if string(origJSON) != string(loadJSON) {
		t.Errorf("round-trip mismatch:\n  original: %s\n  loaded:   %s", origJSON, loadJSON)
	}
}

// TestLoadQuotaState_CorruptJSON verifies that a malformed JSON file returns
// an error matching the PRD-specified message: "quota.json is malformed —
// run gc quota clear --all --force to reset".
func TestLoadQuotaState_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.json")

	if err := os.WriteFile(path, []byte("{not valid json!!!"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := loadQuotaState(path)
	if err == nil {
		t.Fatal("expected error for corrupt JSON")
	}
	errMsg := err.Error()
	// PRD: error should say "malformed".
	if !strings.Contains(errMsg, "malformed") {
		t.Errorf("error should contain 'malformed' per PRD, got: %s", errMsg)
	}
	// PRD: error should include the exact recovery command.
	if !strings.Contains(errMsg, "gc quota clear --all --force") {
		t.Errorf("error should contain 'gc quota clear --all --force' per PRD, got: %s", errMsg)
	}
}

// ---------------------------------------------------------------------------
// saveQuotaState tests
// ---------------------------------------------------------------------------

// TestSaveQuotaState_Atomic verifies that the file is written atomically
// (temp file + rename pattern). After save, the file should exist and be
// valid JSON. We verify no partial writes by checking the file is always
// valid JSON even if read during write.
func TestSaveQuotaState_Atomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota.json")

	state := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {
				Status:    config.QuotaStatusLimited,
				LimitedAt: "2026-04-07T14:00:00Z",
			},
		},
	}

	if err := saveQuotaState(path, state); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	// Verify the file exists and is valid JSON.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var loaded config.QuotaState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}

	if loaded.Accounts["work1"].Status != config.QuotaStatusLimited {
		t.Errorf("expected status limited, got %q", loaded.Accounts["work1"].Status)
	}

	// Verify no temp file left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// withQuotaLock tests
// ---------------------------------------------------------------------------

// TestWithQuotaLock_ExclusiveAccess verifies that two goroutines holding the
// lock are serialized: the second waits for the first to release, then sees
// the first's changes.
func TestWithQuotaLock_ExclusiveAccess(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, "quota.json")

	// Initialize with empty state.
	initialState := &config.QuotaState{Accounts: map[string]config.QuotaAccountState{}}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	started := make(chan struct{})
	done := make(chan struct{})

	// Goroutine 1: holds lock, writes work1=limited.
	go func() {
		defer close(done)
		err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
			close(started) // signal lock acquired
			time.Sleep(200 * time.Millisecond)
			if state.Accounts == nil {
				state.Accounts = make(map[string]config.QuotaAccountState)
			}
			state.Accounts["work1"] = config.QuotaAccountState{Status: config.QuotaStatusLimited}
			return nil
		})
		if err != nil {
			t.Errorf("goroutine 1 withQuotaLock: %v", err)
		}
	}()

	<-started // wait for goroutine 1 to hold lock

	// Goroutine 2 (main): should wait, then see goroutine 1's changes.
	err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
		// Should see work1=limited written by goroutine 1.
		if state.Accounts == nil {
			t.Error("expected non-nil Accounts map")
			return nil
		}
		if state.Accounts["work1"].Status != config.QuotaStatusLimited {
			t.Errorf("expected work1 status 'limited', got %q", state.Accounts["work1"].Status)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("goroutine 2 withQuotaLock: %v", err)
	}
	<-done
}

// TestWithQuotaLock_Timeout verifies that when the flock is held by another
// goroutine and the timeout is exceeded, the function returns a busy error
// matching the PRD message.
func TestWithQuotaLock_Timeout(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, "quota.json")

	// Initialize with empty state.
	initialState := &config.QuotaState{Accounts: map[string]config.QuotaAccountState{}}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	// Goroutine 1: holds lock until told to release.
	go func() {
		defer close(done)
		err := withQuotaLock(quotaPath, 5*time.Second, func(_ *config.QuotaState) error {
			close(started)
			<-release // hold lock until released
			return nil
		})
		if err != nil {
			// Error expected if lock is force-released; ignore.
			_ = err
		}
	}()

	<-started // wait for goroutine 1 to hold lock

	// Goroutine 2 (main): try to acquire with very short timeout → should fail.
	err := withQuotaLock(quotaPath, 100*time.Millisecond, func(_ *config.QuotaState) error {
		t.Error("callback should not be called when lock times out")
		return nil
	})

	close(release) // let goroutine 1 finish
	<-done         // wait for goroutine 1 to fully release the lock file

	if err == nil {
		t.Fatal("expected error for lock timeout")
	}
	errMsg := err.Error()
	if !strings.Contains(errMsg, "locked") || !strings.Contains(errMsg, "another rotation") {
		t.Errorf("expected busy error matching PRD message, got: %s", errMsg)
	}
}

// TestWithQuotaLock_TimeoutMessage verifies the exact PRD error message text
// for the timeout/busy case. PRD §Scenario #42 specifies stderr should contain:
// "error: quota state is locked by another rotation in progress. Try again in a moment."
func TestWithQuotaLock_TimeoutMessage(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, "quota.json")

	initialState := &config.QuotaState{Accounts: map[string]config.QuotaAccountState{}}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = withQuotaLock(quotaPath, 5*time.Second, func(_ *config.QuotaState) error {
			close(started)
			<-release
			return nil
		})
	}()

	<-started

	err := withQuotaLock(quotaPath, 100*time.Millisecond, func(_ *config.QuotaState) error {
		return nil
	})

	close(release)
	<-done // wait for goroutine to fully release the lock and finish file I/O

	if err == nil {
		t.Fatal("expected timeout error")
	}
	// PRD exact message must include "error:" prefix per PRD §Scenario #42.
	const wantMsg = "error: quota state is locked by another rotation in progress. Try again in a moment."
	if err.Error() != wantMsg {
		t.Errorf("lock-timeout error should match PRD exactly:\n  want: %s\n  got:  %s", wantMsg, err.Error())
	}
}

// TestWithQuotaLock_ReadAfterLock verifies that the quota state is read AFTER
// the flock is acquired (TOCTOU prevention). We do this by modifying the file
// between the function call and lock acquisition: write initial state, start
// goroutine 1 that holds the lock and modifies the file, then goroutine 2
// acquires the lock and should see goroutine 1's modifications.
func TestWithQuotaLock_ReadAfterLock(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, "quota.json")

	// Initialize with work1=available.
	initialState := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable},
		},
	}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	started := make(chan struct{})
	done := make(chan struct{})

	// Goroutine 1: acquire lock, modify work1 to limited, release.
	go func() {
		defer close(done)
		err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
			close(started)
			time.Sleep(100 * time.Millisecond) // hold lock briefly
			state.Accounts["work1"] = config.QuotaAccountState{Status: config.QuotaStatusLimited}
			return nil
		})
		if err != nil {
			t.Errorf("goroutine 1: %v", err)
		}
	}()

	<-started // goroutine 1 holds lock

	// Goroutine 2 (main): waits for lock, then reads state AFTER lock.
	// If read happens before lock, it would see "available" (stale).
	// If read happens after lock (correct), it sees "limited".
	err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
		if state.Accounts["work1"].Status != config.QuotaStatusLimited {
			t.Errorf("expected work1='limited' (read-after-lock), got %q — TOCTOU vulnerability", state.Accounts["work1"].Status)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("goroutine 2: %v", err)
	}

	<-done
}

// TestWithQuotaLock_CrashRecovery verifies that if the callback returns an
// error (simulating a crash), the flock is released and the next invocation
// reads the last successfully saved state.
func TestWithQuotaLock_CrashRecovery(t *testing.T) {
	dir := t.TempDir()
	quotaPath := filepath.Join(dir, "quota.json")

	// Initialize with work1=available.
	initialState := &config.QuotaState{
		Accounts: map[string]config.QuotaAccountState{
			"work1": {Status: config.QuotaStatusAvailable},
		},
	}
	if err := saveQuotaState(quotaPath, initialState); err != nil {
		t.Fatalf("saveQuotaState: %v", err)
	}

	// First call: callback returns error → changes should NOT be persisted.
	err := withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
		state.Accounts["work1"] = config.QuotaAccountState{Status: config.QuotaStatusLimited}
		return os.ErrClosed // simulate crash/error
	})
	if err == nil {
		t.Fatal("expected error from callback")
	}

	// Second call: should read the last successfully saved state (work1=available),
	// proving the flock was released and no corrupt state was persisted.
	var mu sync.Mutex
	var readStatus config.QuotaAccountStatus
	err = withQuotaLock(quotaPath, 5*time.Second, func(state *config.QuotaState) error {
		mu.Lock()
		defer mu.Unlock()
		readStatus = state.Accounts["work1"].Status
		return nil
	})
	if err != nil {
		t.Fatalf("second withQuotaLock: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if readStatus != config.QuotaStatusAvailable {
		t.Errorf("expected work1='available' after crash recovery, got %q", readStatus)
	}
}
