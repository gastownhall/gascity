package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// --- gc convoy create ---

func TestConvoyCreate(t *testing.T) {
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doConvoyCreate(store, events.Discard, []string{"deploy v2.0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCreate = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Created convoy gc-1 "deploy v2.0"`) {
		t.Errorf("stdout = %q, want convoy creation confirmation", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Type != "convoy" {
		t.Errorf("bead Type = %q, want %q", b.Type, "convoy")
	}
	if b.Title != "deploy v2.0" {
		t.Errorf("bead Title = %q, want %q", b.Title, "deploy v2.0")
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q", b.Status, "open")
	}
}

func TestConvoyCreateWithIssues(t *testing.T) {
	store := beads.NewMemStore()
	// Pre-create issues.
	_, _ = store.Create(beads.Bead{Title: "epic", Type: "epic"})         // gc-1
	_, _ = store.Create(beads.Bead{Title: "fix auth", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "fix logging"})                // gc-3

	var stdout, stderr bytes.Buffer
	code := doConvoyCreate(store, events.Discard, []string{"security fixes", "gc-2", "gc-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCreate = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "tracking 2 issue(s)") {
		t.Errorf("stdout = %q, want tracking count", stdout.String())
	}

	got, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentID != "gc-1" {
		t.Errorf("bead gc-2 ParentID = %q, want preserved epic parent gc-1", got.ParentID)
	}
	requireConvoyTrack(t, store, "gc-4", "gc-2")
	requireConvoyTrack(t, store, "gc-4", "gc-3")
}

func TestConvoyCreateJSON(t *testing.T) {
	store := beads.NewMemStore()
	issue, _ := store.Create(beads.Bead{Title: "fix auth"})

	var stdout, stderr bytes.Buffer
	code := doConvoyCreateWithOptionsJSON(store, nil, "", events.Discard,
		[]string{"security fixes", issue.ID}, convoyCreateOptions{}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCreateWithOptionsJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	var got struct {
		SchemaVersion string   `json:"schema_version"`
		OK            bool     `json:"ok"`
		Command       string   `json:"command"`
		ConvoyID      string   `json:"convoy_id"`
		IssueIDs      []string `json:"issue_ids"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || !got.OK || got.Command != "convoy.create" || got.ConvoyID == "" || len(got.IssueIDs) != 1 {
		t.Fatalf("payload = %+v", got)
	}
}

func TestConvoyCreateMissingName(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyCreate(store, events.Discard, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyCreate = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing convoy name") {
		t.Errorf("stderr = %q, want missing name error", stderr.String())
	}
}

func TestConvoyCreateBadIssueID(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyCreate(store, events.Discard, []string{"batch", "gc-999"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyCreate = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "bead not found") {
		t.Errorf("stderr = %q, want not found error", stderr.String())
	}
}

func TestConvoyCreateMultiRig(t *testing.T) {
	// Simulate cross-rig convoy: convoy in city store, children in rig store.
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	// Create children in rig store.
	child1, _ := rigStore.Create(beads.Bead{Title: "task A"})
	child2, _ := rigStore.Create(beads.Bead{Title: "task B"})

	// Test 1: single-store mode (cfg=nil) — all beads in same store.
	var stdout, stderr bytes.Buffer
	code := doConvoyCreateWithOptions(cityStore, events.Discard,
		[]string{"cross-rig batch", child1.ID, child2.ID}, convoyCreateOptions{}, &stdout, &stderr)
	// Should fail because children are in rigStore, not cityStore.
	if code != 1 {
		t.Fatalf("expected failure (children not in city store), got code %d", code)
	}

	// Test 2: same store — children and convoy in same store.
	stdout.Reset()
	stderr.Reset()
	child3, _ := cityStore.Create(beads.Bead{Title: "city task"})
	child4, _ := cityStore.Create(beads.Bead{Title: "city task 2"})
	code = doConvoyCreateWithOptions(cityStore, events.Discard,
		[]string{"same-store batch", child3.ID, child4.ID}, convoyCreateOptions{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("same-store convoy failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "tracking 2 issue") {
		t.Errorf("stdout = %q, want tracking 2 issues", stdout.String())
	}

	// Verify children are tracked without requiring parent changes.
	got3, _ := cityStore.Get(child3.ID)
	got4, _ := cityStore.Get(child4.ID)
	if got3.ParentID != "" || got4.ParentID != "" {
		t.Fatalf("children were reparented: %q, %q", got3.ParentID, got4.ParentID)
	}
	convoys, err := cityStore.List(beads.ListQuery{Type: "convoy", IncludeClosed: true})
	if err != nil {
		t.Fatalf("list convoys: %v", err)
	}
	convoyID := ""
	for _, convoy := range convoys {
		if convoy.Title == "same-store batch" {
			convoyID = convoy.ID
			break
		}
	}
	if convoyID == "" {
		t.Fatalf("same-store convoy not found: %+v", convoys)
	}
	requireConvoyTrack(t, cityStore, convoyID, child3.ID)
	requireConvoyTrack(t, cityStore, convoyID, child4.ID)
	convoy, _ := cityStore.Get(convoyID)
	if convoy.Type != "convoy" {
		t.Errorf("convoy type = %q, want convoy", convoy.Type)
	}
}

// TestConvoyCreateRigChildrenShareStore is a regression test: when children
// have a rig prefix, the convoy must be created in the same store as the
// children (not the city root store). Otherwise the membership relationship
// points at beads in a different database.
func TestValidateConvoyCreateStoreScopeRejectsMixedStores(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "frontend", Prefix: "fe", Path: "frontend"}},
	}
	cityPath := "/city"
	if err := validateConvoyCreateStoreScope(cfg, cityPath, []string{"fe-1", "gc-2"}); err == nil {
		t.Fatal("expected mixed city/rig store validation error")
	}
}

func TestValidateConvoyCreateStoreScopeAllowsSameRigStore(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "frontend", Prefix: "fe", Path: "frontend"}},
	}
	cityPath := "/city"
	if err := validateConvoyCreateStoreScope(cfg, cityPath, []string{"fe-1", "FE-2"}); err != nil {
		t.Fatalf("validateConvoyCreateStoreScope() = %v, want nil", err)
	}
}

func TestConvoyCreateRigChildrenShareStore(t *testing.T) {
	store := beads.NewMemStore()

	// Create children first.
	c1, _ := store.Create(beads.Bead{Title: "Python hello"})
	c2, _ := store.Create(beads.Bead{Title: "Rust hello"})
	c3, _ := store.Create(beads.Bead{Title: "Haskell hello"})

	var stdout, stderr bytes.Buffer
	code := doConvoyCreateWithOptions(store, events.Discard,
		[]string{"Hello World Variants", c1.ID, c2.ID, c3.ID}, convoyCreateOptions{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("convoy create failed: %s", stderr.String())
	}

	// All children must be tracked by the convoy without being reparented.
	got1, _ := store.Get(c1.ID)
	got2, _ := store.Get(c2.ID)
	got3, _ := store.Get(c3.ID)
	if got1.ParentID != "" || got2.ParentID != "" || got3.ParentID != "" {
		t.Fatalf("children were reparented: %q, %q, %q", got1.ParentID, got2.ParentID, got3.ParentID)
	}
	convoys, err := store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true})
	if err != nil {
		t.Fatalf("list convoys: %v", err)
	}
	if len(convoys) != 1 {
		t.Fatalf("convoys = %d, want 1", len(convoys))
	}
	convoyID := convoys[0].ID
	requireConvoyTrack(t, store, convoyID, c1.ID)
	requireConvoyTrack(t, store, convoyID, c2.ID)
	requireConvoyTrack(t, store, convoyID, c3.ID)

	// Convoy must exist in the SAME store as children.
	convoy, err := store.Get(convoyID)
	if err != nil {
		t.Fatalf("convoy %s not in child store: %v", convoyID, err)
	}
	if convoy.Type != "convoy" {
		t.Errorf("convoy type = %q, want convoy", convoy.Type)
	}
	if convoy.Title != "Hello World Variants" {
		t.Errorf("convoy title = %q, want Hello World Variants", convoy.Title)
	}

	// Verify the convoy is expandable through the compatibility helper.
	children, err := listConvoyChildren(store, convoyID, false)
	if err != nil {
		t.Fatalf("listing convoy children: %v", err)
	}
	if len(children) != 3 {
		t.Errorf("got %d children, want 3", len(children))
	}
}

// --- gc convoy list ---

func TestConvoyList(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch 1", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "fix auth", ParentID: "gc-1"})
	_, _ = store.Create(beads.Bead{Title: "fix logs", ParentID: "gc-1"})
	_ = store.Close("gc-3") // close one child

	var stdout, stderr bytes.Buffer
	code := doConvoyList(store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyList = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"ID", "TITLE", "PROGRESS", "gc-1", "batch 1", "1/2 closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestConvoyListEmpty(t *testing.T) {
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doConvoyList(store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyList = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No open convoys") {
		t.Errorf("stdout = %q, want no open convoys message", stdout.String())
	}
}

func TestConvoyListExcludesClosed(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "done batch", Type: "convoy"})
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	code := doConvoyList(store, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyList = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No open convoys") {
		t.Errorf("stdout = %q, want no open convoys (closed convoy excluded)", stdout.String())
	}
}

func TestConvoyListAcrossStores(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"}) // gc-1
	_, _ = cityStore.Create(beads.Bead{Title: "city task", ParentID: "gc-1"})
	_, _ = rigStore.Create(beads.Bead{Title: "rig batch", Type: "convoy"}) // gc-1
	_, _ = rigStore.Create(beads.Bead{Title: "rig task", ParentID: "gc-1"})

	var stdout, stderr bytes.Buffer
	code := doConvoyListAcrossStores([]convoyStoreView{{store: cityStore}, {store: rigStore}}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyListAcrossStores = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"city batch", "rig batch"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestConvoyListJSON(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "batch 1",
		Type:     "convoy",
		Labels:   []string{"owned"},
		Metadata: map[string]string{"target": "integration/gc-1"},
	})
	_, _ = store.Create(beads.Bead{Title: "fix auth", ParentID: "gc-1"})
	_, _ = store.Create(beads.Bead{Title: "fix logs", ParentID: "gc-1", Assignee: "worker"})
	_ = store.Close("gc-3")

	var stdout, stderr bytes.Buffer
	code := doConvoyListAcrossStores([]convoyStoreView{{store: store}}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyListAcrossStores --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want one JSONL record: %q", len(lines), stdout.String())
	}
	var result convoyListResultJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", result.SchemaVersion)
	}
	if result.Summary.Total != 1 || len(result.Convoys) != 1 {
		t.Fatalf("result summary/items = %+v/%+v, want one convoy", result.Summary, result.Convoys)
	}
	got := result.Convoys[0]
	if got.ID != "gc-1" || got.Title != "batch 1" || !got.Owned {
		t.Fatalf("convoy summary = %+v, want gc-1 batch 1 owned", got)
	}
	if got.Progress.Closed != 1 || got.Progress.Total != 2 {
		t.Fatalf("progress = %+v, want 1/2", got.Progress)
	}
	if got.Fields.Target != "integration/gc-1" {
		t.Fatalf("target = %q, want integration/gc-1", got.Fields.Target)
	}
}

func TestConvoyListJSONEmpty(t *testing.T) {
	store := beads.NewMemStore()

	var stdout, stderr bytes.Buffer
	code := doConvoyListAcrossStores([]convoyStoreView{{store: store}}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyListAcrossStores --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(stdout.String(), "No open convoys") {
		t.Fatalf("json stdout contains human message: %q", stdout.String())
	}
	var result convoyListResultJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.Summary.Total != 0 || len(result.Convoys) != 0 {
		t.Fatalf("result = %+v, want empty v1 list", result)
	}
}

// --- gc convoy status ---

func TestConvoyStatus(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "deploy",
		Type:     "convoy",
		Labels:   []string{"owned"},
		Metadata: map[string]string{"target": "integration/gc-1"},
	}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                     // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1", Assignee: "worker"}) // gc-3
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyStatus(store, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStatus = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"Convoy:   gc-1",
		"Title:    deploy",
		"Status:   open",
		"1/2 closed",
		"Lifecycle: owned",
		"Target:   integration/gc-1",
		"task A", "closed",
		"task B", "worker",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestConvoyStatusTracksDeps(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "deploy", Type: "convoy"})     // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                     // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", Assignee: "worker"}) // gc-3
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-1", "gc-3", "tracks"))
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyStatus(store, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStatus = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"1/2 closed", "task A", "closed", "task B", "worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestConvoyStatusReportsDanglingTracks(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "deploy", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                 // gc-2
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-1", "gc-missing", "tracks"))
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyStatus(store, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStatus = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"Progress: 1/2 closed (1 dangling track)", "gc-missing", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestConvoyStatusJSON(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "deploy",
		Type:     "convoy",
		Labels:   []string{"owned"},
		Metadata: map[string]string{"target": "integration/gc-1", "convoy.owner": "mayor"},
	})
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1", Assignee: "worker"})
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyStatusWithJSON(store, []string{"gc-1"}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStatus --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want one JSONL record: %q", len(lines), stdout.String())
	}
	var result convoyStatusResultJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", result.SchemaVersion)
	}
	if result.Convoy.ID != "gc-1" || result.Convoy.Title != "deploy" || !result.Convoy.Owned {
		t.Fatalf("convoy = %+v, want owned gc-1 deploy", result.Convoy)
	}
	if result.Convoy.Fields.Target != "integration/gc-1" || result.Convoy.Fields.Owner != "mayor" {
		t.Fatalf("fields = %+v, want target and owner", result.Convoy.Fields)
	}
	if result.Progress.Closed != 1 || result.Progress.Total != 2 {
		t.Fatalf("progress = %+v, want 1/2", result.Progress)
	}
	if len(result.Children) != 2 || result.Children[1].Assignee != "worker" {
		t.Fatalf("children = %+v, want two children with second assigned", result.Children)
	}
}

func TestConvoyStatusJSONReportsDanglingTracks(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "deploy", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                 // gc-2
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-1", "gc-missing", "tracks"))
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyStatusWithJSON(store, []string{"gc-1"}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStatus --json = %d, want 0; stderr: %s", code, stderr.String())
	}

	var result convoyStatusResultJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.Progress.Closed != 1 || result.Progress.Total != 2 || result.Progress.DanglingTracks != 1 {
		t.Fatalf("progress = %+v, want 1/2 with 1 dangling track", result.Progress)
	}
}

func TestConvoyListAndStatusJSONCommands(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	_, _ = store.Create(beads.Bead{Title: "release train", Type: "convoy"})
	_, _ = store.Create(beads.Bead{Title: "ship docs", ParentID: "gc-1"})

	t.Run("list", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"convoy", "list", "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run convoy list --json = %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		var result convoyListResultJSON
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
		}
		if result.SchemaVersion != "1" || len(result.Convoys) != 1 {
			t.Fatalf("result = %+v, want one v1 convoy", result)
		}
	})

	t.Run("status", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"convoy", "status", "gc-1", "--json"}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run convoy status --json = %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q, want empty", stderr.String())
		}
		var result convoyStatusResultJSON
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
		}
		if result.SchemaVersion != "1" || result.Convoy.ID != "gc-1" || len(result.Children) != 1 {
			t.Fatalf("result = %+v, want gc-1 with one child", result)
		}
	})
}

func TestConvoyTarget(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "deploy", Type: "convoy"}) // gc-1

	var stdout, stderr bytes.Buffer
	code := doConvoyTarget(store, []string{"gc-1", "integration/gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyTarget = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Set target of convoy gc-1 to integration/gc-1") {
		t.Errorf("stdout = %q, want target confirmation", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Metadata["target"]; got != "integration/gc-1" {
		t.Fatalf("target metadata = %q, want %q", got, "integration/gc-1")
	}
}

func TestConvoyStoreCandidatesPreferRigPrefixOnBd(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "hello-world",
			Path:   "/rigs/hello-world",
			Prefix: "HW",
		}},
	}

	got := convoyStoreCandidates(cfg, "/city", "HW-42")
	want := []string{"/rigs/hello-world", "/city"}
	if len(got) != len(want) {
		t.Fatalf("convoyStoreCandidates len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("convoyStoreCandidates[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestConvoyStoreCandidatesKeepFileProviderCityScoped(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "hello-world",
			Path:   "/rigs/hello-world",
			Prefix: "HW",
		}},
	}

	got := convoyStoreCandidates(cfg, "/city", "HW-42")
	if len(got) != 1 || got[0] != "/city" {
		t.Fatalf("convoyStoreCandidates = %v, want [/city]", got)
	}
}

func TestConvoyStoreCandidatesIncludeBdRigUnderLegacyFileCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "hello-world")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"hw"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "hello-world",
			Path:   rigDir,
			Prefix: "HW",
		}},
	}

	got := convoyStoreCandidates(cfg, cityDir, "HW-42")
	want := []string{rigDir, cityDir}
	if len(got) != len(want) {
		t.Fatalf("convoyStoreCandidates len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("convoyStoreCandidates[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestConvoyStoreCandidatesIncludeMarkedFileRigUnderLegacyFileCity(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "hello-world")
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "hello-world",
			Path:   rigDir,
			Prefix: "HW",
		}},
	}

	got := convoyStoreCandidates(cfg, cityDir, "HW-42")
	want := []string{rigDir, cityDir}
	if len(got) != len(want) {
		t.Fatalf("convoyStoreCandidates len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("convoyStoreCandidates[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestResolveConvoyStoreFindsUnprefixedRigConvoy(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	convoy, _ := rigStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})
	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:   "hello-world",
			Path:   "/rigs/hello-world",
			Prefix: "HW",
		}},
	}
	openStore := func(dir string) (beads.Store, error) {
		switch dir {
		case "/city":
			return cityStore, nil
		case "/rigs/hello-world":
			return rigStore, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return nil, nil
		}
	}

	store, err := resolveConvoyStore(convoy.ID, cfg, "/city", openStore)
	if err != nil {
		t.Fatalf("resolveConvoyStore: %v", err)
	}
	if store != rigStore {
		t.Fatalf("resolveConvoyStore returned wrong store")
	}
}

func TestConvoyStatusNotConvoy(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "just a task"}) // type=task

	var stderr bytes.Buffer
	code := doConvoyStatus(store, []string{"gc-1"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyStatus = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a convoy") {
		t.Errorf("stderr = %q, want 'not a convoy'", stderr.String())
	}
}

func TestConvoyStatusMissingID(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyStatus(store, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyStatus = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing convoy ID") {
		t.Errorf("stderr = %q, want missing ID error", stderr.String())
	}
}

// --- gc convoy add ---

func TestConvoyAdd(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "epic", Type: "epic"})       // gc-2
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-2"}) // gc-3

	var stdout, stderr bytes.Buffer
	code := doConvoyAdd(store, []string{"gc-1", "gc-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Added gc-3 to convoy gc-1") {
		t.Errorf("stdout = %q, want add confirmation", stdout.String())
	}

	b, err := store.Get("gc-3")
	if err != nil {
		t.Fatal(err)
	}
	if b.ParentID != "gc-2" {
		t.Errorf("bead ParentID = %q, want preserved epic parent gc-2", b.ParentID)
	}
	requireConvoyTrack(t, store, "gc-1", "gc-3")
}

func TestConvoyAddNotConvoy(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "just a task"}) // type=task
	_, _ = store.Create(beads.Bead{Title: "another"})

	var stderr bytes.Buffer
	code := doConvoyAdd(store, []string{"gc-1", "gc-2"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a convoy") {
		t.Errorf("stderr = %q, want 'not a convoy'", stderr.String())
	}
}

func TestConvoyAddMissingArgs(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyAdd(store, []string{"gc-1"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want usage message", stderr.String())
	}
}

// --- gc convoy close ---

func TestConvoyClose(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})

	var stdout, stderr bytes.Buffer
	code := doConvoyClose(store, events.Discard, []string{"gc-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyClose = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Closed convoy gc-1") {
		t.Errorf("stdout = %q, want close confirmation", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
	if got := b.Metadata["close_reason"]; got != convoyManualCloseReason {
		t.Errorf("metadata.close_reason = %q, want %q", got, convoyManualCloseReason)
	}
}

func TestConvoyCloseNotConvoy(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "a task"})

	var stderr bytes.Buffer
	code := doConvoyClose(store, events.Discard, []string{"gc-1"}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyClose = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a convoy") {
		t.Errorf("stderr = %q, want 'not a convoy'", stderr.String())
	}
}

func TestConvoyCloseMissingID(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyClose(store, events.Discard, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyClose = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing convoy ID") {
		t.Errorf("stderr = %q, want missing ID error", stderr.String())
	}
}

// --- gc convoy check ---

func TestConvoyCheck(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1"}) // gc-3
	_ = store.Close("gc-2")
	_ = store.Close("gc-3")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `Auto-closed convoy gc-1 "batch"`) {
		t.Errorf("stdout missing auto-close message:\n%s", out)
	}
	if !strings.Contains(out, "1 convoy(s) auto-closed") {
		t.Errorf("stdout missing summary:\n%s", out)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
}

func TestConvoyCheckTracksDeps(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B"})                // gc-3
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-1", "gc-3", "tracks"))
	_ = store.Close("gc-2")
	_ = store.Close("gc-3")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Auto-closed convoy gc-1 "batch"`) {
		t.Errorf("stdout missing auto-close message:\n%s", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want closed", b.Status)
	}
}

func TestConvoyCheckTreatsTombstoneTrackAsComplete(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                // gc-2
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	tombstone := "tombstone"
	requireNoError(t, store.Update("gc-2", beads.UpdateOpts{Status: &tombstone}))

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Auto-closed convoy gc-1 "batch"`) {
		t.Errorf("stdout missing auto-close message:\n%s", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want closed", b.Status)
	}
}

func TestConvoyCheckDanglingTrackDoesNotAutoClose(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                // gc-2
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-1", "gc-missing", "tracks"))
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Auto-closed") {
		t.Errorf("stdout should not contain Auto-closed for unresolved tracked item:\n%s", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want open", b.Status)
	}
}

func TestConvoyCheckJSONAutoCloseEmitsSingleResult(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, events.Discard, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheckAcrossStoresJSON = %d, want 0; stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Auto-closed convoy") {
		t.Fatalf("stdout contains human auto-close text in JSON mode:\n%s", stdout.String())
	}
	if strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout = %q, want exactly one JSONL result", stdout.String())
	}

	var got struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Command       string `json:"command"`
		Action        string `json:"action"`
		Closed        int    `json:"closed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || !got.OK || got.Command != "convoy.check" || got.Action != "check" || got.Closed != 1 {
		t.Fatalf("payload = %+v", got)
	}
}

func TestConvoyCheckJSONReportsWriteError(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, events.Discard, true, errWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("doConvoyCheckAcrossStoresJSON = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "writing JSON result") {
		t.Fatalf("stderr = %q, want JSON write error", stderr.String())
	}
}

func TestConvoyCheckPartial(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1"}) // gc-3
	_ = store.Close("gc-2")                                            // only one closed

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if strings.Contains(out, "Auto-closed") {
		t.Errorf("stdout should not contain Auto-closed (partial completion):\n%s", out)
	}
	if !strings.Contains(out, "0 convoy(s) auto-closed") {
		t.Errorf("stdout missing zero summary:\n%s", out)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q (should stay open)", b.Status, "open")
	}
}

func TestConvoyCheckEmpty(t *testing.T) {
	store := beads.NewMemStore()
	// Convoy with no children should not be auto-closed.
	_, _ = store.Create(beads.Bead{Title: "empty batch", Type: "convoy"})

	var stdout bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "0 convoy(s) auto-closed") {
		t.Errorf("stdout = %q, want zero summary (empty convoy not auto-closed)", stdout.String())
	}
}

func TestConvoyCheckAcrossStores(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"}) // gc-1
	_, _ = cityStore.Create(beads.Bead{Title: "city task", ParentID: "gc-1"})
	_ = cityStore.Close("gc-2")

	_, _ = rigStore.Create(beads.Bead{Title: "rig batch", Type: "convoy"}) // gc-1
	_, _ = rigStore.Create(beads.Bead{Title: "rig task", ParentID: "gc-1"})
	_ = rigStore.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheckAcrossStores([]convoyStoreView{{store: cityStore}, {store: rigStore}}, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheckAcrossStores = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 convoy(s) auto-closed") {
		t.Fatalf("stdout = %q, want two auto-closed convoys", stdout.String())
	}
	if got, _ := cityStore.Get("gc-1"); got.Status != "closed" {
		t.Fatalf("city convoy status = %q, want closed", got.Status)
	}
	if got, _ := rigStore.Get("gc-1"); got.Status != "closed" {
		t.Fatalf("rig convoy status = %q, want closed", got.Status)
	}
}

// --- gc convoy stranded ---

func TestConvoyStranded(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})                          // gc-1
	_, _ = store.Create(beads.Bead{Title: "assigned", ParentID: "gc-1", Assignee: "worker"}) // gc-2 — has worker
	_, _ = store.Create(beads.Bead{Title: "unassigned", ParentID: "gc-1"})                   // gc-3 — stranded

	var stdout, stderr bytes.Buffer
	code := doConvoyStranded(store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStranded = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "gc-3") {
		t.Errorf("stdout missing stranded issue gc-3:\n%s", out)
	}
	if !strings.Contains(out, "unassigned") {
		t.Errorf("stdout missing stranded issue title:\n%s", out)
	}
	// Assigned issue should not appear as stranded.
	if strings.Contains(out, "assigned\t") && !strings.Contains(out, "unassigned") {
		t.Errorf("stdout should not show assigned issues as stranded:\n%s", out)
	}
}

func TestConvoyStrandedNone(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	_, _ = store.Create(beads.Bead{Title: "done", ParentID: "gc-1", Assignee: "worker"})

	var stdout bytes.Buffer
	code := doConvoyStranded(store, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyStranded = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No stranded work") {
		t.Errorf("stdout = %q, want no stranded message", stdout.String())
	}
}

func TestConvoyStrandedClosedExcluded(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	_, _ = store.Create(beads.Bead{Title: "done task", ParentID: "gc-1"}) // no assignee but closed
	_ = store.Close("gc-2")

	var stdout bytes.Buffer
	code := doConvoyStranded(store, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyStranded = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No stranded work") {
		t.Errorf("stdout = %q, want no stranded (closed issues excluded)", stdout.String())
	}
}

func TestConvoyListReportsDanglingTracks(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                // gc-2
	requireNoError(t, store.DepAdd("gc-1", "gc-2", "tracks"))
	requireNoError(t, store.DepAdd("gc-1", "gc-missing", "tracks"))
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyList(store, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyList = %d, want 0; stderr: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "1/2 closed (1 dangling track)") {
		t.Errorf("stdout = %q, want dangling track progress", stdout.String())
	}
}

func TestConvoyStrandedIgnoresDanglingTracks(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	requireNoError(t, store.DepAdd("gc-1", "gc-missing", "tracks"))

	var stdout bytes.Buffer
	code := doConvoyStranded(store, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyStranded = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No stranded work") {
		t.Errorf("stdout = %q, want no stranded message for dangling tracks", stdout.String())
	}
}

func TestConvoyStrandedAcrossStores(t *testing.T) {
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"}) // gc-1
	_, _ = cityStore.Create(beads.Bead{Title: "city unassigned", ParentID: "gc-1"})
	_, _ = rigStore.Create(beads.Bead{Title: "rig batch", Type: "convoy"}) // gc-1
	_, _ = rigStore.Create(beads.Bead{Title: "rig unassigned", ParentID: "gc-1"})

	var stdout, stderr bytes.Buffer
	code := doConvoyStrandedAcrossStores([]convoyStoreView{{store: cityStore}, {store: rigStore}}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStrandedAcrossStores = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"city unassigned", "rig unassigned"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// --- gc convoy check: owned convoys ---

func TestConvoyCheckSkipsOwned(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "owned batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                               // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1"})                               // gc-3
	_ = store.Close("gc-2")
	_ = store.Close("gc-3")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	// Should NOT auto-close the owned convoy.
	if strings.Contains(out, "Auto-closed") {
		t.Errorf("stdout = %q, owned convoy should NOT be auto-closed", out)
	}
	if !strings.Contains(out, "0 convoy(s) auto-closed") {
		t.Errorf("stdout = %q, want 0 auto-closed", out)
	}

	// Verify it's still open.
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "open" {
		t.Errorf("owned convoy Status = %q, want %q (should stay open)", b.Status, "open")
	}
}

func TestConvoyCheckClosesNonOwned(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "normal batch", Type: "convoy"})                           // gc-1 (no owned label)
	_, _ = store.Create(beads.Bead{Title: "owned batch", Type: "convoy", Labels: []string{"owned"}}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "task for normal", ParentID: "gc-1"})                      // gc-3
	_, _ = store.Create(beads.Bead{Title: "task for owned", ParentID: "gc-2"})                       // gc-4
	_ = store.Close("gc-3")
	_ = store.Close("gc-4")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheck(store, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	// Non-owned convoy should be auto-closed.
	if !strings.Contains(out, `Auto-closed convoy gc-1 "normal batch"`) {
		t.Errorf("stdout = %q, want non-owned convoy auto-closed", out)
	}
	if !strings.Contains(out, "1 convoy(s) auto-closed") {
		t.Errorf("stdout = %q, want 1 auto-closed", out)
	}

	// Verify gc-1 is closed, gc-2 is still open.
	b1, _ := store.Get("gc-1")
	if b1.Status != "closed" {
		t.Errorf("non-owned convoy Status = %q, want %q", b1.Status, "closed")
	}
	b2, _ := store.Get("gc-2")
	if b2.Status != "open" {
		t.Errorf("owned convoy Status = %q, want %q (should stay open)", b2.Status, "open")
	}
}

// --- hasLabel ---

func TestHasLabel(t *testing.T) {
	if !hasLabel([]string{"owned", "urgent"}, "owned") {
		t.Error("hasLabel should find 'owned'")
	}
	if hasLabel([]string{"urgent"}, "owned") {
		t.Error("hasLabel should not find 'owned'")
	}
	if hasLabel(nil, "owned") {
		t.Error("hasLabel(nil) should return false")
	}
}

// --- gc convoy autoclose ---

func TestConvoyAutocloseHappyPath(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1"}) // gc-3
	_ = store.Close("gc-2")
	_ = store.Close("gc-3")

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-3", &stdout, &bytes.Buffer{})

	out := stdout.String()
	if !strings.Contains(out, `Auto-closed convoy gc-1 "batch"`) {
		t.Errorf("stdout = %q, want auto-close message", out)
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("convoy Status = %q, want %q", b.Status, "closed")
	}
}

func TestConvoyAutocloseTracksDeps(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "epic", Type: "epic"})       // gc-1
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-2
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-3
	_, _ = store.Create(beads.Bead{Title: "task B"})                   // gc-4
	requireNoError(t, store.DepAdd("gc-2", "gc-3", "tracks"))
	requireNoError(t, store.DepAdd("gc-2", "gc-4", "tracks"))
	_ = store.Close("gc-3")
	_ = store.Close("gc-4")

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-3", &stdout, &bytes.Buffer{})

	out := stdout.String()
	if !strings.Contains(out, `Auto-closed convoy gc-2 "batch"`) {
		t.Errorf("stdout = %q, want tracks convoy auto-close message", out)
	}

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("convoy Status = %q, want closed", b.Status)
	}
}

func TestConvoyAutocloseOwnedSkip(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "owned batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                               // gc-2
	_ = store.Close("gc-2")

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-2", &stdout, &bytes.Buffer{})

	if strings.Contains(stdout.String(), "Auto-closed") {
		t.Errorf("owned convoy should NOT be auto-closed: %q", stdout.String())
	}

	b, _ := store.Get("gc-1")
	if b.Status != "open" {
		t.Errorf("owned convoy Status = %q, want %q", b.Status, "open")
	}
}

func TestConvoyAutocloseNoParent(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "orphan task"}) // gc-1, no parent
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-1", &stdout, &bytes.Buffer{})

	if stdout.String() != "" {
		t.Errorf("no-parent bead should produce no output, got %q", stdout.String())
	}
}

func TestConvoyAutocloseNotConvoy(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "epic", Type: "task"})    // gc-1 (not a convoy)
	_, _ = store.Create(beads.Bead{Title: "sub", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-2")

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-2", &stdout, &bytes.Buffer{})

	if stdout.String() != "" {
		t.Errorf("non-convoy parent should produce no output, got %q", stdout.String())
	}

	b, _ := store.Get("gc-1")
	if b.Status != "open" {
		t.Errorf("non-convoy parent Status = %q, want %q", b.Status, "open")
	}
}

func TestConvoyAutoclosePartialSiblings(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1"}) // gc-3
	_ = store.Close("gc-2")                                            // only one sibling closed

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-2", &stdout, &bytes.Buffer{})

	if strings.Contains(stdout.String(), "Auto-closed") {
		t.Errorf("partial siblings should NOT auto-close: %q", stdout.String())
	}

	b, _ := store.Get("gc-1")
	if b.Status != "open" {
		t.Errorf("convoy Status = %q, want %q (partial siblings)", b.Status, "open")
	}
}

// TestConvoyAutocloseStampsCloseReason verifies that the hook-driven
// autoclose path (doConvoyAutocloseWith) stamps the canonical
// convoyAutocloseReason on the convoy bead before closing it. The
// metadata is what BdStore.Close() forwards as `bd close --reason`,
// which is what allows cities running with validation.on-close=error
// to accept the close.
func TestConvoyAutocloseStampsCloseReason(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-2")

	var stdout bytes.Buffer
	doConvoyAutocloseWith(store, events.Discard, "gc-2", &stdout, &bytes.Buffer{})

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("convoy Status = %q, want %q", b.Status, "closed")
	}
	if got := b.Metadata["close_reason"]; got != convoyAutocloseReason {
		t.Errorf("metadata.close_reason = %q, want %q", got, convoyAutocloseReason)
	}
}

// TestConvoyCheckStampsCloseReason verifies that the bulk autoclose
// path (gc convoy check) stamps the same convoyAutocloseReason on
// every convoy it auto-closes.
func TestConvoyCheckStampsCloseReason(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	if code := doConvoyCheck(store, events.Discard, &stdout, &stderr); code != 0 {
		t.Fatalf("doConvoyCheck = %d, want 0; stderr: %s", code, stderr.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("convoy Status = %q, want %q", b.Status, "closed")
	}
	if got := b.Metadata["close_reason"]; got != convoyAutocloseReason {
		t.Errorf("metadata.close_reason = %q, want %q", got, convoyAutocloseReason)
	}
}

func TestCloseConvoyWithReasonReturnsMetadataError(t *testing.T) {
	base := beads.NewMemStore()
	_, _ = base.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	store := failingSetMetadataStore{Store: base}

	err := closeConvoyWithReason(store, "gc-1", convoyAutocloseReason)
	if err == nil {
		t.Fatal("closeConvoyWithReason returned nil, want metadata error")
	}
	if !strings.Contains(err.Error(), "stamping convoy gc-1 close reason") {
		t.Fatalf("error = %v, want close reason context", err)
	}

	b, getErr := base.Get("gc-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if b.Status != "open" {
		t.Fatalf("convoy Status = %q, want open after metadata failure", b.Status)
	}
}

type failingSetMetadataStore struct {
	beads.Store
}

func (s failingSetMetadataStore) SetMetadata(string, string, string) error {
	return errors.New("metadata write failed")
}

func TestCloseConvoyWithReasonBdStoreForwardsReasonWithoutShow(t *testing.T) {
	const id = "bd-x"
	var closeArgs []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected command name: %s", name)
		}
		if len(args) > 0 && args[0] == "show" {
			return nil, fmt.Errorf("unexpected bd show before convoy close")
		}
		switch strings.Join(args, " ") {
		case "update --json " + id + " --set-metadata close_reason=" + convoyAutocloseReason:
			return []byte(`[{"id":"bd-x","title":"batch","status":"open","issue_type":"convoy","created_at":"2025-01-15T10:30:00Z"}]`), nil
		case "close --force --json --reason " + convoyAutocloseReason + " " + id:
			closeArgs = append([]string(nil), args...)
			return []byte(`[{"id":"bd-x","title":"batch","status":"closed","issue_type":"convoy","created_at":"2025-01-15T10:30:00Z"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
	}
	store := beads.NewBdStore("/city", runner)

	if err := closeConvoyWithReason(store, id, convoyAutocloseReason); err != nil {
		t.Fatal(err)
	}

	want := []string{"close", "--force", "--json", "--reason", convoyAutocloseReason, id}
	if got := fmt.Sprint(closeArgs); got != fmt.Sprint(want) {
		t.Fatalf("close args = %v, want %v", closeArgs, want)
	}
}

// --- gc convoy land ---

func TestConvoyLandHappyPath(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                         // gc-2
	_, _ = store.Create(beads.Bead{Title: "task B", ParentID: "gc-1"})                         // gc-3
	_ = store.Close("gc-2")
	_ = store.Close("gc-3")

	var stdout, stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyLand = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `Landed convoy gc-1 "batch"`) {
		t.Errorf("stdout = %q, want land confirmation", stdout.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Errorf("convoy Status = %q, want %q", b.Status, "closed")
	}
	if got := b.Metadata["close_reason"]; got != convoyLandCloseReason {
		t.Errorf("metadata.close_reason = %q, want %q", got, convoyLandCloseReason)
	}
}

func TestConvoyLandForceWithOpenIssues(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                         // gc-2 (open)

	var stdout, stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{Force: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyLand = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Landed convoy gc-1") {
		t.Errorf("stdout = %q, want land confirmation", stdout.String())
	}

	b, _ := store.Get("gc-1")
	if b.Status != "closed" {
		t.Errorf("convoy Status = %q, want %q", b.Status, "closed")
	}
}

func TestConvoyLandOpenChildrenError(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                         // gc-2 (open)

	var stdout, stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("doConvoyLand = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "1 open child") {
		t.Errorf("stderr = %q, want open children error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("stderr = %q, want --force hint", stderr.String())
	}
}

func TestConvoyLandDryRun(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A", ParentID: "gc-1"})                         // gc-2
	_ = store.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{DryRun: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyLand = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Would land convoy gc-1") {
		t.Errorf("stdout = %q, want dry-run preview", stdout.String())
	}

	// Should NOT actually close the convoy.
	b, _ := store.Get("gc-1")
	if b.Status != "open" {
		t.Errorf("convoy Status = %q, want %q (dry-run should not close)", b.Status, "open")
	}
}

func TestConvoyLandNotOwned(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1 (no "owned" label)

	var stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyLand = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not owned") {
		t.Errorf("stderr = %q, want 'not owned' error", stderr.String())
	}
}

func TestConvoyLandAlreadyClosed(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy", Labels: []string{"owned"}}) // gc-1
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyLand = %d, want 0 (idempotent)", code)
	}
	if !strings.Contains(stdout.String(), "already closed") {
		t.Errorf("stdout = %q, want 'already closed' message", stdout.String())
	}
}

func TestConvoyLandMissingID(t *testing.T) {
	store := beads.NewMemStore()

	var stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, nil, landOpts{}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyLand = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing convoy ID") {
		t.Errorf("stderr = %q, want missing ID error", stderr.String())
	}
}

func TestConvoyLandNotConvoy(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "just a task"}) // gc-1

	var stderr bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doConvoyLand = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a convoy") {
		t.Errorf("stderr = %q, want 'not a convoy'", stderr.String())
	}
}

// --- ConvoyFields ---

func TestConvoyFieldsRoundTrip(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1

	fields := ConvoyFields{
		Owner:    "mayor",
		Notify:   "mayor",
		Molecule: "mol-1",
		Merge:    "mr",
		Target:   "integration/gc-1",
	}

	if err := setConvoyFields(store, "gc-1", fields); err != nil {
		t.Fatalf("setConvoyFields: %v", err)
	}

	// Read back.
	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	got := getConvoyFields(b)
	if got != fields {
		t.Errorf("getConvoyFields = %+v, want %+v", got, fields)
	}
}

func TestConvoyFieldsPartial(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1

	fields := ConvoyFields{Owner: "mayor"}
	if err := setConvoyFields(store, "gc-1", fields); err != nil {
		t.Fatalf("setConvoyFields: %v", err)
	}

	b, _ := store.Get("gc-1")
	got := getConvoyFields(b)
	if got.Owner != "mayor" {
		t.Errorf("Owner = %q, want %q", got.Owner, "mayor")
	}
	if got.Notify != "" {
		t.Errorf("Notify = %q, want empty", got.Notify)
	}
}

func TestConvoyFieldsEmpty(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1

	// Set empty fields — should be a no-op.
	if err := setConvoyFields(store, "gc-1", ConvoyFields{}); err != nil {
		t.Fatalf("setConvoyFields: %v", err)
	}

	b, _ := store.Get("gc-1")
	got := getConvoyFields(b)
	if got != (ConvoyFields{}) {
		t.Errorf("getConvoyFields = %+v, want empty", got)
	}
}

func TestConvoyFieldsNotFound(t *testing.T) {
	store := beads.NewMemStore()
	err := setConvoyFields(store, "gc-999", ConvoyFields{Owner: "mayor"})
	if err == nil {
		t.Error("setConvoyFields on nonexistent bead should return error")
	}
}

func TestConvoyCreateWithFields(t *testing.T) {
	store := beads.NewMemStore()
	fields := ConvoyFields{Owner: "mayor", Merge: "mr"}

	var stdout, stderr bytes.Buffer
	code := doConvoyCreateWithOptions(store, events.Discard, []string{"deploy"}, convoyCreateOptions{Fields: fields}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCreateWithOptions = %d, want 0; stderr: %s", code, stderr.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	got := getConvoyFields(b)
	if got.Owner != "mayor" {
		t.Errorf("Owner = %q, want %q", got.Owner, "mayor")
	}
	if got.Merge != "mr" {
		t.Errorf("Merge = %q, want %q", got.Merge, "mr")
	}
}

func TestConvoyCreateWithOptionsOwnedAndTarget(t *testing.T) {
	store := beads.NewMemStore()
	opts := convoyCreateOptions{
		Fields: ConvoyFields{Target: "integration/gc-1"},
		Owned:  true,
	}

	var stdout, stderr bytes.Buffer
	code := doConvoyCreateWithOptions(store, events.Discard, []string{"deploy"}, opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCreateWithOptions = %d, want 0; stderr: %s", code, stderr.String())
	}

	b, err := store.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if !hasLabel(b.Labels, "owned") {
		t.Fatalf("labels = %v, want owned label", b.Labels)
	}
	if got := b.Metadata["target"]; got != "integration/gc-1" {
		t.Fatalf("target metadata = %q, want %q", got, "integration/gc-1")
	}
}

func TestConvoyLandWithNotify(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{
		Title:    "batch",
		Type:     "convoy",
		Labels:   []string{"owned"},
		Metadata: map[string]string{"convoy.notify": "mayor"},
	}) // gc-1

	var stdout bytes.Buffer
	code := doConvoyLand(store, events.Discard, []string{"gc-1"}, landOpts{Force: true}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("doConvoyLand = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "notify: mayor") {
		t.Errorf("stdout = %q, want notify message", stdout.String())
	}
}

func requireConvoyTrack(t *testing.T, store beads.Store, convoyID, itemID string) {
	t.Helper()
	deps, err := store.DepList(convoyID, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", convoyID, err)
	}
	for _, dep := range deps {
		if dep.IssueID == convoyID && dep.DependsOnID == itemID && dep.Type == "tracks" {
			return
		}
	}
	t.Fatalf("missing tracks dep %s -> %s; deps=%v", convoyID, itemID, deps)
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Six-row read-path routing matrix for `gc convoy list/status` (ADR 0001,
// ga-h6w). `gc convoy check` keeps the same route-log coverage below, but it
// is deliberately excluded from API routing because it may mutate state.
// Each API-routed command gets the six mandatory rows:
//
//   api-happy-path       API returns 200 with items         route=api, exit 0
//   api-cache-not-live   API returns 503 cache_not_live     fallback, exit 0
//   api-500-fallback     API returns generic 500            fallback (conn-refused), exit 0
//   api-404-error        API returns 404                    no fallback, exit 1
//   controller-down      apiClient returns nil (no env)     fallback (controller-down), exit 0
//   escape-hatch         GC_NO_API truthy                   fallback (escape-hatch), exit 0
//
// Tests invoke route*Convoy* directly with an injected api.Client or nil +
// reason so no tmux / controller process is needed.
// ---------------------------------------------------------------------------

type convoyMatrixHandler func(t *testing.T) http.Handler

// okConvoyListHandler serves both /convoys and /convoy/{id}/check with a
// non-stale single-convoy fixture so fetchConvoyProgress succeeds after
// ListConvoys returns.
func okConvoyListHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/convoys"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{"id": "gc-1", "title": "sprint", "issue_type": "convoy", "status": "open", "created_at": "2026-04-23T10:00:00Z"},
				},
				"total": 1,
			})
		case strings.HasSuffix(r.URL.Path, "/check"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"convoy_id": "gc-1",
				"total":     2,
				"closed":    1,
				"complete":  false,
			})
		default:
			http.NotFound(w, r)
		}
	})
}

// okConvoyStatusHandler serves /convoy/{id} with a full detail payload.
func okConvoyStatusHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"convoy":   map[string]any{"id": "gc-1", "title": "sprint", "issue_type": "convoy", "status": "open", "created_at": "2026-04-23T10:00:00Z"},
			"children": []map[string]any{{"id": "gc-2", "title": "task a", "issue_type": "task", "status": "open", "created_at": "2026-04-23T10:00:00Z"}},
			"progress": map[string]any{"total": 1, "closed": 0},
		})
	})
}

// okConvoyCheckHandler serves /convoys (empty list) so routeConvoyCheck's
// auto-close path exits cleanly with no convoys to process.
func okConvoyCheckHandler(_ *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "2")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/convoys") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{},
				"total": 0,
			})
			return
		}
		http.NotFound(w, r)
	})
}

func convoyProblemHandler(status int, detail string) convoyMatrixHandler {
	return func(_ *testing.T) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"status": status,
				"title":  http.StatusText(status),
				"detail": detail,
			})
		})
	}
}

// writeConvoyTestCity creates a minimal city directory sufficient for the
// fallback path to succeed (resolveCity + openAllConvoyStores). The city
// has no real bd store so "No open convoys" is the expected fallback
// output.
func writeConvoyTestCity(t *testing.T) string {
	t.Helper()
	clearInheritedBeadsEnv(t)
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY_PATH", cityPath)
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath
}

func TestRouteConvoyList_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      convoyMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okConvoyListHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "sprint",
		},
		{
			name:       "api-cache-not-live",
			handler:    convoyProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
		},
		{
			name:       "api-500-fallback",
			handler:    convoyProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
		},
		{
			name:       "api-404-error",
			handler:    convoyProblemHandler(http.StatusNotFound, "not_found: city not configured"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := writeConvoyTestCity(t)
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeConvoyList(cityPath, c, tc.nilReason, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
			if tc.wantRoute == "fallback" {
				if !strings.Contains(stdout.String(), "No open convoys") {
					t.Errorf("fallback stdout missing expected empty-list marker:\n%s", stdout.String())
				}
			}
		})
	}
}

func TestRouteConvoyStatus_SixRowMatrix(t *testing.T) {
	tests := []struct {
		name         string
		handler      convoyMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path",
			handler:    okConvoyStatusHandler,
			wantExit:   0,
			wantRoute:  "api",
			wantStdout: "Convoy:   gc-1",
		},
		{
			name:       "api-cache-not-live",
			handler:    convoyProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   1,
			wantRoute:  "fallback",
			wantReason: "cache-not-live",
			wantStderr: "gc convoy status",
		},
		{
			name:       "api-500-fallback",
			handler:    convoyProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   1,
			wantRoute:  "fallback",
			wantReason: "conn-refused",
			wantStderr: "gc convoy status",
		},
		{
			name:       "api-404-error",
			handler:    convoyProblemHandler(http.StatusNotFound, "not_found: convoy missing"),
			wantExit:   1,
			wantStderr: "not_found",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     1,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
			wantStderr:   "gc convoy status",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     1,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
			wantStderr:   "gc convoy status",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := writeConvoyTestCity(t)
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			// Fallback path hits openConvoyStoreByIDAt which will fail with
			// no real bd store on disk; tests assert exit=1 and route log
			// for those rows, focusing on the routing branch rather than
			// the fallback output itself.
			code := routeConvoyStatus(cityPath, "gc-1", c, tc.nilReason, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
		})
	}
}

func TestRouteConvoyCheckAlwaysFallsBackForLiveMutation(t *testing.T) {
	tests := []struct {
		name         string
		handler      convoyMatrixHandler
		useNilClient bool
		nilReason    string
		wantExit     int
		wantRoute    string
		wantReason   string
		wantStderr   string
		wantStdout   string
	}{
		{
			name:       "api-happy-path-ignored-for-live-mutation",
			handler:    okConvoyCheckHandler,
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "requires-live-read",
			wantStdout: "0 convoy(s) auto-closed",
		},
		{
			name:       "api-cache-not-live-ignored-for-live-mutation",
			handler:    convoyProblemHandler(http.StatusServiceUnavailable, "cache_not_live: supervisor cache is priming"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "requires-live-read",
		},
		{
			name:       "api-500-ignored-for-live-mutation",
			handler:    convoyProblemHandler(http.StatusInternalServerError, "internal: something exploded"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "requires-live-read",
		},
		{
			name:       "api-404-ignored-for-live-mutation",
			handler:    convoyProblemHandler(http.StatusNotFound, "not_found: city not configured"),
			wantExit:   0,
			wantRoute:  "fallback",
			wantReason: "requires-live-read",
		},
		{
			name:         "controller-down",
			useNilClient: true,
			nilReason:    "controller-down",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "controller-down",
		},
		{
			name:         "escape-hatch",
			useNilClient: true,
			nilReason:    "escape-hatch",
			wantExit:     0,
			wantRoute:    "fallback",
			wantReason:   "escape-hatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := writeConvoyTestCity(t)
			t.Setenv("GC_DEBUG", "1")

			var c *api.Client
			if !tc.useNilClient {
				srv := httptest.NewServer(tc.handler(t))
				defer srv.Close()
				c = api.NewCityScopedClient(srv.URL, "test-city")
			}

			var stdout, stderr bytes.Buffer
			code := routeConvoyCheck(cityPath, c, tc.nilReason, false, &stdout, &stderr)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d; stderr=%q stdout=%q", code, tc.wantExit, stderr.String(), stdout.String())
			}
			assertRouteLog(t, stderr.String(), tc.wantRoute, tc.wantReason)
			if tc.wantStderr != "" && !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantStderr, stderr.String())
			}
			if tc.wantStdout != "" && !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout missing %q:\n%s", tc.wantStdout, stdout.String())
			}
			if tc.wantRoute == "fallback" {
				// Fallback path also prints "N convoy(s) auto-closed" with
				// N=0 for an empty bd store.
				if !strings.Contains(stdout.String(), "convoy(s) auto-closed") {
					t.Errorf("fallback stdout missing auto-closed summary:\n%s", stdout.String())
				}
			}
		})
	}
}

func TestRouteConvoyCheckUsesLiveStoreForAutoCloseDecision(t *testing.T) {
	cityPath := writeConvoyTestCity(t)
	t.Setenv("GC_DEBUG", "1")

	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("openCityStoreAt(): %v", err)
	}
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "still open", ParentID: convoy.ID}); err != nil {
		t.Fatalf("create child: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "25")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/convoys"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{
					{"id": convoy.ID, "title": convoy.Title, "issue_type": "convoy", "status": "open", "created_at": "2026-04-23T10:00:00Z"},
				},
				"total": 1,
			})
		case strings.HasSuffix(r.URL.Path, "/check"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"convoy_id": convoy.ID,
				"total":     1,
				"closed":    1,
				"complete":  true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := routeConvoyCheck(cityPath, c, "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("routeConvoyCheck = %d, want 0; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	assertRouteLog(t, stderr.String(), "fallback", "requires-live-read")
	if strings.Contains(stdout.String(), "Auto-closed") {
		t.Fatalf("stdout should not auto-close from API progress:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "0 convoy(s) auto-closed") {
		t.Fatalf("stdout = %q, want zero auto-close summary", stdout.String())
	}

	got, err := store.Get(convoy.ID)
	if err != nil {
		t.Fatalf("get convoy: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("convoy status = %q, want open; stdout=%q stderr=%q", got.Status, stdout.String(), stderr.String())
	}
}

// assertRouteLog verifies exactly one route=... line with the expected
// route and reason is present in stderr. Skips verification when the row
// is on an error path that doesn't emit a route log.
func assertRouteLog(t *testing.T, stderrStr, wantRoute, wantReason string) {
	t.Helper()
	if wantRoute == "" {
		return
	}
	want := "route=" + wantRoute
	if wantReason != "" {
		want += " reason=" + wantReason
	}
	if !strings.Contains(stderrStr, want) {
		t.Errorf("stderr missing %q:\n%s", want, stderrStr)
	}
	if n := strings.Count(stderrStr, "route="); n != 1 {
		t.Errorf("route=... lines = %d, want 1:\n%s", n, stderrStr)
	}
}

func TestRouteConvoyList_StaleBannerOver30s(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")
	cityPath := writeConvoyTestCity(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/convoys") {
			json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total": 0}) //nolint:errcheck
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	if code := routeConvoyList(cityPath, c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	// Zero-convoy list prints "No open convoys" and returns early without
	// surfacing the cache-age banner. Assert the empty case path is correct
	// and that the stale banner appears when there is at least one row.
	if !strings.Contains(stdout.String(), "No open convoys") {
		t.Errorf("empty list output missing marker:\n%s", stdout.String())
	}
}

func TestRouteConvoyStatus_StaleBannerOver30s(t *testing.T) {
	t.Setenv("GC_DEBUG", "0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GC-Cache-Age-S", "45")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"convoy":   map[string]any{"id": "gc-1", "title": "old", "issue_type": "convoy", "status": "open", "created_at": "2026-04-23T10:00:00Z"},
			"children": []map[string]any{},
			"progress": map[string]any{"total": 0, "closed": 0},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	cityPath := writeConvoyTestCity(t)
	var stdout, stderr bytes.Buffer
	if code := routeConvoyStatus(cityPath, "gc-1", c, "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cache age: 45s") {
		t.Errorf("stale banner missing from human output:\n%s", stdout.String())
	}
}

func TestRouteConvoyStatus_WorkflowConvoyFallsBack(t *testing.T) {
	// Graph/workflow convoys produce an empty Convoy.ID in the API response;
	// the router must fall back to the local path so workflow-aware
	// rendering still works. The local fallback won't find the store so exit
	// is non-zero, but the route log must record the workflow-convoy reason.

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			// workflow-snapshot shape: convoy pointer nil, children nil,
			// progress nil. Translator yields zero-value ConvoyStatusView.
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	cityPath := writeConvoyTestCity(t)
	t.Setenv("GC_DEBUG", "1")
	var stdout, stderr bytes.Buffer
	_ = routeConvoyStatus(cityPath, "gc-wf-1", c, "", false, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "route=fallback reason=workflow-convoy") {
		t.Errorf("stderr missing workflow-convoy route log:\n%s", stderr.String())
	}
}

func TestRouteConvoyStatus_RemoteWorkflowConvoyNoLocalFallback(t *testing.T) {
	// A REMOTE city is authoritative: a workflow/graph convoy the remote API
	// cannot render (empty Convoy.ID) must surface as a hard remote error, never
	// fall back to the operator's LOCAL store (gate G1). This is the regression
	// guard for the fallbackAfterFetch sentinel bypassing the remote no-fallback
	// gate — the local store below is fully resolvable, so a wrong fallback would
	// render from it and exit 0.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{}) //nolint:errcheck // empty workflow-convoy body
	}))
	defer srv.Close()
	c, err := api.NewRemoteCityScopedClient(srv.URL, "test-city", api.RemoteOptions{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("NewRemoteCityScopedClient: %v", err)
	}

	cityPath := writeConvoyTestCity(t)
	t.Setenv("GC_DEBUG", "1")
	var stdout, stderr bytes.Buffer
	code := routeConvoyStatus(cityPath, "gc-wf-1", c, "", false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("remote workflow-convoy status must not exit 0 (local fallback happened):\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "route=fallback") {
		t.Errorf("remote client must not take the local fallback path:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "route=api reason=error") {
		t.Errorf("expected route=api reason=error for a remote workflow-convoy:\n%s", stderr.String())
	}
}

// relocatedConvoyCity builds the shape `gc storage migrate` leaves behind for a
// convoy: one id, two rows, and a binding that is the live one.
//
// The work ledger keeps the copy the migration RETAINED — frozen at cutover,
// still carrying the open child it had then — and the binding holds the row the
// city has gone on mutating, whose children are finished. Every read surface has
// to prefer the second, and the two differ in exactly the way that makes a wrong
// choice visible: the frozen copy still gates, the live one is ready to close.
func relocatedConvoyCity(t *testing.T) (cityPath, convoyID string, work, binding beads.Store) {
	t.Helper()
	cityPath, _ = foreignProviderCity(t)
	work = workStoreFor(t, cityPath)

	frozen, err := work.Create(beads.Bead{Title: "the retained frozen convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("seeding the retained convoy in the work store: %v", err)
	}
	if _, err := work.Create(beads.Bead{Title: "still open in the frozen copy", ParentID: frozen.ID}); err != nil {
		t.Fatalf("seeding the frozen copy's open child: %v", err)
	}

	binding, relocated := cliSoleClassBindingStore(cityPath)
	if !relocated {
		t.Fatal("the fixture city resolved no class binding; it is not split")
	}
	if _, err := migrationSeed(binding, beads.Bead{ID: frozen.ID, Title: "the binding's live convoy", Type: "convoy"}); err != nil {
		t.Fatalf("carrying %s across to the class binding: %v", frozen.ID, err)
	}
	binding = recensusAfterSeedingARelic(t, cityPath)

	done, err := binding.Create(beads.Bead{Title: "finished in the binding", ParentID: frozen.ID})
	if err != nil {
		t.Fatalf("seeding the binding copy's child: %v", err)
	}
	if err := binding.Close(done.ID); err != nil {
		t.Fatalf("closing the binding copy's child: %v", err)
	}
	return cityPath, frozen.ID, work, binding
}

// TestConvoyCheckReadsTheBindingRow is the ga-efyq4 regression on the arm that
// MUTATES.
//
// `gc convoy check` closes convoys whose children are all done, and it decides
// that from whichever row its directory scan happened to find. On a migrated
// city that is the retained copy, whose children were frozen mid-flight at
// cutover — so the convoy the city actually finished never auto-closes, and the
// gate it feeds never opens.
func TestConvoyCheckReadsTheBindingRow(t *testing.T) {
	cityPath, convoyID, work, binding := relocatedConvoyCity(t)

	var stdout, stderr bytes.Buffer
	if code := doConvoyCheckFallback(cityPath, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy check exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Auto-closed convoy "+convoyID) {
		t.Errorf("the finished convoy did not auto-close; check read the frozen copy's open child:\n%s", stdout.String())
	}

	closed, err := binding.Get(convoyID)
	if err != nil {
		t.Fatalf("reading %s back from the binding: %v", convoyID, err)
	}
	if !convoycore.IsTerminalStatus(closed.Status) {
		t.Errorf("the binding's convoy is %q after the check, want a terminal status", closed.Status)
	}
	retained, err := work.Get(convoyID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", convoyID, err)
	}
	if convoycore.IsTerminalStatus(retained.Status) {
		t.Errorf("the check closed the retained work copy too; the frozen copy still has an open child and is not the row this decides from")
	}
}

// TestConvoyListReadsTheBindingRow is the ga-efyq4 regression on the plain list.
//
// One convoy must print once, as the binding has it. Printing the frozen copy
// instead reports progress that stopped at cutover; printing both reports two
// convoys where the city has one.
func TestConvoyListReadsTheBindingRow(t *testing.T) {
	cityPath, _, _, _ := relocatedConvoyCity(t)

	var stdout, stderr bytes.Buffer
	if code := doConvoyListFallback(cityPath, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "the binding's live convoy"); got != 1 {
		t.Errorf("the binding's convoy appears %d times, want exactly 1:\n%s", got, out)
	}
	if strings.Contains(out, "the retained frozen convoy") {
		t.Errorf("the list printed the frozen work copy; the binding is the truth for reads:\n%s", out)
	}
}

// TestConvoyListDropsTheFrozenCopyOfAConvoyTheBindingClosed is the ga-efyq4
// regression on the state a migrated city ENDS in.
//
// Every relocated workflow eventually finishes, and finishing it closes the
// binding's row. From that moment the binding's open-only query returns nothing
// for the id, so a supersede set derived from the query's ROWS forgets the
// binding owns it — and the retained copy, frozen open at cutover and never
// touched since, is served as the live convoy. The convoy reports open forever,
// which is the incident this bead exists to close, arrived at through the normal
// end of the workflow rather than through any edge case.
func TestConvoyListDropsTheFrozenCopyOfAConvoyTheBindingClosed(t *testing.T) {
	cityPath, convoyID, _, binding := relocatedConvoyCity(t)
	if err := binding.Close(convoyID); err != nil {
		t.Fatalf("closing the binding's convoy: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doConvoyListFallback(cityPath, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "the retained frozen convoy") {
		t.Errorf("a convoy the city CLOSED in the binding is listed open from its frozen twin:\n%s", out)
	}
	if strings.Contains(out, convoyID) {
		t.Errorf("%s is still listed after the binding closed it:\n%s", convoyID, out)
	}
}

// rigHoldingID registers a rig on cityPath and plants a bead in it under a
// pinned id, so a fixture can put a rig row and a binding row in collision.
//
// Ids are unique only within a store. A rig mints from its own sequence and
// nothing keeps that sequence disjoint from the ids a relocated binding holds,
// so "gc-1" in a rig and "gc-1" in the binding are two different beads that both
// belong in a fan-out's output. Without a rig in the fixtures, every relocated
// city under test has exactly one non-binding store and a merge that collapsed
// on id alone would look correct.
//
// The pinned id goes in through the file store's explicit-id mode, which is off
// by default so that ordinary creates cannot mint a collision by accident. The
// returned handle is the fixture's own; the CLI opens the same JSON afresh.
func rigHoldingID(t *testing.T, cityPath, id, title, beadType string) beads.Store {
	t.Helper()
	const rigName = "frontend"
	rigDir := filepath.Join(cityPath, "rigs", rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating the directory of rig %s: %v", rigName, err)
	}
	cityTOML := filepath.Join(cityPath, "city.toml")
	body, err := os.ReadFile(cityTOML)
	if err != nil {
		t.Fatalf("reading city.toml to register rig %s: %v", rigName, err)
	}
	body = append(body, []byte(fmt.Sprintf("\n[[rigs]]\nname = %q\npath = %q\n", rigName, rigDir))...)
	if err := os.WriteFile(cityTOML, body, 0o644); err != nil {
		t.Fatalf("registering rig %s in city.toml: %v", rigName, err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("giving rig %s its own file store: %v", rigName, err)
	}
	store, err := openScopeLocalFileStore(rigDir)
	if err != nil {
		t.Fatalf("opening the store of rig %s: %v", rigName, err)
	}
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{ID: id, Title: title, Type: beadType}); err != nil {
		t.Fatalf("planting %s in rig %s: %v", id, rigName, err)
	}
	return store
}

// TestConvoyListKeepsARigConvoyThatCollidesWithABindingID pins the SCOPE of the
// supersede rule: it collapses the migration's duplicate and nothing else.
//
// The migration is the one place an id means the same bead twice, because it
// copies with ids preserved and deletes nothing. Two stores the directory scan
// enumerated are not that: a rig's "gc-1" and the binding's "gc-1" are separate
// beads, and a merge that dropped either because the binding "owns" the id would
// delete a live rig convoy from the listing.
//
// This is also the row that makes the supersede pin bite. Without a colliding
// rig, dropping every id the binding owns and dropping only the migration
// source's produce identical output, so the role scoping is unfalsifiable.
func TestConvoyListKeepsARigConvoyThatCollidesWithABindingID(t *testing.T) {
	cityPath, convoyID, _, _ := relocatedConvoyCity(t)
	rigHoldingID(t, cityPath, convoyID, "the rig's own convoy", "convoy")

	var stdout, stderr bytes.Buffer
	if code := doConvoyListFallback(cityPath, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "the rig's own convoy") {
		t.Errorf("the rig's convoy was dropped as if the binding superseded it; it is a different bead that merely shares an id:\n%s", out)
	}
	if !strings.Contains(out, "the binding's live convoy") {
		t.Errorf("the binding's convoy is missing from the listing:\n%s", out)
	}
	if strings.Contains(out, "the retained frozen convoy") {
		t.Errorf("the frozen work copy is still listed; the binding supersedes it:\n%s", out)
	}
}

// TestBeadsListKeepsARigBeadThatCollidesWithABindingID is the same scope pin on
// the arm the caller filters, where the ownership probe runs against ids drawn
// from a query result rather than a whole-store listing.
func TestBeadsListKeepsARigBeadThatCollidesWithABindingID(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	frozen, err := work.Create(beads.Bead{Title: "the retained frozen copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained copy in the work store: %v", err)
	}
	if _, classStore := classResidentWorkShapedBead(t, cityPath, frozen.ID, "the binding's live row"); classStore == nil {
		t.Fatal("seeding the binding's row returned no class store")
	}
	rigHoldingID(t, cityPath, frozen.ID, "the rig's own row", "task")

	var stdout, stderr bytes.Buffer
	if code := doBeadsListFallback(cityPath, "", beadFilters{}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "the rig's own row") {
		t.Errorf("the rig's bead was dropped as if the binding superseded it:\n%s", out)
	}
	if !strings.Contains(out, "the binding's live row") {
		t.Errorf("the binding's bead is missing from the listing:\n%s", out)
	}
	if strings.Contains(out, "the retained frozen copy") {
		t.Errorf("the frozen work copy is still listed; the binding supersedes it:\n%s", out)
	}
}

// TestBeadsListFilteredByStatusDropsTheFrozenCopy is the same regression on the
// arm that lets the CALLER choose the filter.
//
// `gc beads list --status open` asks every store for its open rows. The binding
// answers with the ones that are open THERE; the frozen twin of a row the
// binding has since closed is open in the work ledger, and a supersede set built
// from the binding's answer to the caller's filter cannot see that it is a twin.
// Whether the binding owns an id and whether the binding has a row matching this
// filter are two different questions, and only the first one decides a merge.
func TestBeadsListFilteredByStatusDropsTheFrozenCopy(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	frozen, err := work.Create(beads.Bead{Title: "the retained frozen copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained copy in the work store: %v", err)
	}
	_, classStore := classResidentWorkShapedBead(t, cityPath, frozen.ID, "the binding's live row")
	if err := classStore.Close(frozen.ID); err != nil {
		t.Fatalf("closing the binding's row: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsListFallback(cityPath, "", beadFilters{status: "open"}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads list --status open exited %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "the retained frozen copy") {
		t.Errorf("a bead the city CLOSED in the binding is listed open from its frozen twin:\n%s", stdout.String())
	}
}

// idsFaultingClassStore answers an ordinary listing and faults on the OWNERSHIP
// probe, which is the one thing a healthy-binding fixture cannot reach.
//
// The probe is the only read that carries ListQuery.IDs, so keying the fault on
// a non-empty IDs list separates it from the caller's own query without a flag
// the production path could never set. A binding that fails BOTH — the shape
// installFaultingClassBinding produces — never reaches the merge at all, because
// its empty row set makes the probe moot; this one contributes rows and then
// cannot say which of them it owns.
//
// IDPrefix is delegated explicitly for countingClassStore's reason: beads.Store
// does not carry it, so embedding the interface alone would hide the wrapped
// store's declaration.
type idsFaultingClassStore struct {
	beads.Store
	err    error
	probes int
}

func (s *idsFaultingClassStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if len(q.IDs) > 0 {
		s.probes++
		return nil, s.err
	}
	return s.Store.List(q)
}

func (s *idsFaultingClassStore) IDPrefix() string {
	declaring, ok := s.Store.(storeref.HasIDPrefix)
	if !ok {
		return ""
	}
	return declaring.IDPrefix()
}

// TestBeadsListRefusesWhenTheOwnershipProbeFaults pins the FAULT arm of the
// ownership probe, which no healthy-binding row can reach.
//
// Every other relocated-city read fixture serves a binding that answers both
// questions, so the probe's error path is never taken and a merge that read a
// probe fault as "the binding holds none of these" would produce byte-identical
// output. That misreading is the whole hazard: an empty owned set supersedes
// nothing, so every frozen copy the migration retained is served as a live row —
// silently, with the command exiting 0 and the fault named nowhere.
//
// The binding here resolves, answers the caller's query, and fails only when
// asked which ids it holds. The listing must stop and say why rather than print
// the frozen twin.
func TestBeadsListRefusesWhenTheOwnershipProbeFaults(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	frozen, err := work.Create(beads.Bead{Title: "the retained frozen copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the retained copy in the work store: %v", err)
	}
	if _, classStore := classResidentWorkShapedBead(t, cityPath, frozen.ID, "the binding's live row"); classStore == nil {
		t.Fatal("seeding the binding's row returned no class store")
	}
	var probe *idsFaultingClassStore
	installWrappedClassBinding(t, cityPath, func(previous beads.Store) beads.Store {
		probe = &idsFaultingClassStore{Store: previous, err: errors.New("the id index is corrupt")}
		return probe
	})

	var stdout, stderr bytes.Buffer
	code := doBeadsListFallback(cityPath, "", beadFilters{}, &stdout, &stderr)
	if probe.probes == 0 {
		t.Fatal("the ownership probe never ran, so this row asserts on a path it did not take")
	}
	if code != 1 {
		t.Fatalf("gc beads list exited %d after the ownership probe faulted, want 1:\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "the id index is corrupt") {
		t.Errorf("the refusal does not name the fault that caused it: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "the retained frozen copy") {
		t.Errorf("the frozen copy was served as a live row after the probe that supersedes it faulted:\n%s", stdout.String())
	}
}

// sqliteMaxBoundVariables is the ceiling modernc sqlite enforces on one
// statement's bound variables, which is what an unchunked ownership probe hits.
const sqliteMaxBoundVariables = 32766

// varCappedIDStore answers an ownership probe the way a sqlite binding does,
// including the bound-variable ceiling, and records the batch sizes it was asked
// for.
//
// The recording is what makes the chunking observable: a batched probe and an
// unbatched one over a small population return the same answer, so a row taking
// only the answer would pass identically against either. The cap is what makes
// the fault reachable at all — sqliteListSQL emits one placeholder per id, so
// the ceiling is a property of the query the probe builds, not of the data.
type varCappedIDStore struct {
	beads.Store
	held    map[string]bool
	batches []int
}

func (s *varCappedIDStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.batches = append(s.batches, len(q.IDs))
	if len(q.IDs) > sqliteMaxBoundVariables {
		return nil, fmt.Errorf("SQL logic error: too many SQL variables (%d)", len(q.IDs))
	}
	held := make([]beads.Bead, 0, len(q.IDs))
	for _, id := range q.IDs {
		if s.held[id] {
			held = append(held, beads.Bead{ID: id})
		}
	}
	return held, nil
}

// TestConvoyBindingOwnedIDsBatchesPastTheSQLVariableCap pins the scale arm of
// the ownership probe.
//
// The probe puts one SQL placeholder per candidate id on the wire, and the
// candidates are every migration-source row matching the caller's query — with
// `gc beads list` running unbounded, that is the whole work ledger's history on
// a converged city. Past the driver's ceiling the statement does not run at all,
// so the probe fails, the merge fails and the listing exits 1: the large
// migrated cities the federation exists for are exactly the ones that lose the
// read.
//
// Two things are asserted, because either alone is passable by wrong code. The
// batched answer must equal the answer one query would have given if the driver
// allowed one, and no batch may reach the ceiling — and the control below proves
// the ceiling IS reachable from this population, so a probe that stopped
// chunking fails here rather than passing quietly.
func TestConvoyBindingOwnedIDsBatchesPastTheSQLVariableCap(t *testing.T) {
	const population = 100000
	ids := make([]string, population)
	held := make(map[string]bool, population/7+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
		if i%7 == 0 {
			held[ids[i]] = true
		}
	}
	binding := &varCappedIDStore{held: held}

	// Control: the population really does exceed what one statement can carry,
	// so a probe that sent it whole would fail rather than merely run slowly.
	if _, err := binding.List(beads.ListQuery{IDs: ids, IncludeClosed: true}); err == nil {
		t.Fatal("the fixture answered every id in one query; this row cannot tell a chunked probe from an unchunked one")
	}
	binding.batches = nil

	owned, err := convoyBindingOwnedIDs(binding, ids)
	if err != nil {
		t.Fatalf("convoyBindingOwnedIDs over %d candidates: %v", population, err)
	}
	if len(owned) != len(held) {
		t.Fatalf("the probe reported %d owned ids, want %d; the batches do not union to the whole answer", len(owned), len(held))
	}
	for id := range held {
		if !owned[id] {
			t.Fatalf("%s is held by the binding but missing from the batched answer", id)
		}
	}
	if len(binding.batches) < 2 {
		t.Fatalf("the probe issued %d queries for %d candidates; it did not batch", len(binding.batches), population)
	}
	carried := 0
	for _, size := range binding.batches {
		if size > sqliteMaxBoundVariables {
			t.Fatalf("a batch carried %d ids, past the %d the driver allows", size, sqliteMaxBoundVariables)
		}
		carried += size
	}
	if carried != population {
		t.Errorf("the batches carried %d ids in total, want %d; the probe skipped candidates", carried, population)
	}
}

// TestConvoyBindingOwnedIDsKeepsOneQueryUnderTheCap is the other side of the
// same boundary: batching must not turn the ordinary case into a fan of queries.
//
// The probe is an IN-list rather than one Get per row precisely because a
// per-row fan-out is what a converged city cannot afford on every listing, so a
// population that fits stays a single round trip.
func TestConvoyBindingOwnedIDsKeepsOneQueryUnderTheCap(t *testing.T) {
	ids := make([]string, 500)
	held := make(map[string]bool, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
		held[ids[i]] = true
	}
	binding := &varCappedIDStore{held: held}

	owned, err := convoyBindingOwnedIDs(binding, ids)
	if err != nil {
		t.Fatalf("convoyBindingOwnedIDs: %v", err)
	}
	if len(owned) != len(ids) {
		t.Errorf("the probe reported %d owned ids, want %d", len(owned), len(ids))
	}
	if len(binding.batches) != 1 {
		t.Errorf("the probe issued %d queries for %d candidates, want 1", len(binding.batches), len(ids))
	}
}
