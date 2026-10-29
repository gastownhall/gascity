package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// rigImportFixture is a split city whose binding has converged (an empty
// city work store, so the copy carried nothing), with rigs whose stores are
// real file stores on disk.
type rigImportFixture struct {
	t        *testing.T
	request  storageOperatorRequest
	cfg      *config.City
	target   infraBindingTarget
	rigPaths map[string]string
	// open overrides the store a rig opens as (an unreachable rig, a store of
	// another kind); nil means the rig's file store.
	open map[string]func() (beads.Store, error)
	// fixed serves a rig from a store the test holds (an in-memory, native
	// Dolt or bd store) instead of its file store.
	fixed    map[string]beads.Store
	hashConf map[string]*fakeRigHashConfig
	// otherCities are the other registered cities the shared-store check
	// loads.
	otherCities []string
}

// fakeRigHashConfig records the bd config writes the id reservation makes.
type fakeRigHashConfig struct {
	values map[string]string
	sets   []string
}

func (f *fakeRigHashConfig) ConfigGet(key string) (string, error) { return f.values[key], nil }
func (f *fakeRigHashConfig) ConfigSet(key, value string) error {
	f.values[key] = value
	f.sets = append(f.sets, key+"="+value)
	return nil
}

func newRigImportFixture(t *testing.T, rigs ...config.Rig) *rigImportFixture {
	t.Helper()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture city did not converge: %s: %s", got.Outcome, log.String())
	}
	f := &rigImportFixture{
		t:        t,
		request:  request,
		cfg:      cfg,
		target:   mustResolveInfraTarget(t, request.CityPath, cfg),
		rigPaths: map[string]string{},
		open:     map[string]func() (beads.Store, error){},
		fixed:    map[string]beads.Store{},
		hashConf: map[string]*fakeRigHashConfig{},
	}
	for _, rig := range rigs {
		f.addRig(rig)
	}
	prevOpen := openStorageScopeStore
	openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
		for name, path := range f.rigPaths {
			if samePath(storePath, path) {
				if override := f.open[name]; override != nil {
					return override()
				}
				if store := f.fixed[name]; store != nil {
					return store, nil
				}
				return f.openFileRig(name)
			}
		}
		return prevOpen(storePath, cityPath)
	}
	t.Cleanup(func() { openStorageScopeStore = prevOpen })
	prevOthers := rigImportOtherCities
	rigImportOtherCities = func(string) ([]string, error) { return f.otherCities, nil }
	t.Cleanup(func() { rigImportOtherCities = prevOthers })
	prevHash := rigImportHashConfigOpener
	rigImportHashConfigOpener = func(_ string, _ *config.City, rig config.Rig) (rigHashLengthConfig, error) {
		conf := f.hashConf[rig.Name]
		if conf == nil {
			conf = &fakeRigHashConfig{values: map[string]string{}}
			f.hashConf[rig.Name] = conf
		}
		return conf, nil
	}
	t.Cleanup(func() { rigImportHashConfigOpener = prevHash })
	return f
}

func (f *rigImportFixture) addRig(rig config.Rig) {
	f.t.Helper()
	if rig.Path == "" {
		rig.Path = f.t.TempDir()
	}
	f.rigPaths[rig.Name] = rig.Path
	f.cfg.Rigs = append(f.cfg.Rigs, rig)
}

func (f *rigImportFixture) rigFile(name string) string {
	return filepath.Join(f.rigPaths[name], ".gc", "beads.json")
}

// openFileRig opens the rig's real file store, as a fresh handle each time.
func (f *rigImportFixture) openFileRig(name string) (beads.Store, error) {
	prefix := ""
	for _, r := range f.cfg.Rigs {
		if r.Name == name {
			prefix = r.Prefix
		}
	}
	store, err := beads.OpenFileStore(fsys.OSFS{}, f.rigFile(name), beads.WithFileStoreIDPrefix(prefix))
	if err != nil {
		return nil, err
	}
	store.HonorExplicitIDs = true
	return store, nil
}

func (f *rigImportFixture) rig(name string) beads.Store {
	f.t.Helper()
	store, err := f.openFileRig(name)
	if err != nil {
		f.t.Fatalf("opening rig %s: %v", name, err)
	}
	return store
}

// seed writes rows and edges into the rig's file store exactly as given
// (status, timestamps and all), the way a pre-S4 build left them.
func (f *rigImportFixture) seed(name string, rows []beads.Bead, deps []beads.Dep) {
	f.t.Helper()
	store := f.rig(name)
	for _, b := range rows {
		if b.CreatedAt.IsZero() {
			b.CreatedAt = time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
		}
		want := b
		created, err := store.Create(b)
		if err != nil {
			f.t.Fatalf("seeding %s: %v", b.ID, err)
		}
		if want.Status == "closed" {
			if err := store.Close(created.ID); err != nil {
				f.t.Fatalf("closing %s: %v", created.ID, err)
			}
		} else if want.Status != "" && want.Status != "open" {
			s := want.Status
			if err := store.Update(created.ID, beads.UpdateOpts{Status: &s}); err != nil {
				f.t.Fatalf("setting %s %s: %v", created.ID, s, err)
			}
		}
	}
	for _, d := range deps {
		if err := store.DepAdd(d.IssueID, d.DependsOnID, d.Type); err != nil {
			f.t.Fatalf("seeding dep %s -> %s: %v", d.IssueID, d.DependsOnID, err)
		}
	}
}

func (f *rigImportFixture) importRigs(rigs ...string) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageMigrateFromRigs(context.Background(), f.request, rigs, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (f *rigImportFixture) binding() beads.Store {
	f.t.Helper()
	store, err := openInfraBindingReadOnly(f.target)
	if err != nil {
		f.t.Fatalf("opening the binding: %v", err)
	}
	f.t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	return store
}

func (f *rigImportFixture) loc(rig string) rigImportLocation {
	return newRigImportLocation(f.target, f.request.CityPath, rig)
}

// relicFree asks the S4a predicate the readers ask.
func (f *rigImportFixture) relicFree(rig string) bool {
	routes := &storageRoutes{rigMarkers: &rigGraphMarkers{bindingRoot: f.target.Root, cityPath: f.request.CityPath, rigs: f.cfg.Rigs}}
	return routes.rigGraphRelicFree(rig)
}

func rigTestIDs(rows []beads.Bead) []string {
	out := make([]string, 0, len(rows))
	for _, b := range rows {
		out = append(out, b.ID)
	}
	sort.Strings(out)
	return out
}

func rigTestList(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	return rows
}

// graphRow is a pre-S4 rig graph row: a workflow step stamped as one.
func graphRow(id, root, status string, meta map[string]string) beads.Bead {
	m := beads.StringMap{"gc.kind": "task", beadmeta.RootBeadIDMetadataKey: root}
	for k, v := range meta {
		m[k] = v
	}
	return beads.Bead{ID: id, Title: "step " + id, Type: "task", Status: status, Metadata: m}
}

func workRow(id, status string) beads.Bead {
	return beads.Bead{ID: id, Title: "work " + id, Type: "task", Status: status}
}

// TestRigImportMovesGraphRowsKeepingIdsAndStampingScope is the happy path:
// every graph row of the rig, closed history and wisps included, moves into
// the binding under its own id, stamped as the rig's import and scoped to the
// rig; the rig keeps only its work; the per-rig records S4a's relic-free
// predicate reads are written, so readers stop reading the rig.
func TestRigImportMovesGraphRowsKeepingIdsAndStampingScope(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	root := graphRow("gp-root1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"})
	step := graphRow("gp-step1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	unscoped := graphRow("gp-step2", "gp-root1", "closed", nil) // no gc.root_store_ref
	wisp := graphRow("gp-wisp-a1", "gp-root1", "closed", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"})
	wisp.Ephemeral = true
	work := workRow("gp-w1", "open")
	f.seed("packs", []beads.Bead{root, step, unscoped, wisp, work}, []beads.Dep{
		{IssueID: "gp-step1", DependsOnID: "gp-root1", Type: "parent-child"},
		{IssueID: "gp-step2", DependsOnID: "gp-step1", Type: "blocks"},
	})
	if f.relicFree("packs") {
		t.Fatal("precondition: a rig awaiting import read as relic-free")
	}

	code, stdout, stderr := f.importRigs()
	if code != 0 {
		t.Fatalf("import = %d: %s %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "rig packs: imported 4 bead(s)") {
		t.Errorf("the report does not say what moved: %q", stdout)
	}
	binding := f.binding()
	for _, id := range []string{"gp-root1", "gp-step1", "gp-step2", "gp-wisp-a1"} {
		b, err := binding.Get(id)
		if err != nil {
			t.Fatalf("the binding does not hold %s under its own id: %v", id, err)
		}
		if got := b.Metadata[infraMigrationStampKey]; got != "rig:packs" {
			t.Errorf("%s stamp = %q, want rig:packs", id, got)
		}
		if got := b.Metadata[beadmeta.RootStoreRefMetadataKey]; got != "rig:packs" {
			t.Errorf("%s gc.root_store_ref = %q, want rig:packs (an unstamped binding row reads as city scope)", id, got)
		}
	}
	if w, err := binding.Get("gp-wisp-a1"); err == nil && !w.Ephemeral {
		t.Error("the wisp lost its tier in the crossing")
	}
	deps, err := binding.DepList("gp-step2", "down")
	if err != nil || len(deps) != 1 || deps[0].DependsOnID != "gp-step1" {
		t.Errorf("the internal edge was not copied: %v %v", deps, err)
	}
	if got := rigTestIDs(rigTestList(t, f.rig("packs"))); strings.Join(got, ",") != "gp-w1" {
		t.Errorf("the rig still holds %v, want only its work gp-w1", got)
	}
	loc := f.loc("packs")
	manifest, _, err := loc.readManifest()
	if err != nil || manifest == nil {
		t.Fatalf("no manifest: %v", err)
	}
	if manifest.Len() != 4 || !manifest.ScopeAnnotated["gp-step2"] || len(manifest.ScopeAnnotated) != 1 || manifest.Scope != "rig:packs" {
		t.Errorf("manifest = %d ids, annotated %v, scope %q; want 4, {gp-step2}, rig:packs", manifest.Len(), manifest.ScopeAnnotated, manifest.Scope)
	}
	var carriedRoot beads.Bead
	if entries, _, _ := readInfraRetainedBackupAt(loc.BackupPath()); len(entries) > 0 {
		for _, e := range entries {
			if e.Bead.ID == root.ID {
				carriedRoot = e.Bead
			}
		}
	}
	if !manifest.Carried(carriedRoot) {
		t.Error("the manifest does not record the root it carried")
	}
	reused := carriedRoot
	reused.CreatedAt = carriedRoot.CreatedAt.Add(48 * time.Hour)
	if manifest.Carried(reused) {
		t.Error("the manifest reads a later bead under a reused id as carried: it is not keyed by (id, created_at)")
	}
	note, found, err := readInfraClearedNoteAt(loc.NotePath())
	if err != nil || !found || !note.Complete {
		t.Errorf("cleared note = %+v found=%v err=%v, want complete", note, found, err)
	}
	backup, present, err := readInfraRetainedBackupAt(loc.BackupPath())
	if err != nil || !present || len(backup) != 4 {
		t.Errorf("backup = %d entries present=%v err=%v, want 4", len(backup), present, err)
	}
	if !f.relicFree("packs") {
		t.Error("an imported rig does not read as relic-free, so its readers keep reading it")
	}
}

// TestRigImportEdgePolicyMatrix pins every row of the boundary edge policy:
// what is copied, gated, dropped and recorded, and what refuses.
func TestRigImportEdgePolicyMatrix(t *testing.T) {
	type want struct {
		refuse   string // substring of the refusal; empty means it imports
		gates    int
		copied   bool   // the edge is in the binding afterwards
		record   string // action recorded on the moved row, if any
		recordOn string
	}
	cases := []struct {
		name string
		rows []beads.Bead
		deps []beads.Dep
		want want
	}{
		{
			"D->W blocks, W open, D open: gate",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "blocks"}},
			want{gates: 1, record: rigEdgeGated, recordOn: "gp-d1"},
		},
		{
			"D->W waits-for, W open, D open: refuse",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "waits-for"}},
			want{refuse: "encodes only blocks"},
		},
		{
			"D->W blocks, W closed: drop",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "closed")},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "blocks"}},
			want{record: rigEdgeDropped, recordOn: "gp-d1"},
		},
		{
			"D->W blocks, D closed: drop",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "closed", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "blocks"}},
			want{record: rigEdgeDropped, recordOn: "gp-d1"},
		},
		{
			"W->D blocks, both open: refuse",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-w1", DependsOnID: "gp-d1", Type: "blocks"}},
			want{refuse: "gc bd dep remove gp-w1 gp-d1"},
		},
		{
			"W->D blocks, W closed: drop",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "closed")},
			[]beads.Dep{{IssueID: "gp-w1", DependsOnID: "gp-d1", Type: "blocks"}},
			want{record: rigEdgeDropped, recordOn: "gp-d1"},
		},
		{
			"W->D tracks, both open: refuse",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-w1", DependsOnID: "gp-d1", Type: "tracks"}},
			want{refuse: "membership edge"},
		},
		{
			"D->W parent-child, both open: refuse",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "parent-child"}},
			want{refuse: "membership edge"},
		},
		{
			"W->D tracks, D closed: drop",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "closed", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-w1", DependsOnID: "gp-d1", Type: "tracks"}},
			want{record: rigEdgeDropped, recordOn: "gp-d1"},
		},
		{
			"D->W related: drop",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "related"}},
			want{record: rigEdgeDropped, recordOn: "gp-d1"},
		},
		{
			"D->external: copy",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil)},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "external:gh:1", Type: "blocks"}},
			want{copied: true, record: rigEdgeExternal, recordOn: "gp-d1"},
		},
		{
			"D->id no store holds: record, not copied",
			[]beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil)},
			[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "ga-elsewhere", Type: "discovered-from"}},
			want{record: rigEdgeDropped, recordOn: "gp-d1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
			f.seed("packs", tc.rows, tc.deps)
			before := rigTestIDs(rigTestList(t, f.rig("packs")))
			code, stdout, stderr := f.importRigs()
			if tc.want.refuse != "" {
				if code == 0 {
					t.Fatalf("an uncodable edge imported: %s", stdout)
				}
				if !strings.Contains(stderr, tc.want.refuse) || !strings.Contains(stderr, "gp-d1") {
					t.Errorf("the refusal does not name the bead and the fix %q: %s", tc.want.refuse, stderr)
				}
				if got := rigTestIDs(rigTestList(t, f.rig("packs"))); strings.Join(got, ",") != strings.Join(before, ",") {
					t.Errorf("a refused rig changed: %v -> %v", before, got)
				}
				if fileExists(f.loc("packs").MarkerPath()) {
					t.Error("a refused rig carries an import marker")
				}
				if n := len(rigTestList(t, f.binding())); n != 0 {
					t.Errorf("a refused import wrote %d binding row(s)", n)
				}
				return
			}
			if code != 0 {
				t.Fatalf("import = %d: %s %s", code, stdout, stderr)
			}
			binding := f.binding()
			gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(gates) != tc.want.gates {
				t.Fatalf("gates = %d, want %d", len(gates), tc.want.gates)
			}
			for _, g := range gates {
				if g.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] != "gp-d1" || g.Metadata[beadmeta.CrossStoreBlockerMetadataKey] != "gp-w1" || g.Metadata[infraMigrationStampKey] != "rig:packs" || !strings.Contains(g.ID, "-xsg-") {
					t.Errorf("gate %s = %+v, want owner gp-d1, blocker gp-w1, the import stamp and a pinned xsg id", g.ID, g.Metadata)
				}
				deps, _ := binding.DepList("gp-d1", "down")
				if len(deps) != 1 || deps[0].DependsOnID != g.ID || deps[0].Type != "blocks" {
					t.Errorf("gp-d1's edges = %v, want one blocks edge onto its gate %s", deps, g.ID)
				}
			}
			d, err := binding.Get("gp-d1")
			if err != nil {
				t.Fatal(err)
			}
			for _, dep := range tc.deps {
				got, _ := binding.DepList(dep.IssueID, "down")
				found := false
				for _, e := range got {
					found = found || e.DependsOnID == dep.DependsOnID
				}
				if found != tc.want.copied {
					t.Errorf("edge %s -> %s in the binding = %v, want %v", dep.IssueID, dep.DependsOnID, found, tc.want.copied)
				}
			}
			if tc.want.record != "" {
				var recs []rigCrossEdgeRecord
				if err := json.Unmarshal([]byte(d.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]), &recs); err != nil || len(recs) != 1 || recs[0].Action != tc.want.record {
					t.Errorf("recorded edges on %s = %q (%v), want one %s", tc.want.recordOn, d.Metadata[beadmeta.MigratedCrossEdgesMetadataKey], err, tc.want.record)
				}
			}
			assertNoCrossStoreEdge(t, binding)
		})
	}
}

// assertNoCrossStoreEdge fails on any binding edge whose target the binding
// does not hold (an external: reference is not a store's row).
func assertNoCrossStoreEdge(t *testing.T, binding beads.Store) {
	t.Helper()
	for _, b := range rigTestList(t, binding) {
		deps, err := binding.DepList(b.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range deps {
			if strings.HasPrefix(d.DependsOnID, "external:") {
				continue
			}
			if _, err := binding.Get(d.DependsOnID); err != nil {
				t.Errorf("cross-store edge written: %s -[%s]-> %s, which the binding does not hold", b.ID, d.Type, d.DependsOnID)
			}
		}
	}
}

// TestRigImportReleasesAndRetiresItsGatesThroughTheS3bLane runs the merged
// S3b release machinery over a gate the import minted: it holds its owner
// while the rig work is open, releases once the work closes, and retires once
// the owner closes.
func TestRigImportReleasesAndRetiresItsGatesThroughTheS3bLane(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-d1", "gp-d1", "open", nil), workRow("gp-w1", "open")},
		[]beads.Dep{{IssueID: "gp-d1", DependsOnID: "gp-w1", Type: "blocks"}})
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, stdout, stderr)
	}
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(binding) })
	rig := f.rig("packs")
	resolve := func(blockers []string) (dispatch.CrossStoreBlockerResolution, error) {
		found, err := beads.GetMany(rig, blockers)
		if err != nil {
			return dispatch.CrossStoreBlockerResolution{}, err
		}
		copies := map[string][]beads.Bead{}
		for id, b := range found {
			copies[id] = append(copies[id], b)
		}
		return dispatch.CrossStoreBlockerResolution{Copies: copies}, nil
	}
	gateOpen := func() bool {
		gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, Status: "open", TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			t.Fatal(err)
		}
		return len(gates) == 1
	}
	if _, err := dispatch.ReleaseCrossStoreGates(binding, resolve); err != nil {
		t.Fatal(err)
	}
	if !gateOpen() {
		t.Fatal("the gate released while its rig blocker is open")
	}
	if err := rig.Close("gp-w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatch.ReleaseCrossStoreGates(binding, resolve); err != nil {
		t.Fatal(err)
	}
	if gateOpen() {
		t.Fatal("the gate still holds its owner after the rig blocker closed")
	}
}

// TestRigImportResumesAfterACrashAtEveryStep kills the import at every step
// boundary and re-runs it: the end state is the clean run's, with no row
// twice, the backup marks a dropped boundary edge as dropped whichever run
// wrote it, and a pending clear holds boot until the re-run finishes it.
func TestRigImportResumesAfterACrashAtEveryStep(t *testing.T) {
	points := []string{"prepared", "copying", "rows", "verified", "manifest", "marker", "reserved", "backup", "pending", "deleting"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
			var rows []beads.Bead
			var deps []beads.Dep
			for i := 0; i < 300; i++ {
				id := fmt.Sprintf("gp-s%03d", i)
				rows = append(rows, graphRow(id, "gp-s000", "closed", nil))
				if i > 0 {
					deps = append(deps, beads.Dep{IssueID: id, DependsOnID: "gp-s000", Type: "parent-child"})
				}
			}
			rows = append(rows, workRow("gp-w1", "open"))
			deps = append(deps, beads.Dep{IssueID: "gp-s001", DependsOnID: "gp-w1", Type: "related"})
			f.seed("packs", rows, deps)

			crashed := false
			prev := rigImportCrashPoint
			rigImportCrashPoint = func(step string) error {
				if step == point && !crashed {
					crashed = true
					return errors.New("killed at " + step)
				}
				return nil
			}
			t.Cleanup(func() { rigImportCrashPoint = prev })
			if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "killed at "+point) {
				t.Fatalf("the crash at %s did not stop the import: %d %s", point, code, stderr)
			}
			loc := f.loc("packs")
			if note, found, _ := readInfraClearedNoteAt(loc.NotePath()); found && !note.Complete {
				var stdout, stderr bytes.Buffer
				if code := doStorageStatus(f.request, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "retained") {
					t.Errorf("status exits 0 over a pending rig clear: %s", stdout.String())
				}
				if err := rigBootRefusal(f.request.CityPath, f.target); err == nil || !strings.Contains(err.Error(), rigImportInstruction("packs")) {
					t.Errorf("boot does not refuse a pending rig clear naming the command: %v", err)
				}
			}
			if f.relicFree("packs") {
				t.Error("an interrupted import reads as relic-free")
			}
			if code, stdout, stderr := f.importRigs(); code != 0 {
				t.Fatalf("the re-run after a crash at %s = %d: %s %s", point, code, stdout, stderr)
			}
			binding := f.binding()
			got := rigTestList(t, binding)
			if len(got) != 300 {
				t.Errorf("binding holds %d rows after the resume, want 300", len(got))
			}
			if rest := rigTestIDs(rigTestList(t, f.rig("packs"))); strings.Join(rest, ",") != "gp-w1" {
				t.Errorf("the rig holds %d rows after the resume, want only gp-w1", len(rest))
			}
			deps2, _ := binding.DepList("gp-s299", "down")
			if len(deps2) != 1 {
				t.Errorf("gp-s299 edges after resume = %v", deps2)
			}
			backup, _, err := readInfraRetainedBackupAt(loc.BackupPath())
			if err != nil || len(backup) != 300 {
				t.Errorf("backup = %d entries (%v), want 300", len(backup), err)
			}
			disposition := "no backup edge"
			for _, e := range backup {
				for _, d := range e.Deps {
					if e.Bead.ID == "gp-s001" && d.DependsOnID == "gp-w1" {
						disposition = d.Disposition
					}
				}
			}
			if disposition != rigEdgeDropped {
				t.Errorf("the backup marks gp-s001's boundary edge to gp-w1 as %q, want %q (what the import did with it)", disposition, rigEdgeDropped)
			}
			if !f.relicFree("packs") {
				t.Error("a resumed import does not read as relic-free")
			}
			if err := rigBootRefusal(f.request.CityPath, f.target); err != nil {
				t.Errorf("boot refuses a finished import: %v", err)
			}
		})
	}
}

// TestRigImportIsIdempotent: a second run on an unchanged rig changes nothing.
func TestRigImportIsIdempotent(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), workRow("gp-w", "open")}, nil)
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("first import = %d: %s %s", code, stdout, stderr)
	}
	loc := f.loc("packs")
	snapshot := func() string {
		var b strings.Builder
		for _, p := range []string{loc.ManifestPath(), loc.MarkerPath(), loc.BackupPath(), loc.NotePath(), f.rigFile("packs")} {
			data, _ := os.ReadFile(p)
			b.Write(data)
		}
		b.WriteString(strings.Join(infraStoreFingerprint(t, f.binding()), ","))
		return b.String()
	}
	before := snapshot()
	code, stdout, stderr := f.importRigs()
	if code != 0 || !strings.Contains(stdout, "nothing to import") {
		t.Fatalf("the re-run = %d %q %q, want 0 and nothing to import", code, stdout, stderr)
	}
	if after := snapshot(); after != before {
		t.Error("an idempotent re-run changed a record, the rig or the binding")
	}
}

// TestRigImportCopiesRowsWrittenAfterTheClearIncrementally: a row a rig
// acquires after its import is stranded; status says so, and the re-run moves
// only it.
func TestRigImportCopiesRowsWrittenAfterTheClearIncrementally(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("first import = %d: %s %s", code, stdout, stderr)
	}
	f.seed("packs", []beads.Bead{graphRow("gp-late", "gp-a", "open", nil)}, nil)
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(f.request, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "stranded") || !strings.Contains(stdout.String(), "gp-late") {
		t.Fatalf("status = %d %q, want non-zero naming the stranded row", code, stdout.String())
	}
	code, out, errOut := f.importRigs()
	if code != 0 || !strings.Contains(out, "imported 1 bead(s)") {
		t.Fatalf("incremental import = %d %q %q", code, out, errOut)
	}
	manifest, _, _ := f.loc("packs").readManifest()
	if manifest == nil || manifest.Len() != 2 {
		t.Fatalf("manifest after the incremental import = %v, want both ids", manifest)
	}
	if len(rigTestList(t, f.rig("packs"))) != 0 {
		t.Error("the incremental import left the row in the rig")
	}
}

// TestRigImportRecordsACleanRig: a rig with only work gets a clean record and
// reads as relic-free.
func TestRigImportRecordsACleanRig(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "quiet", Prefix: "gq"})
	f.seed("quiet", []beads.Bead{workRow("gq-1", "open")}, nil)
	code, stdout, stderr := f.importRigs()
	if code != 0 || !strings.Contains(stdout, "recorded clean") {
		t.Fatalf("import = %d %q %q", code, stdout, stderr)
	}
	if !fileExists(f.loc("quiet").CleanPath()) || !f.relicFree("quiet") {
		t.Error("a clean rig is not recorded clean")
	}
}

// TestRigImportMovesASuspendedRigWithoutResumingIt: suspension is config the
// import never touches.
func TestRigImportMovesASuspendedRigWithoutResumingIt(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp", Suspended: true})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("import of a suspended rig = %d: %s %s", code, stdout, stderr)
	}
	if !f.cfg.Rigs[0].Suspended {
		t.Error("the import resumed the rig")
	}
	if _, err := f.binding().Get("gp-a"); err != nil {
		t.Errorf("the suspended rig's row did not move: %v", err)
	}
}

// TestRigImportUnreachableRigBlocksOnlyItself: one rig's open error refuses
// that rig by name; the others proceed.
func TestRigImportUnreachableRigBlocksOnlyItself(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "down", Prefix: "gd"}, config.Rig{Name: "packs", Prefix: "gp"})
	f.open["down"] = func() (beads.Store, error) { return nil, errors.New("dolt server unreachable") }
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	code, stdout, stderr := f.importRigs()
	if code == 0 {
		t.Fatal("an unreachable rig did not fail the run")
	}
	if !strings.Contains(stderr, "rig down: REFUSED") || !strings.Contains(stderr, "dolt server unreachable") {
		t.Errorf("the unreachable rig is not named with its error: %s", stderr)
	}
	if !strings.Contains(stdout, "rig packs: imported 1 bead(s)") {
		t.Errorf("the reachable rig did not import: %s", stdout)
	}
	if f.relicFree("down") {
		t.Error("the unreachable rig reads as relic-free")
	}
}

// TestRigImportRaisesTheRigsMinimumIdLength: the clear frees short ids; the
// rig's bd min_hash_length is raised past the longest imported hash so none
// is minted again, and never lowered.
func TestRigImportRaisesTheRigsMinimumIdLength(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "high", Prefix: "gh"})
	f.seed("packs", []beads.Bead{graphRow("gp-abc", "gp-abc", "closed", nil), graphRow("gp-k0nvi", "gp-abc", "closed", nil), graphRow("gp-wisp-zzzzzzz", "gp-abc", "closed", nil)}, nil)
	f.seed("high", []beads.Bead{graphRow("gh-ab", "gh-ab", "closed", nil)}, nil)
	f.hashConf["high"] = &fakeRigHashConfig{values: map[string]string{bdMinHashLengthKey: "7"}}
	if code, stdout, stderr := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, stdout, stderr)
	}
	if got := f.hashConf["packs"].values[bdMinHashLengthKey]; got != "8" {
		t.Errorf("packs min_hash_length = %q, want 8 (longest suffix, the wisp's zzzzzzz, + 1)", got)
	}
	if got := f.hashConf["packs"].values[bdMaxHashLengthKey]; got != "" {
		t.Errorf("packs max_hash_length = %q, want it untouched (bd's default 8 covers the floor)", got)
	}
	if sets := f.hashConf["high"].sets; len(sets) != 0 {
		t.Errorf("the import lowered or rewrote a higher min_hash_length: %v", sets)
	}
}

// TestRigImportRefusesAnIdTheBindingHoldsAsAnotherBead: a collision is
// refused by id, never silently skipped.
func TestRigImportRefusesAnIdTheBindingHoldsAsAnotherBead(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-dup", "gp-dup", "closed", nil)}, nil)
	binding, err := openInfraDestination(f.target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{ID: "gp-dup", Title: "another bead", Type: "session"}); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "gp-dup") || !strings.Contains(stderr, "different bead") {
		t.Fatalf("a collision was not refused by id: %d %s", code, stderr)
	}
}

// TestRigImportRefusesASingleStoreCityAndOpensNothing: no binding, nothing to
// import, no rig opened.
func TestRigImportRefusesASingleStoreCityAndOpensNothing(t *testing.T) {
	request := storageTestRequest(t, &config.City{Rigs: []config.Rig{{Name: "packs", Prefix: "gp", Path: t.TempDir()}}})
	prev := openStorageScopeStore
	openStorageScopeStore = func(string, string) (beads.Store, error) {
		t.Error("a single-store city opened a rig store")
		return beads.NewMemStore(), nil
	}
	t.Cleanup(func() { openStorageScopeStore = prev })
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrateFromRigs(context.Background(), request, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "nothing to import") {
		t.Fatalf("= %d %q", code, stderr.String())
	}
}

// TestRigImportRequiresAConvergedCityBinding: a rig import never creates or
// re-copies a binding.
func TestRigImportRequiresAConvergedCityBinding(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	cfg.Rigs = []config.Rig{{Name: "packs", Prefix: "gp", Path: t.TempDir()}}
	stubInfraControllerPing(t, 0)
	var stdout, stderr bytes.Buffer
	if code := doStorageMigrateFromRigs(context.Background(), request, nil, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), storageMigrationCommand) {
		t.Fatalf("= %d %q, want a refusal naming %s", code, stderr.String(), storageMigrationCommand)
	}
}

// TestRigImportRigFlagNarrowsTheRun: --rig imports only the rigs it names;
// the others stay awaiting import.
func TestRigImportRigFlagNarrowsTheRun(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "other", Prefix: "go"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	f.seed("other", []beads.Bead{graphRow("go-a", "go-a", "closed", nil)}, nil)
	code, stdout, stderr := f.importRigs("packs")
	if code != 0 || strings.Contains(stdout, "rig other") {
		t.Fatalf("--rig packs = %d %q %q", code, stdout, stderr)
	}
	if !f.relicFree("packs") || f.relicFree("other") {
		t.Error("--rig packs did not import exactly packs")
	}
	if code, _, stderr := f.importRigs("nosuch"); code == 0 || !strings.Contains(stderr, "no rig \"nosuch\"") {
		t.Errorf("an unknown --rig was not refused: %s", stderr)
	}
}

// TestStorageStatusReportsEachRigsState: status names every rig's state and
// exits non-zero while any rig awaits import; zero once each is imported or
// clean.
func TestStorageStatusReportsEachRigsState(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "quiet", Prefix: "gq"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil), graphRow("gp-b", "gp-a", "open", nil)}, nil)
	f.seed("quiet", []beads.Bead{workRow("gq-1", "open")}, nil)
	var stdout, stderr bytes.Buffer
	code := doStorageStatus(f.request, &stdout, &stderr)
	out := stdout.String()
	if code == 0 || !strings.Contains(out, "rig packs: awaiting import (2 infrastructure bead(s), 1 open)") || !strings.Contains(out, "rig quiet: awaiting import (0 infrastructure bead(s)") {
		t.Fatalf("status = %d, want non-zero naming both rigs awaiting import: %s", code, out)
	}
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	stdout.Reset()
	if code := doStorageStatus(f.request, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "rig packs: imported") || !strings.Contains(stdout.String(), "rig quiet: clean") {
		t.Fatalf("status after the import = %d: %s", code, stdout.String())
	}
}

// TestStorageStatusIsUnchangedOnASingleStoreCity: no binding, no rig lines,
// no rig store opened.
func TestStorageStatusIsUnchangedOnASingleStoreCity(t *testing.T) {
	request := storageTestRequest(t, &config.City{Rigs: []config.Rig{{Name: "packs", Prefix: "gp", Path: t.TempDir()}}})
	prev := openStorageScopeStore
	openStorageScopeStore = func(string, string) (beads.Store, error) {
		t.Error("status opened a rig store on a single-store city")
		return beads.NewMemStore(), nil
	}
	t.Cleanup(func() { openStorageScopeStore = prev })
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(request, &stdout, &stderr); code != 0 || strings.Contains(stdout.String(), "rigs:") {
		t.Errorf("single-store status = %d %q", code, stdout.String())
	}
}

// TestStorageRigsDoctorCheckNamesRigStateAndReusedIds: the doctor check warns
// while a rig awaits import, clears once it is imported, and names a rig row
// that reuses an id the import moved.
func TestStorageRigsDoctorCheckNamesRigStateAndReusedIds(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-abc", "gp-abc", "closed", nil)}, nil)
	check := newStorageRigsCheck(f.cfg, f.request.CityPath)
	if res := check.Run(nil); res.Status == doctor.StatusOK || !strings.Contains(strings.Join(res.Details, " "), "awaiting import") {
		t.Fatalf("doctor before the import = %+v", res)
	}
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("doctor after the import = %+v", res)
	}
	// The store stamps created_at itself, so the bead the import carried is
	// moved two days into the past in the manifest: a row the rig mints now
	// under the same id is then a different bead.
	loc := f.loc("packs")
	manifest, _, err := loc.readManifest()
	if err != nil || manifest == nil {
		t.Fatal(err)
	}
	manifest.Created["gp-abc"] = manifest.Created["gp-abc"].Add(-48 * time.Hour)
	if err := writeInfraCopyManifestFileAt(loc.BindingRoot, loc.ManifestPath(), rigTestKeys(manifest.IDs), manifest.Created, manifest.WrittenAt, manifest.LegacyBound, manifest.Generation, nil); err != nil {
		t.Fatal(err)
	}
	f.seed("packs", []beads.Bead{workRow("gp-abc", "open")}, nil)
	res := check.Run(nil)
	if res.Status == doctor.StatusOK || !strings.Contains(strings.Join(res.Details, " "), "reuse an id") {
		t.Errorf("doctor does not name the reused id: %+v", res)
	}
}

// TestControllerBootCensusEmitsRigResidue: the controller's one asynchronous
// census publishes storage.rig_residue for a rig awaiting import and skips a
// suspended rig without opening it.
func TestControllerBootCensusEmitsRigResidue(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"}, config.Rig{Name: "sleepy", Prefix: "gs", Suspended: true})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	f.open["sleepy"] = func() (beads.Store, error) {
		t.Error("the boot census opened a suspended rig")
		return beads.NewMemStore(), errors.New("suspended")
	}
	done := make(chan struct{})
	prev := rigResidueCensusDone
	rigResidueCensusDone = func() { close(done) }
	t.Cleanup(func() { rigResidueCensusDone = prev })
	rec := events.NewFake()
	var stderr bytes.Buffer
	cr := &CityRuntime{cfg: f.cfg, cityPath: f.request.CityPath, rec: rec, stderr: &stderr, logPrefix: "gc start"}
	cr.startRigResidueCensus(context.Background())
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the boot census did not finish")
	}
	var residue []string
	for _, e := range rec.Events {
		if e.Type == events.StorageRigResidue {
			residue = append(residue, e.Subject)
		}
	}
	if len(residue) != 1 || residue[0] != "packs" {
		t.Errorf("storage.rig_residue subjects = %v, want [packs]", residue)
	}
	if !strings.Contains(stderr.String(), "rig packs: awaiting import") || !strings.Contains(stderr.String(), "sleepy") {
		t.Errorf("the boot warning = %q", stderr.String())
	}
}

// TestRigAddRecordsANewRigCleanOnAConvergedSplitCity: a new, empty rig store
// needs no relic leg; an adopted one stays awaiting import.
func TestRigAddRecordsANewRigCleanOnAConvergedSplitCity(t *testing.T) {
	f := newRigImportFixture(t)
	var stderr bytes.Buffer
	recordNewRigClean(f.request.CityPath, f.cfg, config.Rig{Name: "fresh", Path: t.TempDir()}, false, &stderr)
	if !fileExists(f.loc("fresh").CleanPath()) {
		t.Errorf("a new rig was not recorded clean: %s", stderr.String())
	}
	recordNewRigClean(f.request.CityPath, f.cfg, config.Rig{Name: "adopted", Path: t.TempDir()}, true, &stderr)
	if fileExists(f.loc("adopted").CleanPath()) {
		t.Error("an adopted rig was recorded clean without a census")
	}
	single := &config.City{}
	recordNewRigClean(f.request.CityPath, single, config.Rig{Name: "plain", Path: t.TempDir()}, false, &stderr)
	if fileExists(f.loc("plain").CleanPath()) {
		t.Error("a single-store city wrote a rig record")
	}
}

// TestByIDRelicProofReadsTheRigManifestsToo: the id-level proof reads the
// union of the city manifest and every rig manifest.
func TestByIDRelicProofReadsTheRigManifestsToo(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	if manifest, read := migrationCopyManifest(f.request.CityPath, f.cfg); !read || manifest["gp-a"] {
		t.Fatalf("precondition: manifest = %v read=%v", manifest, read)
	}
	if code, o, e := f.importRigs(); code != 0 {
		t.Fatalf("import = %d: %s %s", code, o, e)
	}
	if manifest, read := migrationCopyManifest(f.request.CityPath, f.cfg); !read || !manifest["gp-a"] {
		t.Errorf("the by-id proof does not read the rig manifest: %v read=%v", manifest, read)
	}
}

func rigTestKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
