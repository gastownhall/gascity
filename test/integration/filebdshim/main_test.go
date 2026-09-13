package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestRunFileStoreReadyExcludesSessionBeads(t *testing.T) {
	cityDir := newShimTestCity(t)
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}
	defer recorder.Close() //nolint:errcheck

	if _, err := store.Create(beads.Bead{Title: "task", Type: "task"}); err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "session", Type: "session"}); err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stdout bytes.Buffer
	code, handled, err := runFileStore(cityDir, []string{"ready", "--json"}, &stdout)
	if err != nil {
		t.Fatalf("runFileStore(ready): %v", err)
	}
	if !handled || code != 0 {
		t.Fatalf("runFileStore handled=%v code=%d, want handled=true code=0", handled, code)
	}

	var items []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput=%s", err, stdout.String())
	}
	if len(items) != 1 {
		t.Fatalf("ready returned %d items, want 1\noutput=%s", len(items), stdout.String())
	}
	if got := items[0]["title"]; got != "task" {
		t.Fatalf("ready title = %v, want task", got)
	}
}

func TestRunFileStoreReadyRespectsAssigneeFilter(t *testing.T) {
	cityDir := newShimTestCity(t)
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatalf("openFileStore: %v", err)
	}
	defer recorder.Close() //nolint:errcheck

	task, err := store.Create(beads.Bead{Title: "claimed-task", Type: "task"})
	if err != nil {
		t.Fatalf("Create(task): %v", err)
	}
	if err := store.Update(task.ID, beads.UpdateOpts{Assignee: stringPtr("worker")}); err != nil {
		t.Fatalf("Update(task assignee): %v", err)
	}
	session, err := store.Create(beads.Bead{Title: "worker-session", Type: "session"})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	if err := store.Update(session.ID, beads.UpdateOpts{Assignee: stringPtr("worker")}); err != nil {
		t.Fatalf("Update(session assignee): %v", err)
	}

	var stdout bytes.Buffer
	code, handled, err := runFileStore(cityDir, []string{"ready", "--assignee=worker", "--json"}, &stdout)
	if err != nil {
		t.Fatalf("runFileStore(ready --assignee): %v", err)
	}
	if !handled || code != 0 {
		t.Fatalf("runFileStore handled=%v code=%d, want handled=true code=0", handled, code)
	}

	var items []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput=%s", err, stdout.String())
	}
	if len(items) != 1 {
		t.Fatalf("ready returned %d items, want 1\noutput=%s", len(items), stdout.String())
	}
	if got := items[0]["title"]; got != "claimed-task" {
		t.Fatalf("ready title = %v, want claimed-task", got)
	}
}

func newShimTestCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatalf("openFileStore(init): %v", err)
	}
	defer recorder.Close() //nolint:errcheck
	if _, err := store.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List(init): %v", err)
	}
	return cityDir
}

func stringPtr(s string) *string {
	return &s
}
