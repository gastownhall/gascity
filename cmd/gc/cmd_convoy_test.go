package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
	_, _ = store.Create(beads.Bead{Title: "fix auth"})    // gc-1
	_, _ = store.Create(beads.Bead{Title: "fix logging"}) // gc-2

	var stdout, stderr bytes.Buffer
	code := doConvoyCreate(store, events.Discard, []string{"security fixes", "gc-1", "gc-2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCreate = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "tracking 2 issue(s)") {
		t.Errorf("stdout = %q, want tracking count", stdout.String())
	}

	// Verify issues have convoy as parent.
	for _, id := range []string{"gc-1", "gc-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.ParentID != "gc-3" {
			t.Errorf("bead %s ParentID = %q, want %q", id, b.ParentID, "gc-3")
		}
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
	code := doConvoyCreateWithOptions(cityStore, nil, "", events.Discard,
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
	code = doConvoyCreateWithOptions(cityStore, nil, "", events.Discard,
		[]string{"same-store batch", child3.ID, child4.ID}, convoyCreateOptions{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("same-store convoy failed: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "tracking 2 issue") {
		t.Errorf("stdout = %q, want tracking 2 issues", stdout.String())
	}

	// Verify children have parent set.
	got3, _ := cityStore.Get(child3.ID)
	got4, _ := cityStore.Get(child4.ID)
	convoyID := got3.ParentID
	if convoyID == "" {
		t.Fatal("child3 has no parent")
	}
	if got4.ParentID != convoyID {
		t.Errorf("child4 parent = %q, want %q", got4.ParentID, convoyID)
	}
	convoy, _ := cityStore.Get(convoyID)
	if convoy.Type != "convoy" {
		t.Errorf("convoy type = %q, want convoy", convoy.Type)
	}
}

// TestConvoyCreateRigChildrenShareStore is a regression test: when children
// have a rig prefix, the convoy must be created in the same store as the
// children (not the city root store). Otherwise bd update --parent fails
// because the parent bead doesn't exist in the child's database.
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
	code := doConvoyCreateWithOptions(store, nil, "", events.Discard,
		[]string{"Hello World Variants", c1.ID, c2.ID, c3.ID}, convoyCreateOptions{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("convoy create failed: %s", stderr.String())
	}

	// All children must have parent set to the convoy.
	got1, _ := store.Get(c1.ID)
	got2, _ := store.Get(c2.ID)
	got3, _ := store.Get(c3.ID)
	convoyID := got1.ParentID
	if convoyID == "" {
		t.Fatal("child1 has no parent — convoy not linked")
	}
	if got2.ParentID != convoyID || got3.ParentID != convoyID {
		t.Errorf("children have different parents: %q, %q, %q", got1.ParentID, got2.ParentID, got3.ParentID)
	}

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

	// Verify the convoy is expandable (Children returns all 3).
	children, err := store.Children(convoyID)
	if err != nil {
		t.Fatalf("listing children: %v", err)
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
	code := doConvoyListAcrossStores([]convoyStoreView{{store: cityStore}, {store: rigStore}}, &stdout, &stderr)
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
	_, _ = store.Create(beads.Bead{Title: "batch", Type: "convoy"}) // gc-1
	_, _ = store.Create(beads.Bead{Title: "task A"})                // gc-2

	var stdout, stderr bytes.Buffer
	code := doConvoyAdd(store, []string{"gc-1", "gc-2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Added gc-2 to convoy gc-1") {
		t.Errorf("stdout = %q, want add confirmation", stdout.String())
	}

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.ParentID != "gc-1" {
		t.Errorf("bead ParentID = %q, want %q", b.ParentID, "gc-1")
	}
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
	code := doConvoyCreateWithOptions(store, nil, "", events.Discard, []string{"deploy"}, convoyCreateOptions{Fields: fields}, &stdout, &stderr)
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
	code := doConvoyCreateWithOptions(store, nil, "", events.Discard, []string{"deploy"}, opts, &stdout, &stderr)
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
