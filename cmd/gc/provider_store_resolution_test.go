package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func writeProviderAwareTestCity(t *testing.T, cityDir, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func chdirProviderAwareTest(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRigAwareStoreUsesScopeLocalFileStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
[[rigs]]
name = "frontend"
path = "frontend"
prefix = "FE"
`)
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	cityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cityStore.Create(beads.Bead{Title: "city-only", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}
	rigStore, err := openScopeLocalFileStore(rigDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rigStore.Create(beads.Bead{Title: "rig-only", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	chdirProviderAwareTest(t, cityDir)

	store, code := openRigAwareStore([]string{"FE-42"}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("openRigAwareStore code = %d, want 0", code)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Title != "rig-only" {
		t.Fatalf("openRigAwareStore() opened wrong store: %#v", list)
	}
}

func TestServiceRuntimeBeadStoreUsesProviderAwareRigStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatal(err)
	}
	rigStore, err := openScopeLocalFileStore(rigDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rigStore.Create(beads.Bead{Title: "rig-only", Type: "task"}); err != nil {
		t.Fatal(err)
	}

	rt := &serviceRuntime{cr: &CityRuntime{cityPath: cityDir, cfg: &config.City{Rigs: []config.Rig{{Name: "frontend", Path: "frontend"}}}}}
	store := rt.BeadStore("frontend")
	if store == nil {
		t.Fatal("BeadStore(frontend) = nil")
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Title != "rig-only" {
		t.Fatalf("serviceRuntime.BeadStore(frontend) opened wrong store: %#v", list)
	}
}

func TestCmdOrderHistoryUsesProviderAwareCityStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, "orders", "digest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "digest", "order.toml"), []byte(`[order]
formula = "mol-digest"
trigger = "manual"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Create(beads.Bead{Title: "digest run", Type: "task", Labels: []string{"order-run:digest"}})
	if err != nil {
		t.Fatal(err)
	}
	chdirProviderAwareTest(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdOrderHistory("digest", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderHistory = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), run.ID) {
		t.Fatalf("stdout missing persisted order run %q:\n%s", run.ID, stdout.String())
	}
}

func TestConvoyStoreCandidatesUseRigStoresForScopedFileProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityDir, "frontend")
	cfg := &config.City{Rigs: []config.Rig{{Name: "frontend", Path: rigDir, Prefix: "FE"}}}

	got := convoyStoreCandidates(cfg, cityDir, "FE-42")
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

func TestDoConvoyAutocloseUsesProviderAwareStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
`)
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := store.Create(beads.Bead{Title: "deploy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "task", Type: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatal(err)
	}
	chdirProviderAwareTest(t, cityDir)

	var stdout, stderr bytes.Buffer
	doConvoyAutoclose(child.ID, &stdout, &stderr)

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := reloaded.Get(convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "closed" {
		t.Fatalf("convoy status = %q, want closed", updated.Status)
	}
	if !strings.Contains(stdout.String(), "Auto-closed convoy") {
		t.Fatalf("stdout = %q, want autoclose message", stdout.String())
	}
}

func TestCmdOrderRunExecSkipsStoreOpenForScopedFileProvider(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, "orders", "poll"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "poll", "order.toml"), []byte(`[order]
exec = "printf 'exec ok\\n'"
trigger = "manual"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	chdirProviderAwareTest(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdOrderRun("poll", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderRun(exec) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "exec ok") {
		t.Fatalf("stdout = %q, want exec output", stdout.String())
	}
}

func TestCmdOrderRunFormulaUsesProviderAwareCityStore(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, "orders", "digest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "digest", "order.toml"), []byte(`[order]
formula = "mol-digest"
trigger = "manual"
pool = "dog"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "formulas"), 0o755); err != nil {
		t.Fatal(err)
	}
	formulaText, err := os.ReadFile(filepath.Join(sharedTestFormulaDir, "mol-digest.formula.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "formulas", "mol-digest.formula.toml"), formulaText, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	chdirProviderAwareTest(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdOrderRun("digest", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderRun(formula) = %d, want 0; stderr: %s", code, stderr.String())
	}

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	results, err := reloaded.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatalf("ListByLabel(order-run:digest): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ListByLabel(order-run:digest) len = %d, want 1 (%#v)", len(results), results)
	}
	if got := results[0].Metadata["gc.routed_to"]; got != "dog" {
		t.Fatalf("gc.routed_to = %q, want dog", got)
	}
}

func TestDoConvoyAutocloseUsesBeadsDirStoreRoot(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
`)
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := store.Create(beads.Bead{Title: "deploy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "task", Type: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	chdirProviderAwareTest(t, outsideDir)
	t.Setenv("BEADS_DIR", filepath.Join(cityDir, ".beads"))

	var stdout, stderr bytes.Buffer
	doConvoyAutoclose(child.ID, &stdout, &stderr)

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := reloaded.Get(convoy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "closed" {
		t.Fatalf("convoy status = %q, want closed", updated.Status)
	}
	if !strings.Contains(stdout.String(), "Auto-closed convoy") {
		t.Fatalf("stdout = %q, want autoclose message", stdout.String())
	}
	eventsData, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	if !strings.Contains(string(eventsData), convoy.ID) {
		t.Fatalf("events.jsonl missing convoy id %q:\n%s", convoy.ID, string(eventsData))
	}
}

func TestDoWispAutocloseUsesBeadsDirStoreRoot(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "demo"
`)
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Create(beads.Bead{Title: "work item"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	chdirProviderAwareTest(t, outsideDir)
	t.Setenv("BEADS_DIR", filepath.Join(cityDir, ".beads"))

	var stdout, stderr bytes.Buffer
	doWispAutoclose(parent.ID, &stdout, &stderr)

	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := reloaded.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "closed" {
		t.Fatalf("molecule status = %q, want closed", updated.Status)
	}
	if !strings.Contains(stdout.String(), "Auto-closed molecule") {
		t.Fatalf("stdout = %q, want autoclose message", stdout.String())
	}
}
