package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// writeFenceStatusCityTOML gives a retiredNudgeTestCity dir the minimal
// city.toml cmdNudgeStatus's resolution needs, with the fake session provider
// so nothing resolves a runtime.
func writeFenceStatusCityTOML(t *testing.T, dir string) {
	t.Helper()
	toml := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n"
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}

// TestNudgeStatusListsItemsFencedToTheSessionBeyondTheirQueueKey covers AC-7
// of gastownhall/gascity#4534: `gc nudge status <session>` answers by fence as
// well as by key, so an item fenced to the session whose queue key the
// session no longer serves (a moved identity, a renamed alias) still shows up
// — as pending while the session lives, and as a dead letter with its reason
// once the session terminated — instead of the 0/0/0 the key-only read
// reported.
func TestNudgeStatusListsItemsFencedToTheSessionBeyondTheirQueueKey(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	dir, _, _, info := retiredNudgeTestCity(t, nil)
	writeFenceStatusCityTOML(t, dir)
	t.Setenv("GC_CITY", dir)

	// A pending item fenced to the session but keyed to an identity it does
	// not serve: the key-only read lists nothing for this session.
	item := newQueuedNudgeWithOptions("reviewer", "status by fence", "human", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	var jsonOut, jsonErr bytes.Buffer
	if code := cmdNudgeStatus([]string{info.ID}, true, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("cmdNudgeStatus --json = %d, want 0; stderr=%s", code, jsonErr.String())
	}
	var result nudgeStatusJSON
	if err := json.Unmarshal(jsonOut.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, jsonOut.String())
	}
	if result.Counts.Pending != 1 || len(result.Pending) != 1 || result.Pending[0].ID != item.ID {
		t.Fatalf("pending = %d items %v, want exactly the fence-matched %q (counts %+v)", result.Counts.Pending, result.Pending, item.ID, result.Counts)
	}

	var textOut, textErr bytes.Buffer
	if code := cmdNudgeStatus([]string{info.ID}, false, &textOut, &textErr); code != 0 {
		t.Fatalf("cmdNudgeStatus text = %d, want 0; stderr=%s", code, textErr.String())
	}
	if !strings.Contains(textOut.String(), item.ID) {
		t.Fatalf("text stdout = %q, want it to list the fence-matched item %q", textOut.String(), item.ID)
	}
}

// TestNudgeStatusListsTheDeadLetterForATerminatedSessionByKeyMismatch covers
// AC-7's terminal arm: once the session terminated and the retired-fence
// sweep dead-lettered its orphan, the status read — the same command's
// maintaining list triggers the sweep — reports the dead letter with its
// reason for a key that no longer matches, where the key-only read reported
// nothing at all.
func TestNudgeStatusListsTheDeadLetterForATerminatedSessionByKeyMismatch(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	dir, mgr, _, info := retiredNudgeTestCity(t, nil)
	writeFenceStatusCityTOML(t, dir)
	t.Setenv("GC_CITY", dir)
	if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	item := newQueuedNudgeWithOptions("reviewer", "status by fence", "human", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: info.ID})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	var jsonOut, jsonErr bytes.Buffer
	if code := cmdNudgeStatus([]string{info.ID}, true, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("cmdNudgeStatus --json = %d, want 0; stderr=%s", code, jsonErr.String())
	}
	var result nudgeStatusJSON
	if err := json.Unmarshal(jsonOut.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, jsonOut.String())
	}
	if result.Counts.Dead != 1 || len(result.Dead) != 1 || result.Dead[0].ID != item.ID {
		t.Fatalf("dead = %d items %v, want exactly the fence-matched %q (counts %+v)", result.Counts.Dead, result.Dead, item.ID, result.Counts)
	}
	if result.Dead[0].LastError != "session-terminated" {
		t.Fatalf("dead item last_error = %q, want session-terminated", result.Dead[0].LastError)
	}

	var textOut, textErr bytes.Buffer
	if code := cmdNudgeStatus([]string{info.ID}, false, &textOut, &textErr); code != 0 {
		t.Fatalf("cmdNudgeStatus text = %d, want 0; stderr=%s", code, textErr.String())
	}
	if !strings.Contains(textOut.String(), "reason=session-terminated") {
		t.Fatalf("text stdout = %q, want the dead letter's reason", textOut.String())
	}
}
