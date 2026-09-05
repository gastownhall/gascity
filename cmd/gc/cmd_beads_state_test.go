package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// beadsStateTestCity creates a minimal city with file beads provider and returns
// the city path.
func beadsStateTestCity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	return dir
}

// beadsStateTestCityWithBeads creates a city with representative beads
// that have known deterministic states (no dependency on ready/blocked sets).
func beadsStateTestCityWithBeads(t *testing.T) string {
	t.Helper()
	dir := beadsStateTestCity(t)

	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	// done: closed bead — Create then Update because the file store defaults all
	// new beads to status=open regardless of the Status field passed to Create.
	closed, err := store.Create(beads.Bead{Title: "closed task", Type: "task"})
	if err != nil {
		t.Fatalf("create closed bead: %v", err)
	}
	if err := store.Update(closed.ID, beads.UpdateOpts{Status: strPtr("closed")}); err != nil {
		t.Fatalf("close bead: %v", err)
	}

	// in-progress: status=in_progress, no session name (status-based path)
	inprog, err := store.Create(beads.Bead{Title: "running task", Type: "task"})
	if err != nil {
		t.Fatalf("create in_progress bead: %v", err)
	}
	if err := store.Update(inprog.ID, beads.UpdateOpts{Status: strPtr("in_progress")}); err != nil {
		t.Fatalf("set in_progress status: %v", err)
	}

	// waiting-human: open task with "human" label
	if _, err := store.Create(beads.Bead{Title: "human task", Type: "task", Labels: []string{"human"}}); err != nil {
		t.Fatalf("create human-labeled bead: %v", err)
	}

	return dir
}

func TestBeadsState_JSONShape(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	code := cmdBeadsState("", "", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdBeadsState --json = %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}

	var result struct {
		SchemaVersion string `json:"schema_version"`
		States        map[string]struct {
			Owner string   `json:"owner"`
			Count int      `json:"count"`
			IDs   []string `json:"ids"`
		} `json:"states"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("JSON parse failed: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion == "" {
		t.Error("schema_version missing from JSON output")
	}

	// done state must appear: we created one closed bead
	done, ok := result.States["done"]
	if !ok {
		t.Fatalf("'done' state missing from JSON; states present: %v", stateKeys(result.States))
	}
	if done.Count < 1 {
		t.Errorf("done.count = %d, want >= 1", done.Count)
	}
	if done.Owner == "" {
		t.Error("done.owner is empty")
	}

	// in-progress state must appear: we created one in_progress bead
	if _, ok := result.States["in-progress"]; !ok {
		t.Errorf("'in-progress' state missing; states: %v", stateKeys(result.States))
	}

	// waiting-human must appear: we created a human-labeled bead
	if _, ok := result.States["waiting-human"]; !ok {
		t.Errorf("'waiting-human' state missing; states: %v", stateKeys(result.States))
	}
}

func TestBeadsState_StateFilter(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	// filter to "done" only
	code := cmdBeadsState("", "done", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdBeadsState --state done --json = %d; stderr=%s", code, stderr.String())
	}

	var result struct {
		States map[string]json.RawMessage `json:"states"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("JSON parse failed: %v\nraw: %s", err, stdout.String())
	}
	for k := range result.States {
		if k != "done" {
			t.Errorf("--state done: unexpected state %q in output", k)
		}
	}
	if _, ok := result.States["done"]; !ok {
		t.Error("--state done: 'done' missing from filtered output")
	}
}

func TestBeadsState_IDsFlag(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	// --ids on table output (non-JSON)
	code := cmdBeadsState("", "done", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdBeadsState --state done --ids = %d; stderr=%s", code, stderr.String())
	}
	// File-store bead IDs start with "gc-"
	if !strings.Contains(stdout.String(), "gc-") {
		t.Errorf("--ids: expected bead ID in output, got:\n%s", stdout.String())
	}
}

func TestBeadsState_TableOutput(t *testing.T) {
	dir := beadsStateTestCityWithBeads(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	code := cmdBeadsState("", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdBeadsState = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Table must have at least one state row
	if !strings.Contains(out, "done") && !strings.Contains(out, "in-progress") {
		t.Errorf("table missing expected states; output:\n%s", out)
	}
}

func TestBeadsState_UnknownStateFilter(t *testing.T) {
	dir := beadsStateTestCity(t)
	cityFlag = dir
	defer func() { cityFlag = "" }()

	var stdout, stderr bytes.Buffer
	// filtering to a state with zero beads should still exit 0
	code := cmdBeadsState("", "orphaned", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdBeadsState --state orphaned = %d; stderr=%s", code, stderr.String())
	}
}

// stateKeys returns the sorted key list for error messages.
func stateKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
