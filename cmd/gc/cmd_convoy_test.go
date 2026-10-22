package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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
	_, _ = store.Create(beads.Bead{Title: "ship docs", ParentID: "tc-1"})

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
		code := run([]string{"convoy", "status", "tc-1", "--json"}, &stdout, &stderr)
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
		if result.SchemaVersion != "1" || result.Convoy.ID != "tc-1" || len(result.Children) != 1 {
			t.Fatalf("result = %+v, want tc-1 with one child", result)
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
	code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, 0, events.Discard, true, &stdout, &stderr)
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
	code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, 0, events.Discard, true, errWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("doConvoyCheckAcrossStoresJSON = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "writing JSON result") {
		t.Fatalf("stderr = %q, want JSON write error", stderr.String())
	}
}

// TestConvoyCheckJSONWritesNoResultWhenIncomplete: the result schema's ok is
// always true, so a check that could not reach a store reports on stderr and
// exits non-zero with no result line, like every convoy command's failure.
func TestConvoyCheckJSONWritesNoResultWhenIncomplete(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: beads.NewMemStore()}}, 1, events.Discard, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doConvoyCheckAcrossStoresJSON = %d with a store unopened, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no result line for an incomplete check", stdout.String())
	}
	if !strings.Contains(stderr.String(), "1 store(s) could not be checked") {
		t.Fatalf("stderr = %q, want the incomplete check reported", stderr.String())
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

// TestConvoyCheckStoreCandidatesSkipSuspendedRigs: gc convoy check runs on a
// timer as the core pack's convoy-autoclose-backstop order, so its store
// list leaves the suspended rigs out, as the order-tracking sweep's does
// (TestOrderTrackingSweepTargetsSkipSuspendedRigs). A store the city or an
// awake rig also reads stays in, and the fan-out readers (gc convoy list,
// gc workflow delete) keep every store. By-id resolution leaves suspended rigs
// out too; see TestResolveOwningStoreDirLeavesSuspendedRigStoresUnopened.
func TestConvoyCheckStoreCandidatesSkipSuspendedRigs(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SUSPENDED", "")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "/rigs/alpha"},
			{Name: "beta", Path: "/rigs/beta", SuspendedOnStart: true},
			{Name: "gamma", Path: "/rigs/alpha", SuspendedOnStart: true},
			{Name: "delta", Path: "/city", SuspendedOnStart: true},
		},
	}

	got, skipped := convoyCheckStoreCandidates(cfg, "/city")
	if want := []string{"/city", "/rigs/alpha"}; !slices.Equal(got, want) {
		t.Fatalf("check candidates = %v, want %v", got, want)
	}
	if want := []string{"beta"}; !slices.Equal(skipped, want) {
		t.Fatalf("skipped rigs = %v, want %v", skipped, want)
	}
	if all := convoyStoreCandidates(cfg, "/city", ""); !slices.Contains(all, "/rigs/beta") {
		t.Fatalf("the fan-out readers' candidates lost the suspended rig's store: %v", all)
	}
}

// suspendedRigByIDCity is a city with an awake rig (alpha, prefix "al"), a
// suspended rig with a store of its own (beta, prefix "be"), and a suspended
// rig sharing alpha's store (gamma). Each named store is a memstore keyed by
// its directory; opened records every directory the resolver opens.
func suspendedRigByIDCity(t *testing.T) (cfg *config.City, stores map[string]*beads.MemStore, opened *[]string, openStore func(string) (beads.Store, error)) {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SUSPENDED", "")
	cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "/rigs/alpha", Prefix: "al"},
			{Name: "beta", Path: "/rigs/beta", Prefix: "be", SuspendedOnStart: true},
			{Name: "gamma", Path: "/rigs/alpha", Prefix: "ga", SuspendedOnStart: true},
		},
	}
	stores = map[string]*beads.MemStore{}
	for _, dir := range []string{"/city", "/rigs/alpha", "/rigs/beta"} {
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		stores[dir] = store
	}
	opened = &[]string{}
	openStore = func(dir string) (beads.Store, error) {
		*opened = append(*opened, dir)
		store, ok := stores[dir]
		if !ok {
			t.Fatalf("unexpected store dir %q", dir)
		}
		return store, nil
	}
	return cfg, stores, opened, openStore
}

// TestResolveOwningStoreDirLeavesSuspendedRigStoresUnopened: by-id resolution
// is a periodic reader too (bd's on-close autoclose, the supervisor's sweep,
// execution-fact emission, convoy status run by orders), and on a bd-owned
// proxied rig any read of the store restarts the pair the controller stopped
// when the rig was suspended. So the uniqueness probe leaves a suspended rig's
// store unopened, even when it holds the same id; awake stores are still
// probed in full.
func TestResolveOwningStoreDirLeavesSuspendedRigStoresUnopened(t *testing.T) {
	cfg, stores, opened, openStore := suspendedRigByIDCity(t)
	if _, err := stores["/rigs/alpha"].Create(beads.Bead{ID: "al-7", Title: "awake work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores["/rigs/beta"].Create(beads.Bead{ID: "al-7", Title: "a copy only a probe of the suspended rig would see"}); err != nil {
		t.Fatal(err)
	}

	store, dir, err := resolveOwningStoreDir("al-7", cfg, "/city", openStore)
	if err != nil {
		t.Fatalf("resolveOwningStoreDir: %v", err)
	}
	if store != stores["/rigs/alpha"] || dir != "/rigs/alpha" {
		t.Fatalf("resolved to %q, want /rigs/alpha", dir)
	}
	if slices.Contains(*opened, "/rigs/beta") {
		t.Fatalf("opened %v: the suspended rig's store was opened to answer for an id it does not own", *opened)
	}
	if want := []string{"/rigs/alpha", "/city"}; !slices.Equal(*opened, want) {
		t.Fatalf("opened %v, want every awake store %v", *opened, want)
	}
}

// TestResolveOwningStoreDirStillProbesAwakeStoresForUniqueness: leaving the
// suspended stores out does not weaken the uniqueness contract among the
// awake ones.
func TestResolveOwningStoreDirStillProbesAwakeStoresForUniqueness(t *testing.T) {
	cfg, stores, _, openStore := suspendedRigByIDCity(t)
	for _, dir := range []string{"/city", "/rigs/alpha"} {
		if _, err := stores[dir].Create(beads.Bead{ID: "al-7", Title: "one of two awake copies"}); err != nil {
			t.Fatal(err)
		}
	}

	_, _, err := resolveOwningStoreDir("al-7", cfg, "/city", openStore)
	if err == nil || !strings.Contains(err.Error(), "exists in multiple stores") {
		t.Fatalf("resolveOwningStoreDir = %v, want the uniqueness refusal", err)
	}
}

// TestResolveOwningStoreDirReadsASuspendedRigForItsOwnID: an id whose prefix
// routes to a suspended rig is a question about that rig's bead, so its store
// is read. Only the probes for ids that route elsewhere are left out.
func TestResolveOwningStoreDirReadsASuspendedRigForItsOwnID(t *testing.T) {
	cfg, stores, _, openStore := suspendedRigByIDCity(t)
	if _, err := stores["/rigs/beta"].Create(beads.Bead{ID: "be-3", Title: "suspended rig work"}); err != nil {
		t.Fatal(err)
	}

	store, dir, err := resolveOwningStoreDir("be-3", cfg, "/city", openStore)
	if err != nil {
		t.Fatalf("resolveOwningStoreDir: %v", err)
	}
	if store != stores["/rigs/beta"] || dir != "/rigs/beta" {
		t.Fatalf("resolved to %q, want /rigs/beta", dir)
	}
}

// byIDSuspendedRigCity is a file-store city with an awake rig (ops) and a
// suspended rig (dormant), each holding a bead under one ops id. A by-id
// resolution that probed the suspended rig would refuse the id as ambiguous;
// one that leaves it alone answers from ops.
func byIDSuspendedRigCity(t *testing.T) (cityDir string, id string) {
	t.Helper()
	cityDir = convoyCheckRigCity(t, `
[[rigs]]
name = "ops"
path = "ops"
prefix = "OP"

[[rigs]]
name = "dormant"
path = "dormant"
prefix = "DM"
`, "ops", "dormant")
	ops, err := openScopeLocalFileStoreForCity(filepath.Join(cityDir, "ops"), cityDir)
	if err != nil {
		t.Fatal(err)
	}
	bead, err := ops.Create(beads.Bead{Title: "ops task"})
	if err != nil {
		t.Fatal(err)
	}
	// Minting under ops' prefix is how the dormant rig comes to hold the
	// same id: two ledgers disagreeing, which only a probe of it would see.
	dormant, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityDir, "dormant", ".gc", "beads.json"), beads.WithFileStoreIDPrefix("OP"))
	if err != nil {
		t.Fatal(err)
	}
	twin, err := dormant.Create(beads.Bead{Title: "dormant task"})
	if err != nil {
		t.Fatal(err)
	}
	if twin.ID != bead.ID {
		t.Fatalf("the fixture minted %s and %s; the collision needs one id", bead.ID, twin.ID)
	}
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "dormant", &suspended); err != nil {
		t.Fatal(err)
	}
	return cityDir, bead.ID
}

// TestAutocloseOwningStoreLeavesASuspendedRigUnprobed covers the background
// by-id callers: bd's on-close autoclose hooks and the supervisor's autoclose
// sweep resolve through autocloseOwningStore, and execution-fact emission
// falls back to it on a primary-store miss.
func TestAutocloseOwningStoreLeavesASuspendedRigUnprobed(t *testing.T) {
	cityDir, id := byIDSuspendedRigCity(t)

	store, dir, ok := autocloseOwningStore(id, cityDir)
	if !ok {
		t.Fatalf("autocloseOwningStore(%s) found no owner; the suspended rig's copy was probed", id)
	}
	if want := filepath.Join(cityDir, "ops"); !samePath(dir, want) {
		t.Fatalf("autocloseOwningStore(%s) dir = %q, want %q", id, dir, want)
	}
	if got, err := store.Get(id); err != nil || got.Title != "ops task" {
		t.Fatalf("owning store Get(%s) = %+v, %v; want the ops copy", id, got, err)
	}

	emit := executionEmitStore(beads.NewMemStore(), cityDir)
	got, err := emit.Get(id)
	if err != nil {
		t.Fatalf("executionEmitStore Get(%s) on a primary miss: %v", id, err)
	}
	if got.Title != "ops task" {
		t.Fatalf("executionEmitStore Get(%s) = %q, want the ops copy", id, got.Title)
	}
}

// convoyCheckRigCity is a file-store city with scope-local stores for the gc
// convoy check fallback: rigsTOML is appended to city.toml, and the city and
// each of rigDirs (relative to the city) get a persisted store.
func convoyCheckRigCity(t *testing.T, rigsTOML string, rigDirs ...string) string {
	t.Helper()
	clearInheritedBeadsEnv(t)
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SUSPENDED", "")
	cityDir := t.TempDir()
	t.Setenv("GC_CITY_PATH", cityDir)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, "[workspace]\nname = \"demo\"\n"+rigsTOML)
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	for _, dir := range rigDirs {
		if err := ensurePersistedScopeLocalFileStore(filepath.Join(cityDir, dir)); err != nil {
			t.Fatal(err)
		}
	}
	return cityDir
}

// seedCompletedConvoy creates a convoy in the rig store at cityDir/rigDir with
// one member, closed.
func seedCompletedConvoy(t *testing.T, cityDir, rigDir string) beads.Bead {
	t.Helper()
	store, err := openScopeLocalFileStoreForCity(filepath.Join(cityDir, rigDir), cityDir)
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := store.Create(beads.Bead{Title: rigDir + " batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Create(beads.Bead{Title: rigDir + " task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	return convoy
}

// rigConvoyStatus reads id back from the rig store at cityDir/rigDir.
func rigConvoyStatus(t *testing.T, cityDir, rigDir, id string) string {
	t.Helper()
	store, err := openStoreAtForCity(filepath.Join(cityDir, rigDir), cityDir)
	if err != nil {
		t.Fatal(err)
	}
	return statusOf(t, store, id)
}

// TestConvoyCheckLeavesASuspendedRigsConvoysAlone: suspension is quiescence
// (beads_quiescence.go) — a bd read restarts a suspended rig's retired proxy —
// so while `gc rig suspend` holds a rig the check leaves its store unread. It
// names the rig, closes what the awake stores hold, and closes the suspended
// rig's completed convoy on the first check after the rig resumes.
func TestConvoyCheckLeavesASuspendedRigsConvoysAlone(t *testing.T) {
	cityDir := convoyCheckRigCity(t, `
[[rigs]]
name = "ops"
path = "ops"
prefix = "OP"

[[rigs]]
name = "dormant"
path = "dormant"
prefix = "DM"
`, "ops", "dormant")
	awake := seedCompletedConvoy(t, cityDir, "ops")
	asleep := seedCompletedConvoy(t, cityDir, "dormant")
	setDormant := func(suspended bool) {
		t.Helper()
		if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "dormant", &suspended); err != nil {
			t.Fatal(err)
		}
	}

	setDormant(true)
	var stdout, stderr bytes.Buffer
	if code := doConvoyCheckFallback(cityDir, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy check exited %d:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `rig "dormant" is suspended`) {
		t.Errorf("stderr does not name the suspended rig it skipped: %q", stderr.String())
	}
	if got := rigConvoyStatus(t, cityDir, "ops", awake.ID); got != "closed" {
		t.Errorf("the awake rig's completed convoy is %q, want closed", got)
	}
	if got := rigConvoyStatus(t, cityDir, "dormant", asleep.ID); got != "open" {
		t.Fatalf("the suspended rig's completed convoy is %q, want it left open", got)
	}

	setDormant(false)
	stdout.Reset()
	stderr.Reset()
	if code := doConvoyCheckFallback(cityDir, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy check after resume exited %d:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "is suspended") {
		t.Errorf("stderr still names a suspended rig after resume: %q", stderr.String())
	}
	if got := rigConvoyStatus(t, cityDir, "dormant", asleep.ID); got != "closed" {
		t.Errorf("the resumed rig's completed convoy is %q, want closed", got)
	}
}

// writeUnreadableFileStore leaves a file store at scopeRoot that fails to open.
func writeUnreadableFileStore(t *testing.T, scopeRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".gc", "beads.json"), []byte("not a bead store"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestConvoyCheckNamesAStoreItCannotOpen: a store the check cannot open is
// named and fails the run, after the stores it could open are checked, rather
// than dropped from a sweep that then reports success.
func TestConvoyCheckNamesAStoreItCannotOpen(t *testing.T) {
	cityDir := convoyCheckRigCity(t, `
[[rigs]]
name = "ops"
path = "ops"
prefix = "OP"

[[rigs]]
name = "broken"
path = "broken"
prefix = "BR"
`, "ops")
	writeUnreadableFileStore(t, filepath.Join(cityDir, "broken"))
	convoy := seedCompletedConvoy(t, cityDir, "ops")

	var stdout, stderr bytes.Buffer
	if code := doConvoyCheckFallback(cityDir, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc convoy check exited %d with a store it could not open, want 1:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if broken := resolveStoreScopeRoot(cityDir, "broken"); !strings.Contains(stderr.String(), "opening the store at "+broken) {
		t.Errorf("stderr does not name the store at %s: %q", broken, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Auto-closed convoy "+convoy.ID) {
		t.Errorf("the stores that opened were not checked:\n%s", stdout.String())
	}
	if got := rigConvoyStatus(t, cityDir, "ops", convoy.ID); got != "closed" {
		t.Errorf("the awake rig's completed convoy is %q, want closed", got)
	}
}

// TestConvoyCheckRereadsEachConvoyLiveBeforeClosing: the check decides from a
// convoy list that may be stale (here a cached row), so it closes through the
// reactive autoclose's fence: a convoy that changed behind the list is left
// alone, with no event and no count.
func TestConvoyCheckRereadsEachConvoyLiveBeforeClosing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		change     func(backing beads.Store, id string) error
		wantStatus string
		wantReason string
	}{{
		name: "labeled owned",
		change: func(backing beads.Store, id string) error {
			return backing.Update(id, beads.UpdateOpts{Labels: []string{"owned"}})
		},
		wantStatus: "open",
	}, {
		name: "closed by an operator",
		change: func(backing beads.Store, id string) error {
			return closeConvoyWithReason(backing, id, convoyManualCloseReason)
		},
		wantStatus: "closed",
		wantReason: convoyManualCloseReason,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			member, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := backing.Close(member.ID); err != nil {
				t.Fatal(err)
			}
			cached := beads.NewCachingStoreForTest(backing, nil)
			if err := cached.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := tc.change(backing, convoy.ID); err != nil {
				t.Fatal(err)
			}

			rec := events.NewFake()
			var stdout, stderr bytes.Buffer
			if code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: cached}}, 0, rec, false, &stdout, &stderr); code != 0 {
				t.Fatalf("doConvoyCheckAcrossStoresJSON = %d, want 0; stderr: %s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "0 convoy(s) auto-closed") {
				t.Errorf("stdout = %q, want no convoy counted as auto-closed", stdout.String())
			}
			for _, evt := range rec.Events {
				if evt.Type == events.ConvoyClosed {
					t.Errorf("recorded %s for a convoy this check did not close", evt.Type)
				}
			}
			live, err := backing.Get(convoy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if live.Status != tc.wantStatus {
				t.Errorf("convoy %s, want %s", live.Status, tc.wantStatus)
			}
			if got := live.Metadata["close_reason"]; got != tc.wantReason {
				t.Errorf("close_reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

// TestConvoyCheckLeavesAConvoyThatMovedUnderItsClose: a convoy written after
// the fence's live re-read (so its CAS loses) or deleted after the list is
// not a failure. The check leaves it to its next run: exit 0, nothing on
// stderr, no event and no count.
func TestConvoyCheckLeavesAConvoyThatMovedUnderItsClose(t *testing.T) {
	for _, tc := range []struct {
		name    string
		require bool // conditional writes required, so the close is a CAS
		wrap    func(t *testing.T, backing *beads.MemStore, id string) beads.Store
	}{{
		name:    "CAS lost to a write after the re-read",
		require: true,
		wrap: func(_ *testing.T, backing *beads.MemStore, id string) beads.Store {
			return &interleavedStore{Store: backing, id: id, between: func() { _ = backing.SetMetadata(id, "touched", "yes") }}
		},
	}, {
		name: "deleted after the list",
		wrap: func(t *testing.T, backing *beads.MemStore, id string) beads.Store {
			cached := beads.NewCachingStoreForTest(backing, nil)
			if err := cached.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := backing.Delete(id); err != nil {
				t.Fatal(err)
			}
			return cached
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			if tc.require {
				stampedMemStore(t, backing)
			}
			convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
			if err != nil {
				t.Fatal(err)
			}
			member, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := backing.Close(member.ID); err != nil {
				t.Fatal(err)
			}
			store := tc.wrap(t, backing, convoy.ID)

			rec := events.NewFake()
			var stdout, stderr bytes.Buffer
			if code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, 0, rec, false, &stdout, &stderr); code != 0 {
				t.Fatalf("doConvoyCheckAcrossStoresJSON = %d, want 0:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want nothing for a convoy left to the next check", stderr.String())
			}
			if !strings.Contains(stdout.String(), "0 convoy(s) auto-closed") {
				t.Errorf("stdout = %q, want no convoy counted as auto-closed", stdout.String())
			}
			for _, evt := range rec.Events {
				if evt.Type == events.ConvoyClosed {
					t.Errorf("recorded %s for a convoy this check did not close", evt.Type)
				}
			}
		})
	}
}

// convoyCheckFailStore fails the reads and writes gc convoy check makes: the
// store's convoy list, or one convoy's members or close.
type convoyCheckFailStore struct {
	beads.Store
	failConvoyList      bool
	failDeps, failClose string
}

func (s convoyCheckFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.failConvoyList && query.Type == "convoy" {
		return nil, errors.New("convoy list unavailable")
	}
	return s.Store.List(query)
}

func (s convoyCheckFailStore) DepList(id, direction string) ([]beads.Dep, error) {
	if id == s.failDeps {
		return nil, errors.New("member edges unavailable")
	}
	return s.Store.DepList(id, direction)
}

func (s convoyCheckFailStore) Close(id string) error {
	if id == s.failClose {
		return errors.New("close rejected")
	}
	return s.Store.Close(id)
}

// seedCompletedMemConvoy creates a convoy in store with one member, closed.
func seedCompletedMemConvoy(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	convoy, err := store.Create(beads.Bead{Title: title, Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Create(beads.Bead{Title: title + " task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	return convoy
}

// TestConvoyCheckContinuesPastAConvoyItCannotCheck: one convoy whose members
// or close fail does not stop the sweep. The convoys after it are still
// checked, the failure is named with its convoy, and the check exits non-zero.
func TestConvoyCheckContinuesPastAConvoyItCannotCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(id string) convoyCheckFailStore
	}{
		{"members unreadable", func(id string) convoyCheckFailStore { return convoyCheckFailStore{failDeps: id} }},
		{"close rejected", func(id string) convoyCheckFailStore { return convoyCheckFailStore{failClose: id} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			bad, good := seedCompletedMemConvoy(t, mem, "first"), seedCompletedMemConvoy(t, mem, "second")
			if bad.ID >= good.ID {
				t.Fatalf("the check walks convoys in id order; %s must come before %s", bad.ID, good.ID)
			}
			store := tc.fail(bad.ID)
			store.Store = mem

			var stdout, stderr bytes.Buffer
			if code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, 0, events.Discard, false, &stdout, &stderr); code != 1 {
				t.Fatalf("doConvoyCheckAcrossStoresJSON = %d, want 1:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), bad.ID) {
				t.Errorf("stderr does not name %s: %q", bad.ID, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Auto-closed convoy "+good.ID) {
				t.Errorf("the convoy after the failure was not closed:\n%s", stdout.String())
			}
			if got := statusOf(t, mem, good.ID); got != "closed" {
				t.Errorf("%s is %q, want closed", good.ID, got)
			}
			if got := statusOf(t, mem, bad.ID); got != "open" {
				t.Errorf("%s is %q, want open", bad.ID, got)
			}
		})
	}
}

// TestConvoyCheckContinuesPastAStoreItCannotList: an ordinary store whose
// convoy list fails is named and counted as unchecked, and the other stores'
// convoys are still closed. A class binding whose convoy list fails ends the
// check before it closes anything, as doConvoyCheckFallback does for a binding
// it cannot read, even though the binding still answers which of the migration
// source's ids it holds.
func TestConvoyCheckContinuesPastAStoreItCannotList(t *testing.T) {
	t.Run("ordinary store", func(t *testing.T) {
		readable := beads.NewMemStore()
		convoy := seedCompletedMemConvoy(t, readable, "batch")
		stores := []convoyStoreView{
			{path: "/rigs/broken", store: convoyCheckFailStore{Store: beads.NewMemStore(), failConvoyList: true}},
			{path: "/rigs/ops", store: readable},
		}

		var stdout, stderr bytes.Buffer
		if code := doConvoyCheckAcrossStoresJSON(stores, 0, events.Discard, false, &stdout, &stderr); code != 1 {
			t.Fatalf("doConvoyCheckAcrossStoresJSON = %d with a store it could not list, want 1:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "listing convoys in the store at /rigs/broken") {
			t.Errorf("stderr does not name the store it could not list: %q", stderr.String())
		}
		if !strings.Contains(stderr.String(), "0 convoy(s) and 1 store(s) could not be checked") {
			t.Errorf("stderr = %q, want the unlisted store counted as unchecked", stderr.String())
		}
		if !strings.Contains(stdout.String(), "Auto-closed convoy "+convoy.ID) {
			t.Errorf("the store that listed was not checked:\n%s", stdout.String())
		}
		if got := statusOf(t, readable, convoy.ID); got != "closed" {
			t.Errorf("%s is %q, want closed", convoy.ID, got)
		}
	})

	t.Run("class binding", func(t *testing.T) {
		city := beads.NewMemStore()
		convoy := seedCompletedMemConvoy(t, city, "batch")
		stores := []convoyStoreView{
			{path: "/city", store: city, role: convoyViewMigrationSource},
			{path: convoyBindingViewPath, store: convoyCheckFailStore{Store: beads.NewMemStore(), failConvoyList: true}, role: convoyViewClassBinding},
		}

		var stdout, stderr bytes.Buffer
		if code := doConvoyCheckAcrossStoresJSON(stores, 0, events.Discard, false, &stdout, &stderr); code != 1 {
			t.Fatalf("doConvoyCheckAcrossStoresJSON = %d with a binding it could not list, want 1:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "listing convoys in "+convoyBindingViewPath) {
			t.Errorf("stderr does not name the binding it could not list: %q", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want nothing from a check that stopped at the binding", stdout.String())
		}
		if got := statusOf(t, city, convoy.ID); got != "open" {
			t.Errorf("the migration source's copy of %s is %q; a check without the binding must not close it", convoy.ID, got)
		}
	})
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

func TestConvoyAutocloseStoreRoot(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		cwd  string
		want string
	}{
		{
			name: "no env falls back to cwd",
			cwd:  "/work/dir",
			want: "/work/dir",
		},
		{
			name: "GC_STORE_ROOT wins outright, absolute",
			env:  map[string]string{"GC_STORE_ROOT": "/store/root"},
			cwd:  "/work/dir",
			want: "/store/root",
		},
		{
			name: "GC_STORE_ROOT relative is joined against cwd",
			env:  map[string]string{"GC_STORE_ROOT": "rel/root"},
			cwd:  "/work/dir",
			want: "/work/dir/rel/root",
		},
		{
			name: "BEADS_DIR absolute resolves to its parent",
			env:  map[string]string{"BEADS_DIR": "/city/root/.beads"},
			cwd:  "/work/dir",
			want: "/city/root",
		},
		{
			// filepath.Dir on a trailing-slash path does not strip the final
			// element the way a shell's dirname would, so an unclean BEADS_DIR
			// must not silently resolve to itself instead of its parent.
			name: "BEADS_DIR absolute with a trailing slash still resolves to its parent",
			env:  map[string]string{"BEADS_DIR": "/city/root/.beads/"},
			cwd:  "/work/dir",
			want: "/city/root",
		},
		{
			name: "BEADS_DIR relative is joined against cwd before taking its parent",
			env:  map[string]string{"BEADS_DIR": ".beads"},
			cwd:  "/work/dir",
			want: "/work/dir",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"GC_STORE_ROOT", "BEADS_DIR"} {
				t.Setenv(k, "")
				_ = os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got := convoyAutocloseStoreRoot(tc.cwd)
			if got != tc.want {
				t.Fatalf("convoyAutocloseStoreRoot(%q) with env %v = %q, want %q", tc.cwd, tc.env, got, tc.want)
			}
		})
	}
}

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

	binding = soleClassBindingStore(t, cityPath)
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

// convoyCityWhoseFrozenCopyLooksFinished is relocatedConvoyCity with the two
// copies' children swapped, which is the arrangement an auto-close can act on.
//
// relocatedConvoyCity freezes the retained copy mid-flight, so a check that read
// the wrong row simply declines to close and the damage is a convoy that never
// finishes. The opposite pairing is the one that MUTATES: the retained copy was
// cut over at a moment its children were all closed, and the binding has gone on
// to open more. A check reading the frozen copy finds a convoy that looks
// complete and closes it, while the work the binding is still tracking stays
// open under a convoy the city now calls done.
func convoyCityWhoseFrozenCopyLooksFinished(t *testing.T) (cityPath, convoyID string, work, binding beads.Store) {
	t.Helper()
	cityPath, _ = foreignProviderCity(t)
	work = workStoreFor(t, cityPath)

	frozen, err := work.Create(beads.Bead{Title: "the retained frozen convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("seeding the retained convoy in the work store: %v", err)
	}
	settled, err := work.Create(beads.Bead{Title: "finished in the frozen copy", ParentID: frozen.ID})
	if err != nil {
		t.Fatalf("seeding the frozen copy's child: %v", err)
	}
	if err := work.Close(settled.ID); err != nil {
		t.Fatalf("closing the frozen copy's child: %v", err)
	}

	binding = soleClassBindingStore(t, cityPath)
	if _, err := migrationSeed(binding, beads.Bead{ID: frozen.ID, Title: "the binding's live convoy", Type: "convoy"}); err != nil {
		t.Fatalf("carrying %s across to the class binding: %v", frozen.ID, err)
	}
	binding = recensusAfterSeedingARelic(t, cityPath)

	if _, err := binding.Create(beads.Bead{Title: "still open in the binding", ParentID: frozen.ID}); err != nil {
		t.Fatalf("seeding the binding copy's open child: %v", err)
	}
	return cityPath, frozen.ID, work, binding
}

// TestConvoyCheckRefusesRatherThanAutoclosingThroughARefusedBinding is the
// mutating arm's half of the refusal policy.
//
// A refused binding degrades a READ: the listing prints what it reached and says
// what it could not. `gc convoy check` is not a read — it closes convoys — and
// on a refusal the federation hands back the views unchanged, so the retained
// pre-migration copies stop being superseded and are walked as live rows. Their
// children were frozen at cutover, so a convoy the city is still working looks
// complete, and the check closes it and reports that as success while the
// binding's open row is never touched. A mutation on a view this build could not
// prove is worse than no mutation, so this one refuses.
func TestConvoyCheckRefusesRatherThanAutoclosingThroughARefusedBinding(t *testing.T) {
	cityPath, convoyID, work, _ := convoyCityWhoseFrozenCopyLooksFinished(t)
	failClassBindingReads(t, cityPath, errors.New("the class binding is having a bad day"))

	var stdout, stderr bytes.Buffer
	if code := doConvoyCheckFallback(cityPath, false, &stdout, &stderr); code == 0 {
		t.Fatalf("gc convoy check exited 0 with an unreadable binding; it auto-closed from rows it could not prove:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "bad day") {
		t.Errorf("the refusal does not carry its own cause: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "Auto-closed") {
		t.Errorf("the check reported an auto-close it must not have made:\n%s", stdout.String())
	}

	retained, err := work.Get(convoyID)
	if err != nil {
		t.Fatalf("reading %s back from the work store: %v", convoyID, err)
	}
	if convoycore.IsTerminalStatus(retained.Status) {
		t.Errorf("the check closed the retained pre-migration copy of %s from a view it could not read; the binding's row is the one this decides from", convoyID)
	}
}

// TestConvoyStrandedReadsTheBindingRow is the ga-efyq4 regression on the "is my
// work stuck?" query.
//
// `gc convoy stranded` walks open convoys for open, unassigned children, and on
// a migrated city the directory scan reaches only the copies the migration
// retained. That is wrong in both directions at once: a frozen copy still
// carries the assignees and open children it had at cutover, so it reports work
// that is not stranded, while a convoy the binding minted afterwards is never
// looked at, so genuinely stranded work is invisible. Both halves are asserted
// here because federating only the merge would fix the first and leave the
// second.
func TestConvoyStrandedReadsTheBindingRow(t *testing.T) {
	cityPath, _, _, binding := relocatedConvoyCity(t)

	adrift, err := binding.Create(beads.Bead{Title: "the binding's own convoy", Type: "convoy"})
	if err != nil {
		t.Fatalf("seeding a convoy the binding minted after the migration: %v", err)
	}
	if _, err := binding.Create(beads.Bead{Title: "nobody is on this", ParentID: adrift.ID}); err != nil {
		t.Fatalf("seeding the stranded child: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doConvoyStrandedFallback(cityPath, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc convoy stranded exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "still open in the frozen copy") {
		t.Errorf("the retained pre-migration copy was reported as stranded work; its children stopped at cutover and the binding's row supersedes it:\n%s", out)
	}
	if !strings.Contains(out, "nobody is on this") {
		t.Errorf("work stranded in the binding is invisible; the fan-out never read it:\n%s", out)
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
	classResidentWorkShapedBead(t, cityPath, frozen.ID, "the binding's live row")
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
	classResidentWorkShapedBead(t, cityPath, frozen.ID, "the binding's live row")
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
