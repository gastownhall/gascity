package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
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

func TestRunFileStoreSupportsHookClaimReadProtocol(t *testing.T) {
	cityDir := newShimTestCity(t)
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close() //nolint:errcheck
	row, err := store.Create(beads.Bead{Title: "person decision", Type: "task", Labels: []string{"human"}, Metadata: map[string]string{
		"gc.root_bead_id": "root-1", "gc.continuation_group": "body",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "other group", Type: "task", Metadata: map[string]string{
		"gc.root_bead_id": "root-1", "gc.continuation_group": "another-group",
	}}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"show", "--json", row.ID},
		{
			"list", "--json", "--status=open", "--include-infra", "--include-gates", "--include-templates", "--limit", "0",
			"--metadata-field", "gc.root_bead_id=root-1", "--metadata-field", "gc.continuation_group=body",
		},
	} {
		t.Run(args[0], func(t *testing.T) {
			var stdout bytes.Buffer
			code, handled, err := runFileStore(cityDir, args, &stdout)
			if err != nil || !handled || code != 0 {
				t.Fatalf("hook read: handled=%v code=%d err=%v", handled, code, err)
			}
			var got []struct {
				ID       string            `json:"id"`
				Labels   []string          `json:"labels"`
				Metadata map[string]string `json:"metadata"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("hook read must return an array: %v\n%s", err, stdout.String())
			}
			if len(got) != 1 || got[0].ID != row.ID || !reflect.DeepEqual(got[0].Labels, row.Labels) || !maps.Equal(got[0].Metadata, row.Metadata) {
				t.Errorf("hook read lost label/group identity or scope: %+v, want %s with %v/%v", got, row.ID, row.Labels, row.Metadata)
			}
		})
	}
}

func TestRunFileStoreQuerySelectsEphemeralContinuationRows(t *testing.T) {
	cityDir := newShimTestCity(t)
	store, recorder, err := openFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	defer recorder.Close() //nolint:errcheck
	for _, row := range []beads.Bead{
		{Title: "permanent", Type: "task", Status: "open"},
		{Title: "ephemeral-open", Type: "task", Status: "open", Ephemeral: true, Labels: []string{"human"}},
		{Title: "ephemeral-closed", Type: "task", Status: "closed", Ephemeral: true},
	} {
		created, err := store.Create(row)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status == "closed" {
			if err := store.Close(created.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stdout bytes.Buffer
	code, handled, err := runFileStore(cityDir, []string{"query", "--json", "ephemeral=true AND status=open", "--limit", "0"}, &stdout)
	if err != nil || !handled || code != 0 {
		t.Fatalf("ephemeral hook read: handled=%v code=%d err=%v", handled, code, err)
	}
	var got []struct {
		Title     string   `json:"title"`
		Ephemeral bool     `json:"ephemeral"`
		Labels    []string `json:"labels"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "ephemeral-open" || !got[0].Ephemeral || !reflect.DeepEqual(got[0].Labels, []string{"human"}) {
		t.Errorf("ephemeral read = %+v, want only open ephemeral row with its human label", got)
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
