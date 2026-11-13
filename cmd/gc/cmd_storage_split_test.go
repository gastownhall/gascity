package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// guidedSplitTOML is an unsplit city.toml with comments the split must keep
// and one rig with a store of its own.
func guidedSplitTOML(rigPath string) string {
	return fmt.Sprintf("# an unsplit city\n[workspace]\nname = \"guided\" # keep this comment\n\n[[rigs]]\nname = \"packs\"\npath = %q\nprefix = \"gp\"\n\n# trailing comment\n", rigPath)
}

// guidedSplitCity is an unsplit, stopped city on a stubbed work store, its
// city.toml on disk, with one rig on a real file store holding closed graph
// rows: the state `gc storage split` starts from.
type guidedSplitCity struct {
	t        *testing.T
	request  storageOperatorRequest
	rigPath  string
	provider *migrationWindowProvider
	// demand counts P2's demand-repair runs.
	demand int
}

func newGuidedSplitCity(t *testing.T) *guidedSplitCity {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	cityPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// GC_BEADS=file names a file store for the city, whose exact snapshot
	// copies its file; its rows come from the stubbed work store.
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "beads.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &guidedSplitCity{t: t, rigPath: t.TempDir(), provider: &migrationWindowProvider{}}
	c.request.CityPath = cityPath
	c.writeTOML(guidedSplitTOML(c.rigPath))
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c.request = storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true, TimeBudget: storageDefaultTimeBudget}

	// The work store is the city's file store, opened afresh on every open
	// as the real opener does, so the reverse can write it back.
	prevSource := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return c.openWork() }
	t.Cleanup(func() { openInfraMigrationSource = prevSource })
	work := c.work()
	mustCreateInfraBead(t, work, beads.Bead{Title: "a session", Type: "session", Labels: []string{"gc:session"}})
	mustCreateInfraBead(t, work, beads.Bead{Title: "mail", Type: "message", Assignee: "mayor"})
	if _, err := work.Create(beads.Bead{Title: "work", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	rig := c.rig()
	for _, b := range []beads.Bead{
		graphRow("gp-root1", "gp-root1", "", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs", "gc.kind": "workflow"}),
		graphRow("gp-step1", "gp-root1", "", map[string]string{beadmeta.RootStoreRefMetadataKey: "rig:packs"}),
	} {
		created, err := rig.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.Close(created.ID); err != nil {
			t.Fatal(err)
		}
	}

	prevOpen := openStorageScopeStore
	openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
		if samePath(storePath, c.rigPath) {
			return c.openRig()
		}
		return prevOpen(storePath, cityPath)
	}
	t.Cleanup(func() { openStorageScopeStore = prevOpen })
	stubOtherCities(t)
	prevHash := rigImportHashConfigOpener
	rigImportHashConfigOpener = func(string, *config.City, config.Rig) (rigHashLengthConfig, error) {
		return &fakeRigHashConfig{values: map[string]string{}}, nil
	}
	t.Cleanup(func() { rigImportHashConfigOpener = prevHash })
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubMigrationWindowStartStop(t, c.provider)
	stubMigrationProviderDown(t, false)
	stubSplitPreflightLegs(t, nil)
	prevProbe := minGCVersionPathProbe
	minGCVersionPathProbe = func(*config.City) (string, string, error) { return "", "", nil }
	t.Cleanup(func() { minGCVersionPathProbe = prevProbe })
	prevDemand := splitDemandRepairs
	splitDemandRepairs = func(*config.City, string, io.Writer) { c.demand++ }
	t.Cleanup(func() { splitDemandRepairs = prevDemand })
	return c
}

func (c *guidedSplitCity) tomlPath() string { return filepath.Join(c.request.CityPath, "city.toml") }

func (c *guidedSplitCity) writeTOML(text string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.request.CityPath, "city.toml"), []byte(text), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func (c *guidedSplitCity) readTOML() string {
	c.t.Helper()
	data, err := os.ReadFile(c.tomlPath())
	if err != nil {
		c.t.Fatal(err)
	}
	return string(data)
}

func (c *guidedSplitCity) openWork() (beads.Store, error) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(c.request.CityPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix("gd"))
	if err != nil {
		return nil, err
	}
	store.HonorExplicitIDs = true
	return store, nil
}

// work is a fresh handle on the city's work store.
func (c *guidedSplitCity) work() beads.Store {
	c.t.Helper()
	store, err := c.openWork()
	if err != nil {
		c.t.Fatal(err)
	}
	return store
}

func (c *guidedSplitCity) openRig() (beads.Store, error) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(c.rigPath, ".gc", "beads.json"), beads.WithFileStoreIDPrefix("gp"))
	if err != nil {
		return nil, err
	}
	store.HonorExplicitIDs = true
	return store, nil
}

func (c *guidedSplitCity) rig() beads.Store {
	c.t.Helper()
	store, err := c.openRig()
	if err != nil {
		c.t.Fatal(err)
	}
	return store
}

// reload reads the city's config from disk, as the next command would.
func (c *guidedSplitCity) reload() storageOperatorRequest {
	c.t.Helper()
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(c.request.CityPath, io.Discard)
	if err != nil {
		c.t.Fatal(err)
	}
	request := c.request
	request.Cfg = cfg
	return request
}

func (c *guidedSplitCity) split(opts splitOptions) (int, string, string) {
	c.t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageSplit(context.Background(), c.reload(), opts, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func (c *guidedSplitCity) journal() storageSplitJournal {
	c.t.Helper()
	j, present, err := readStorageSplitJournal(c.request.CityPath)
	if err != nil || !present {
		c.t.Fatalf("the split journal: present %t, %v", present, err)
	}
	return j
}

func (c *guidedSplitCity) target() infraBindingTarget {
	c.t.Helper()
	return mustResolveInfraTarget(c.t, c.request.CityPath, splitHypotheticalConfig(c.request.Cfg))
}

// assertSplit is the invariants a finished split leaves (S5 §10.6).
func (c *guidedSplitCity) assertSplit() {
	c.t.Helper()
	request := c.reload()
	if !splitStorageMatches(request.Cfg) {
		c.t.Fatalf("city.toml after the split composes %+v, want the split's [storage]", request.Cfg.Storage)
	}
	target := c.target()
	if state, err := readInfraConvergenceState(target); err != nil || state != infraConvergenceMarked {
		c.t.Fatalf("the binding = %v, %v; want converged", state, err)
	}
	if _, recorded, err := readMarkerWorkStoreIdentity(target.MarkerPath()); err != nil || !recorded {
		c.t.Fatalf("the marker records no work-store identity: %v", err)
	}
	if floor, present, err := readMinGCVersion(c.request.CityPath); err != nil || !present || floor != splitMinGCVersion {
		c.t.Fatalf("the version floor = %q %t %v, want %s", floor, present, err, splitMinGCVersion)
	}
	var status bytes.Buffer
	if code := doStorageStatus(request, &status, &status); code != 0 {
		c.t.Fatalf("gc storage status after the split = %d:\n%s", code, status.String())
	}
	if n := len(rigTestList(c.t, c.rig())); n != 0 {
		c.t.Fatalf("the rig still holds %d graph row(s) after its import", n)
	}
	if j := c.journal(); j.Phase != splitPhaseComplete {
		c.t.Fatalf("the journal = %s, want complete", j.Phase)
	}
	if mode, _ := doctorReconcilerMode(request.Cfg, c.request.CityPath, nil); mode != reconcilerV2 {
		c.t.Fatalf("the split city's reconciler = %s, want v2 (the default for a split city)", mode)
	}
	if c.provider.starts != c.provider.stops {
		c.t.Fatalf("the window started the provider %d time(s) and stopped it %d", c.provider.starts, c.provider.stops)
	}
	if lock, err := acquireControllerLock(c.request.CityPath); err != nil {
		c.t.Fatalf("the split left the controller lock held: %v", err)
	} else {
		lock.Close() //nolint:errcheck // test cleanup
	}
}

// TestStorageSplitGuidedMovesAnUnsplitCityAndItsRigs is the whole guided
// split end to end (S5 §5.2): the city is split, its rig imported, its
// pre-split city.toml journalled, and every invariant holds.
func TestStorageSplitGuidedMovesAnUnsplitCityAndItsRigs(t *testing.T) {
	c := newGuidedSplitCity(t)
	before := c.readTOML()
	code, stdout, stderr := c.split(splitOptions{})
	if code != 0 {
		t.Fatalf("gc storage split = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
	if c.demand != 1 {
		t.Fatalf("the demand repairs ran %d time(s), want once (the split city runs v2)", c.demand)
	}
	after := c.readTOML()
	if !strings.HasPrefix(after, before) || !strings.Contains(after, "[storage.bindings.infra]") {
		t.Fatalf("city.toml after the split:\n%s\nwant the original bytes, then the [storage] block", after)
	}
	j := c.journal()
	if j.CityTOMLBefore == nil || *j.CityTOMLBefore != before || j.CityTOMLBeforeSHA256 != sha256Hex([]byte(before)) || j.CityTOMLAfterSHA256 != sha256Hex([]byte(after)) {
		t.Fatalf("the journal's city.toml record = %+v", j)
	}
	if j.Snapshot == "" || !fileExists(filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName)) {
		t.Fatalf("the split's snapshot %s is not pinned", j.Snapshot)
	}
	for _, want := range []string{"Split.", "gc start", storageReverseInstruction(reverseToWork, nil), j.Snapshot} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not name %q:\n%s", want, stdout)
		}
	}
}

// guidedSplitSeams are the seams of a guided split, in order (S5-H design
// §3.3), with what a boot between the kill and the next run does: serve
// unsplit before city.toml is written, refuse while the city's copy and
// clear are under way, and serve split once the copy has converged.
var guidedSplitSeams = []struct {
	seam string
	boot string // "serves" or the refusal's text
}{
	{"split guarded", "serves"},
	{"split " + splitPhaseSnapshotted, "serves"},
	{"split " + splitPhaseReadied, "serves"},
	{"split " + splitPhaseConfiguring, "serves"},
	{"split floor", "serves"},
	{"split city.toml", `assign graph/sessions/messaging/orders/nudges to binding "infra"`},
	{"split " + splitPhaseConfigured, `assign graph/sessions/messaging/orders/nudges to binding "infra"`},
	{"prepare", `assign graph/sessions/messaging/orders/nudges to binding "infra"`},
	{"copying rows", `assign graph/sessions/messaging/orders/nudges to binding "infra"`},
	{"verify", `assign graph/sessions/messaging/orders/nudges to binding "infra"`},
	{"manifest", `assign graph/sessions/messaging/orders/nudges to binding "infra"`},
	{"marker", "this city converged on binding"},
	{"clearing", "this city converged on binding"},
	{"floor", "this city converged on binding"},
	{"split " + splitPhaseCityConverged, "serves"},
	{"split " + splitPhaseRigsImported, "serves"},
	{"split " + splitPhaseComplete, "serves"},
}

// assertBoot checks what a boot of the city does now: serve, or refuse with
// want in its refusal.
func (c *guidedSplitCity) assertBoot(want string) {
	c.t.Helper()
	request := c.reload()
	_, err := storageBootGate(c.request.CityPath, request.Cfg, "gc start", nil, io.Discard)
	switch {
	case want == "serves" && err != nil:
		c.t.Fatalf("boot = %v, want it to serve", err)
	case want != "serves" && (err == nil || !strings.Contains(err.Error(), want)):
		c.t.Fatalf("boot = %v, want a refusal naming %q", err, want)
	}
}

// killSplitAt runs the split with the seam point failing once.
func (c *guidedSplitCity) killSplitAt(point string) {
	c.t.Helper()
	prev := infraEngineCrashPoint
	fired := false
	infraEngineCrashPoint = func(p string) error {
		if !fired && (p == point || strings.HasPrefix(p, point+" ")) {
			fired = true
			return errors.New("killed at " + p)
		}
		return nil
	}
	defer func() { infraEngineCrashPoint = prev }() // restored on a Fatal too
	code, stdout, stderr := c.split(splitOptions{})
	if !fired {
		c.t.Fatalf("the seam %q never fired (exit %d):\n%s%s", point, code, stdout, stderr)
	}
}

// TestStorageSplitResumesFromEverySeam (S125): a split killed at each seam
// finishes on the next run, with every invariant, and the journal's
// city.toml record is the one a single run writes.
func TestStorageSplitResumesFromEverySeam(t *testing.T) {
	for _, tc := range guidedSplitSeams {
		t.Run(tc.seam, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			before := c.readTOML()
			c.killSplitAt(tc.seam)
			c.assertBoot(tc.boot)
			code, stdout, stderr := c.split(splitOptions{})
			if code != 0 && tc.seam == "split "+splitPhaseComplete {
				// Killed after the journal said complete: the split is done,
				// and the rerun names the run that did it (§3.3, last row).
				if run := c.journal().Run; !strings.Contains(stderr, "the guided split "+run+" completed") {
					t.Fatalf("a rerun of a completed split = %d: %s%s", code, stdout, stderr)
				}
			} else if code != 0 {
				t.Fatalf("the rerun after a kill at %q = %d:\n%s\n%s", tc.seam, code, stdout, stderr)
			}
			c.assertSplit()
			j := c.journal()
			if *j.CityTOMLBefore != before || j.CityTOMLAfterSHA256 != sha256Hex([]byte(c.readTOML())) {
				t.Fatalf("the journal's city.toml record after a kill at %q does not match the file", tc.seam)
			}
		})
	}
}

// TestStorageSplitThenReverseRestoresTheBytes is the S5-G contract end to
// end: the split's journal is what the reverse restores city.toml from, byte
// for byte, CRLF and a missing final newline included, through a symlink.
func TestStorageSplitThenReverseRestoresTheBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mangle  func(string) string
		symlink bool
	}{
		{"as written", func(s string) string { return s }, false},
		{"CRLF, no final newline", func(s string) string {
			return strings.TrimRight(strings.ReplaceAll(s, "\n", "\r\n"), "\r\n")
		}, false},
		{"through a symlink", func(s string) string { return s }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			before := tc.mangle(guidedSplitTOML(c.rigPath))
			if tc.symlink {
				target := filepath.Join(t.TempDir(), "real-city.toml")
				if err := os.WriteFile(target, []byte(before), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(c.tomlPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, c.tomlPath()); err != nil {
					t.Fatal(err)
				}
			} else {
				c.writeTOML(before)
			}
			if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
				t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
			}
			if tc.symlink {
				if info, err := os.Lstat(c.tomlPath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("the split replaced the city.toml symlink: %v", err)
				}
			}
			var stdout, stderr bytes.Buffer
			if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code != 0 {
				t.Fatalf("--to-work after the split = %d:\n%s\n%s", code, stdout.String(), stderr.String())
			}
			if got := c.readTOML(); got != before {
				t.Fatalf("city.toml after split and reverse:\n%q\nwant the bytes from before the split:\n%q", got, before)
			}
			if !strings.Contains(stderr.String(), "restored to its bytes from before the split") {
				t.Errorf("the reverse did not say it restored the bytes: %s", stderr.String())
			}
		})
	}
}

// TestStorageSplitEditedTOMLReverseKeepsTheEdit: an edit after the split
// changes city.toml from what the split wrote, so the reverse removes
// [storage] in place and keeps the edit, rather than restoring bytes that
// would lose it.
func TestStorageSplitEditedTOMLReverseKeepsTheEdit(t *testing.T) {
	c := newGuidedSplitCity(t)
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
	}
	edited := strings.Replace(c.readTOML(), "# trailing comment", "# an operator's edit after the split", 1)
	c.writeTOML(edited)
	var stdout, stderr bytes.Buffer
	if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code != 0 {
		t.Fatalf("--to-work = %d:\n%s\n%s", code, stdout.String(), stderr.String())
	}
	got := c.readTOML()
	if !strings.Contains(got, "an operator's edit after the split") || strings.Contains(got, "[storage") {
		t.Fatalf("city.toml after the reverse:\n%s\nwant the edit kept and [storage] gone", got)
	}
}

// TestStorageSplitJournalSatisfiesTheReverseReader: the journal's keys are
// the ones S5-G's reader reads, and they prove.
func TestStorageSplitJournalSatisfiesTheReverseReader(t *testing.T) {
	c := newGuidedSplitCity(t)
	before := c.readTOML()
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
	}
	got, after, ok := readSplitJournalCityTOML(c.request.CityPath)
	if !ok || string(got) != before || after != sha256Hex([]byte(c.readTOML())) {
		t.Fatalf("readSplitJournalCityTOML = %q, %s, %t", got, after, ok)
	}
	var raw map[string]any
	data, err := os.ReadFile(storageSplitJournalPath(c.request.CityPath))
	if err != nil || json.Unmarshal(data, &raw) != nil {
		t.Fatalf("the journal does not decode: %v", err)
	}
	for _, key := range []string{"city_toml_before", "city_toml_before_sha256", "city_toml_after_sha256"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the journal carries no %s", key)
		}
	}
}

// TestStorageSplitJournalJudge is the judge's table (S5-H design §3.2).
func TestStorageSplitJournalJudge(t *testing.T) {
	at := func(phase string) *storageSplitJournal {
		return &storageSplitJournal{Run: "r1", Phase: phase, CityTOMLBeforeSHA256: "before", CityTOMLAfterSHA256: "after"}
	}
	for _, tc := range []struct {
		name string
		in   splitJudgement
		want splitDecision
		note string
		err  string
	}{
		{"no journal, unsplit", splitJudgement{}, splitFresh, "", ""},
		{"no journal, authored", splitJudgement{authored: true, splitting: true}, splitRefuse, "", "already authors [storage]"},
		{"complete, unsplit (reversed since)", splitJudgement{journal: at(splitPhaseComplete)}, splitFresh, "", ""},
		{"complete, authored", splitJudgement{journal: at(splitPhaseComplete), authored: true, splitting: true, sha: "after"}, splitRefuse, "", "the guided split r1 completed"},
		{"abandoned, authored", splitJudgement{journal: at(splitPhaseAbandoned), authored: true, splitting: true}, splitRefuse, "", "already authors [storage]"},
		{"abandoned, unsplit", splitJudgement{journal: at(splitPhaseAbandoned)}, splitFresh, "", ""},
		{"abandoning", splitJudgement{journal: at(splitPhaseAbandoning)}, splitRefuse, "", "--abandon"},
		{"unknown phase", splitJudgement{journal: at("later")}, splitRefuse, "", "does not know"},
		{"snapshotted, unsplit", splitJudgement{journal: at(splitPhaseSnapshotted), sha: "before"}, splitFresh, "starting it over", ""},
		{"readied, unsplit", splitJudgement{journal: at(splitPhaseReadied), sha: "before"}, splitFresh, "starting it over", ""},
		{"readied, authored by hand", splitJudgement{journal: at(splitPhaseReadied), authored: true, splitting: true}, splitRefuse, "", "--abandon"},
		{"configuring, file as before", splitJudgement{journal: at(splitPhaseConfiguring), sha: "before"}, splitFresh, "starting it over", ""},
		{"configuring, file written", splitJudgement{journal: at(splitPhaseConfiguring), authored: true, splitting: true, sha: "after"}, splitResume, "", ""},
		{"configuring, the split's storage edited", splitJudgement{journal: at(splitPhaseConfiguring), authored: true, splitting: true, sha: "other"}, splitRefuse, "", "neither the file the split recorded"},
		{"configuring, file edited, no storage", splitJudgement{journal: at(splitPhaseConfiguring), sha: "other"}, splitFresh, "starting a new split", ""},
		{"configured, as written", splitJudgement{journal: at(splitPhaseConfigured), authored: true, splitting: true, sha: "after"}, splitResume, "", ""},
		{"city converged, edited", splitJudgement{journal: at(splitPhaseCityConverged), authored: true, splitting: true, sha: "edited"}, splitResume, "in place and keeps the change", ""},
		{"rigs imported, as written", splitJudgement{journal: at(splitPhaseRigsImported), authored: true, splitting: true, sha: "after"}, splitResume, "", ""},
		{"configured, another storage", splitJudgement{journal: at(splitPhaseConfigured), authored: true, sha: "other"}, splitRefuse, "", "not the one the split"},
		{"city converged, undone", splitJudgement{journal: at(splitPhaseCityConverged), sha: "before"}, splitFresh, "starting a new split", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, note, err := judgeSplit(tc.in)
			if got != tc.want {
				t.Fatalf("decision = %d, want %d (%v)", got, tc.want, err)
			}
			if (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if (tc.note == "") != (note == "") || !strings.Contains(note, tc.note) {
				t.Fatalf("note = %q, want %q", note, tc.note)
			}
		})
	}
}

// TestStorageSplitWritesTheVersionFloor: the split writes 1.6.0, never lowers
// a higher floor, and keeps the earlier file for --abandon.
func TestStorageSplitWritesTheVersionFloor(t *testing.T) {
	t.Run("raises a lower floor", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		if err := os.WriteFile(minGCVersionPath(c.request.CityPath), []byte("1.5.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
			t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
		}
		if j := c.journal(); j.MinGCVersionBefore == nil || *j.MinGCVersionBefore != "1.5.1\n" || j.MinGCVersion != splitMinGCVersion {
			t.Fatalf("the journal's floor record = %+v", j)
		}
		c.assertSplit()
	})
	t.Run("keeps a higher floor", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		if err := os.WriteFile(minGCVersionPath(c.request.CityPath), []byte("9.0.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		prevVersion := version
		version = "9.1.0"
		t.Cleanup(func() { version = prevVersion })
		if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
			t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
		}
		if floor, _, _ := readMinGCVersion(c.request.CityPath); floor != "9.0.0" {
			t.Fatalf("the floor = %s, want 9.0.0 kept", floor)
		}
	})
}

// TestStorageSplitRefusesAFragmentStorage: a [storage] a fragment declares
// is the operator's to edit, so the split writes nothing.
func TestStorageSplitRefusesAFragmentStorage(t *testing.T) {
	c := newGuidedSplitCity(t)
	before := c.readTOML()
	after, err := splitCityTOMLAfter(c.request.CityPath, []byte(before+"[storage]\n"), c.request.Cfg)
	if err == nil {
		t.Fatalf("an edit over a declared [storage] was proven: %q", after)
	}
	// A fragment that declares [storage]: the operator's to edit, so the
	// split writes nothing.
	frag := filepath.Join(c.request.CityPath, "storage-frag.toml")
	if err := os.WriteFile(frag, []byte("[storage.classes]\nwork = \"work\"\ngraph = \"work\"\nsessions = \"work\"\nmessaging = \"work\"\norders = \"work\"\nnudges = \"work\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.writeTOML("include = [\"storage-frag.toml\"]\n" + before)
	code, stdout, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "already authors [storage]") {
		t.Fatalf("split over a fragment's [storage] = 0:\n%s", stdout)
	}
	if got := c.readTOML(); got != "include = [\"storage-frag.toml\"]\n"+before {
		t.Fatalf("the refused split changed city.toml: %s (%s)", got, stderr)
	}
	if _, present, _ := readStorageSplitJournal(c.request.CityPath); present {
		t.Fatal("the refused split wrote a journal")
	}
}

// TestStorageSplitRefusesTheExclusions: a held controller lock, a running
// session, an incomplete inventory (unless attested), a held guard and a
// missing attestation each refuse, and nothing is written.
func TestStorageSplitRefusesTheExclusions(t *testing.T) {
	assertNothing := func(t *testing.T, c *guidedSplitCity, before string) {
		t.Helper()
		if c.readTOML() != before {
			t.Fatal("a refused split changed city.toml")
		}
		if _, present, _ := readStorageSplitJournal(c.request.CityPath); present {
			t.Fatal("a refused split wrote a journal")
		}
	}
	t.Run("controller lock", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		held, err := acquireControllerLock(c.request.CityPath)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close() //nolint:errcheck // test cleanup
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "controller lock") {
			t.Fatalf("split under a held lock = %d: %s", code, stderr)
		}
		assertNothing(t, c, before)
	})
	t.Run("a running session", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		stubSessionInventory(t, []string{"mayor"}, true, nil)
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "mayor") {
			t.Fatalf("split with a session running = %d: %s", code, stderr)
		}
		assertNothing(t, c, before)
	})
	t.Run("incomplete inventory", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		stubSessionInventory(t, nil, false, nil)
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "--force=remote-runtime") {
			t.Fatalf("split over an incomplete inventory = %d: %s", code, stderr)
		}
		assertNothing(t, c, before)
		c.request.Force = storageForce{RemoteRuntime: true}
		if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
			t.Fatalf("split attested --force=remote-runtime = %d:\n%s\n%s", code, stdout, stderr)
		}
		if j := c.journal(); strings.Join(j.Forced, ",") != storageHazardRemoteRuntime {
			t.Fatalf("the journal records the attestations %v", j.Forced)
		}
	})
	t.Run("guard", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		guard, err := acquireStorageMigrationGuard(context.Background(), c.request.CityPath)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Release() //nolint:errcheck // test cleanup
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "another storage migration") {
			t.Fatalf("split under a held guard = %d: %s", code, stderr)
		}
		assertNothing(t, c, before)
	})
	t.Run("no attestation", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		c.request.FleetStopped = false
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "--fleet-stopped") {
			t.Fatalf("split without --fleet-stopped = %d: %s", code, stderr)
		}
		assertNothing(t, c, before)
	})
	t.Run("registered", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		before := c.readTOML()
		if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(c.request.CityPath, "guided"); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "registered with the supervisor") {
			t.Fatalf("split of a registered city = %d: %s", code, stderr)
		}
		assertNothing(t, c, before)
	})
}

// TestStorageSplitRefusesAJournalChangedUnderTheGuard: a journal another run
// moved while this one waited is refused, and this run writes nothing.
func TestStorageSplitRefusesAJournalChangedUnderTheGuard(t *testing.T) {
	c := newGuidedSplitCity(t)
	before := c.readTOML()
	prev := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prev })
	infraEngineCrashPoint = func(p string) error {
		if p == "split guarded" {
			return writeStorageSplitJournal(c.request.CityPath, storageSplitJournal{Format: storageSplitJournalFormat, Run: "other", Phase: splitPhaseSnapshotted})
		}
		return nil
	}
	if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "changed while this run waited") {
		t.Fatalf("split over a journal moved under the guard = %d: %s", code, stderr)
	}
	if c.readTOML() != before || c.journal().Run != "other" {
		t.Fatal("the refused split wrote something")
	}
}

// TestStorageSplitRemoteRuntimeNeedsTheAttestation (O1): a remote session
// runtime refuses without --force=remote-runtime; attested, the split runs,
// records the attestation, and doctor warns rather than errs.
func TestStorageSplitRemoteRuntimeNeedsTheAttestation(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.writeTOML(strings.Replace(c.readTOML(), "[[rigs]]", "[session]\nprovider = \"k8s\"\n\n[[rigs]]", 1))
	before := c.readTOML()
	code, _, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "--force=remote-runtime") {
		t.Fatalf("split on k8s without the attestation = %d: %s", code, stderr)
	}
	if c.readTOML() != before {
		t.Fatal("the refused split changed city.toml")
	}
	c.request.Force = storageForce{RemoteRuntime: true}
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split on k8s attested = %d:\n%s\n%s", code, stdout, stderr)
	}
	if !storageAttestationRecorded(c.request.CityPath, storageHazardRemoteRuntime) {
		t.Fatal("the attestation was not recorded")
	}
	request := c.reload()
	if res := newStorageSplitCheck(request.Cfg, c.request.CityPath).Run(nil); res.Status == doctor.StatusError || !strings.Contains(res.Message, "remote runtime") {
		t.Fatalf("doctor after an attested remote split = %v %s", res.Status, res.Message)
	}
}

// TestStorageSplitSharedStore (S40, O2): a work store another registered city
// uses refuses unless attested; attested beside a live co-tenant it still
// refuses; attested with the co-tenant stopped it runs, and refuses a row
// another city's scope names.
func TestStorageSplitSharedStore(t *testing.T) {
	setup := func(t *testing.T) (*guidedSplitCity, string) {
		c := newGuidedSplitCity(t)
		other := t.TempDir()
		if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other-city\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		stubOtherCities(t, other)
		rigRoot := canonicalIdentityPath(c.rigPath)
		prev := storeDoltIdentityOf
		t.Cleanup(func() { storeDoltIdentityOf = prev })
		storeDoltIdentityOf = func(_, root string) storeDoltIdentity {
			if canonicalIdentityPath(root) == rigRoot {
				return storeDoltIdentity{Database: "packs", Endpoint: "loopback:3399"}
			}
			return storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3307"}
		}
		return c, other
	}
	t.Run("unattested", func(t *testing.T) {
		c, other := setup(t)
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "--force=shared-store:"+sharedToken) {
			t.Fatalf("split over a shared store = %d: %s", code, stderr)
		}
	})
	t.Run("live co-tenant", func(t *testing.T) {
		c, other := setup(t)
		c.request.Force = storageForce{SharedStores: []string{sharedToken}}
		held, err := acquireControllerLock(other)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close() //nolint:errcheck // test cleanup
		if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "holds its controller lock") {
			t.Fatalf("split attested beside a live co-tenant = %d: %s", code, stderr)
		}
	})
	t.Run("foreign rows", func(t *testing.T) {
		c, _ := setup(t)
		c.request.Force = storageForce{SharedStores: []string{sharedToken}}
		mustCreateInfraBead(t, c.work(), beads.Bead{Title: "theirs", Type: "message", Assignee: "mayor", Metadata: beads.StringMap{beadmeta.RootStoreRefMetadataKey: "city:other-city"}})
		before := c.readTOML()
		if code, stdout, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stdout+stderr, "another city's scope") {
			t.Fatalf("split over a foreign row = %d: %s%s", code, stdout, stderr)
		}
		if c.readTOML() != before {
			t.Fatal("the refused split changed city.toml")
		}
	})
	t.Run("attested and stopped", func(t *testing.T) {
		c, other := setup(t)
		c.request.Force = storageForce{SharedStores: []string{sharedToken}}
		if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
			t.Fatalf("split attested with the co-tenant stopped = %d:\n%s\n%s", code, stdout, stderr)
		}
		c.assertSplit()
		if j := c.journal(); strings.Join(j.Forced, ",") != storageHazardSharedStore+":"+sharedToken {
			t.Fatalf("the journal records %v", j.Forced)
		}
		lock, err := acquireControllerLock(other)
		if err != nil {
			t.Fatalf("the split left the co-tenant locked: %v", err)
		}
		lock.Close() //nolint:errcheck // test cleanup
	})
}

// TestStorageSplitSharedRigStore (the S4b lift): a rig store another city
// uses stops the split after the city, at its rig, until attested; and one
// co-tenant of both the work store and the rig is locked once, not refused
// as live (the co-tenant lock registry).
func TestStorageSplitSharedRigStore(t *testing.T) {
	c := newGuidedSplitCity(t)
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	stubOtherCities(t, other)
	stubStoreIdentity(t, storeDoltIdentity{Database: "bd_shared", Endpoint: "loopback:3307"})
	c.request.Force = storageForce{SharedStores: []string{sharedToken}}
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split with one co-tenant of the work store and the rig = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
}

// TestStorageSplitBudgetOverrunIsANoteAndPauses (S5-F owner decision): a
// budget the estimate exceeds is a note, not a refusal; a spent budget pauses
// the city copy with exit 75, and the same command finishes it.
func TestStorageSplitBudgetOverrunIsANoteAndPauses(t *testing.T) {
	c := newGuidedSplitCity(t)
	work := c.work()
	for i := 0; i < 600; i++ {
		mustCreateInfraBead(t, work, beads.Bead{Title: fmt.Sprintf("mail %d", i), Type: "message", Assignee: "mayor"})
	}
	c.request.TimeBudget = time.Nanosecond
	code, stdout, stderr := c.split(splitOptions{})
	if code != storagePausedExitCode {
		t.Fatalf("split under a spent budget = %d, want %d:\n%s\n%s", code, storagePausedExitCode, stdout, stderr)
	}
	if !strings.Contains(stdout, "time budget") || strings.Contains(stdout, "would refuse") {
		t.Fatalf("the rehearsal did not note the budget, or refused on it:\n%s", stdout)
	}
	if j := c.journal(); j.Phase != splitPhaseConfigured {
		t.Fatalf("the paused split's journal = %s, want configured", j.Phase)
	}
	c.request.TimeBudget = 0
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the rerun = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
}

// TestStorageSplitPreflightBlockWritesNothing: a rehearsal block refuses the
// split before the snapshot, the journal or city.toml.
func TestStorageSplitPreflightBlockWritesNothing(t *testing.T) {
	c := newGuidedSplitCity(t)
	// An open work bead blocked by an open infrastructure bead: the edge
	// would cross stores, which the cross-boundary plan refuses by id (D9).
	work := c.work()
	blocker := mustCreateInfraBead(t, work, beads.Bead{Title: "a step", Type: "message", Assignee: "mayor"})
	waiting, err := work.Create(beads.Bead{Title: "work waiting on it", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(waiting.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	before := c.readTOML()
	code, stdout, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "the rehearsal refused the split") {
		t.Fatalf("split over a rehearsal block = %d:\n%s\n%s", code, stdout, stderr)
	}
	if c.readTOML() != before || fileExists(minGCVersionPath(c.request.CityPath)) {
		t.Fatal("the refused split wrote city.toml or the floor")
	}
	if _, present, _ := readStorageSplitJournal(c.request.CityPath); present {
		t.Fatal("the refused split wrote a journal")
	}
	if entries, _ := os.ReadDir(storageSnapshotDefaultDir(c.request.CityPath)); len(entries) != 0 {
		t.Fatalf("the refused split took a snapshot: %v", entries)
	}
}

// TestStorageSplitRigRefusalStopsAfterTheCity: a rig the import refuses
// stops the split with the city split and serving; once fixed, the same
// command finishes.
func TestStorageSplitRigRefusalStopsAfterTheCity(t *testing.T) {
	c := newGuidedSplitCity(t)
	prev := openStorageScopeStore
	// The rig becomes unreachable once the city has converged: the rehearsal
	// read it, so only the import finds it gone.
	broken := false
	prevPoint := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "split "+splitPhaseCityConverged {
			broken = true
		}
		return nil
	}
	t.Cleanup(func() { infraEngineCrashPoint = prevPoint })
	openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
		if broken && samePath(storePath, c.rigPath) {
			return nil, errors.New("rig store unreachable")
		}
		return prev(storePath, cityPath)
	}
	t.Cleanup(func() { openStorageScopeStore = prev })
	code, stdout, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "rig's import did not finish") || strings.Contains(stderr, "--abandon") {
		t.Fatalf("split with an unreachable rig = %d:\n%s\n%s", code, stdout, stderr)
	}
	if j := c.journal(); j.Phase != splitPhaseCityConverged {
		t.Fatalf("the journal = %s, want city_converged", j.Phase)
	}
	request := c.reload()
	if _, err := storageBootGate(c.request.CityPath, request.Cfg, "gc start", nil, io.Discard); err != nil {
		t.Fatalf("boot of the city split before its rig = %v", err)
	}
	if res := newStorageSplitCheck(request.Cfg, c.request.CityPath).Run(nil); !strings.Contains(res.Message, "stopped at "+splitPhaseCityConverged) {
		t.Fatalf("doctor does not name the stopped split: %s", res.Message)
	}
	infraEngineCrashPoint = prevPoint
	broken = false
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the rerun = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
}

// TestStorageSplitHeldByAnOpenReverse: an open reverse holds the split.
func TestStorageSplitHeldByAnOpenReverse(t *testing.T) {
	c := newGuidedSplitCity(t)
	if err := writeStorageReverseJournal(c.request.CityPath, storageReverseJournal{Format: storageReverseJournalFormat, Mode: reverseToWork, Phase: reverseR1, Run: "r"}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := c.split(splitOptions{}); code == 0 || !strings.Contains(stderr, "reverse migration") {
		t.Fatalf("split during a reverse = %d: %s", code, stderr)
	}
}

// TestStorageSplitAfterAReverseRunsFresh: a split stopped at its rigs, then
// reversed, then split again runs a new split, and the old journal is kept
// aside.
func TestStorageSplitAfterAReverseRunsFresh(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseCityConverged)
	first := c.journal()
	var stdout, stderr bytes.Buffer
	if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code != 0 {
		t.Fatalf("--to-work after a stopped split = %d:\n%s\n%s", code, stdout.String(), stderr.String())
	}
	code, out, errOut := c.split(splitOptions{})
	if code != 0 {
		t.Fatalf("split after the reverse = %d:\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "starting a new split") {
		t.Errorf("the rerun did not say it started over: %s", errOut)
	}
	c.assertSplit()
	if c.journal().Run == first.Run {
		t.Fatal("the new split reused the old run's journal")
	}
	if !fileExists(storageSplitJournalPath(c.request.CityPath) + ".superseded-" + first.Run) {
		t.Fatal("the stopped split's journal was not kept aside")
	}
	if fileExists(filepath.Join(first.SnapshotTo, first.Snapshot, storageSnapshotPinName)) {
		t.Fatal("the superseded split's snapshot is still pinned")
	}
}

// TestStorageSplitKeepsACompletedJournalAside: a split after a completed one
// was reversed moves the completed journal to .completed-<run>.
func TestStorageSplitKeepsACompletedJournalAside(t *testing.T) {
	c := newGuidedSplitCity(t)
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("split = %d:\n%s\n%s", code, stdout, stderr)
	}
	first := c.journal()
	var stdout, stderr bytes.Buffer
	if code := doStorageReverse(context.Background(), c.reload(), reverseOptions{Mode: reverseToWork}, &stdout, &stderr); code != 0 {
		t.Fatalf("--to-work = %d:\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if code, out, errOut := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the second split = %d:\n%s\n%s", code, out, errOut)
	}
	if !fileExists(storageSplitJournalPath(c.request.CityPath) + ".completed-" + first.Run) {
		t.Fatal("the completed journal was not kept aside")
	}
	if !fileExists(filepath.Join(first.SnapshotTo, first.Snapshot, storageSnapshotPinName)) {
		t.Fatal("the completed split's snapshot was unpinned")
	}
}

// TestStorageSplitDryRunIsThePreflight: --dry-run is preflight --split, and
// takes no lock.
func TestStorageSplitDryRunIsThePreflight(t *testing.T) {
	c := newGuidedSplitCity(t)
	held, err := acquireControllerLock(c.request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	code, stdout, stderr := c.split(splitOptions{DryRun: true})
	var want bytes.Buffer
	wantCode, _ := doStorageSplitPreflight(c.reload(), &want, &want)
	if code != wantCode || stdout+stderr != want.String() {
		t.Fatalf("--dry-run = %d:\n%s%s\nwant preflight --split = %d:\n%s", code, stdout, stderr, wantCode, want.String())
	}
	if _, present, _ := readStorageSplitJournal(c.request.CityPath); present {
		t.Fatal("the dry run wrote a journal")
	}
}

// TestStorageSplitAbandon: --abandon before the convergence marker puts
// city.toml and the floor back, deletes the half-copied rows and closes the
// journal, at every seam; past the marker it refuses and names --to-work.
func TestStorageSplitAbandon(t *testing.T) {
	for _, seam := range []string{"split " + splitPhaseSnapshotted, "split " + splitPhaseConfiguring, "split floor", "split city.toml", "split " + splitPhaseConfigured, "prepare", "copying rows", "verify", "manifest"} {
		t.Run(seam, func(t *testing.T) {
			c := newGuidedSplitCity(t)
			before := c.readTOML()
			c.killSplitAt(seam)
			code, stdout, stderr := c.split(splitOptions{Abandon: true})
			if code != 0 {
				t.Fatalf("--abandon after a kill at %q = %d:\n%s\n%s", seam, code, stdout, stderr)
			}
			c.assertAbandoned(before)
		})
	}
	t.Run("killed mid-abandon", func(t *testing.T) {
		for _, point := range []string{"split " + splitPhaseAbandoning, "split abandon city.toml", "split abandon rows", "split abandon floor"} {
			t.Run(point, func(t *testing.T) {
				c := newGuidedSplitCity(t)
				before := c.readTOML()
				c.killSplitAt("copying rows")
				prev := infraEngineCrashPoint
				infraEngineCrashPoint = func(p string) error {
					if p == point {
						return errors.New("killed")
					}
					return nil
				}
				code, _, _ := c.split(splitOptions{Abandon: true})
				infraEngineCrashPoint = prev
				if code == 0 {
					t.Fatalf("the kill at %q did not stop the abandon", point)
				}
				if again, _, stderr := c.split(splitOptions{}); again == 0 || !strings.Contains(stderr, "being abandoned") {
					t.Fatalf("a split during an abandon = %d: %s", again, stderr)
				}
				if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
					t.Fatalf("the abandon's rerun = %d:\n%s\n%s", code, stdout, stderr)
				}
				c.assertAbandoned(before)
			})
		}
	})
	t.Run("bytes with no final newline", func(t *testing.T) {
		// Removing the appended block in place would leave the newline the
		// split added; the abandon puts the bytes from before back instead.
		c := newGuidedSplitCity(t)
		before := strings.TrimRight(c.readTOML(), "\n")
		c.writeTOML(before)
		c.killSplitAt("copying rows")
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		c.assertAbandoned(before)
	})
	t.Run("restores an earlier floor", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		if err := os.WriteFile(minGCVersionPath(c.request.CityPath), []byte("1.5.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c.killSplitAt("copying rows")
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if data, _ := os.ReadFile(minGCVersionPath(c.request.CityPath)); string(data) != "1.5.1\n" {
			t.Fatalf("the floor after --abandon = %q, want the earlier one", data)
		}
	})
	t.Run("an edited city.toml loses only [storage]", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("copying rows")
		c.writeTOML(strings.Replace(c.readTOML(), "# trailing comment", "# edited mid-split", 1))
		if code, stdout, stderr := c.split(splitOptions{Abandon: true}); code != 0 {
			t.Fatalf("--abandon = %d:\n%s\n%s", code, stdout, stderr)
		}
		if got := c.readTOML(); !strings.Contains(got, "# edited mid-split") || strings.Contains(got, "[storage") {
			t.Fatalf("city.toml after --abandon:\n%s", got)
		}
	})
	t.Run("past the marker", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("marker")
		before := c.readTOML()
		code, _, stderr := c.split(splitOptions{Abandon: true})
		if code == 0 || !strings.Contains(stderr, storageReverseInstruction(reverseToWork, nil)) {
			t.Fatalf("--abandon past the marker = %d: %s", code, stderr)
		}
		if c.readTOML() != before || c.journal().Phase != splitPhaseConfigured {
			t.Fatal("the refused abandon wrote something")
		}
	})
	t.Run("nothing to abandon", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "nothing to abandon") {
			t.Fatalf("--abandon with no split = %d: %s", code, stderr)
		}
	})
	t.Run("a row the copy did not write", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("copying rows")
		binding, err := openInfraDestination(c.target())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := binding.Create(beads.Bead{Title: "someone else's", Type: "message"}); err != nil {
			t.Fatal(err)
		}
		_ = closeBeadStoreHandle(binding)
		if code, _, stderr := c.split(splitOptions{Abandon: true}); code == 0 || !strings.Contains(stderr, "did not write") {
			t.Fatalf("--abandon over a foreign binding row = %d: %s", code, stderr)
		}
	})
}

// assertAbandoned is what --abandon leaves: city.toml as before, no floor,
// no copy rows or manifest in the binding, the journal closed, the snapshot
// unpinned, and a city boot serves unsplit.
func (c *guidedSplitCity) assertAbandoned(before string) {
	c.t.Helper()
	if got := c.readTOML(); got != before {
		c.t.Fatalf("city.toml after --abandon:\n%q\nwant\n%q", got, before)
	}
	if fileExists(minGCVersionPath(c.request.CityPath)) {
		c.t.Fatal("the version floor survived --abandon")
	}
	target := c.target()
	if fileExists(target.ManifestPath()) {
		c.t.Fatal("the copy manifest survived --abandon")
	}
	if fileExists(target.Database) {
		binding, err := openInfraDestination(target)
		if err != nil {
			c.t.Fatal(err)
		}
		n := len(rigTestList(c.t, binding))
		_ = closeBeadStoreHandle(binding)
		if n != 0 {
			c.t.Fatalf("the binding holds %d row(s) after --abandon", n)
		}
	}
	j := c.journal()
	if j.Phase != splitPhaseAbandoned || j.AbandonedFrom == "" {
		c.t.Fatalf("the journal after --abandon = %s (from %q)", j.Phase, j.AbandonedFrom)
	}
	if j.Snapshot != "" && fileExists(filepath.Join(j.SnapshotTo, j.Snapshot, storageSnapshotPinName)) {
		c.t.Fatal("the snapshot is still pinned after --abandon")
	}
	request := c.reload()
	if _, err := storageBootGate(c.request.CityPath, request.Cfg, "gc start", nil, io.Discard); err != nil {
		c.t.Fatalf("boot after --abandon = %v", err)
	}
	// And the split runs again from the start.
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		c.t.Fatalf("a split after --abandon = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
}

// TestDoctorStorageSplitNamesAnOpenSplitJournal: an open split journal is a
// warning that names the command that continues it (owner decision: WARN).
func TestDoctorStorageSplitNamesAnOpenSplitJournal(t *testing.T) {
	c := newGuidedSplitCity(t)
	c.killSplitAt("split " + splitPhaseReadied)
	res := newStorageSplitCheck(c.reload().Cfg, c.request.CityPath).Run(nil)
	if res.Status != doctor.StatusWarning || !strings.Contains(res.Message, storageSplitInstruction()) {
		t.Fatalf("doctor over an open split journal = %v: %s", res.Status, res.Message)
	}
}

// TestStorageSplitVerifyFailsOnADoctorError: P9 runs the storage-split doctor
// check, and an error there (a gc on the session PATH older than the floor
// the split wrote, B10, put there after the rehearsal checked it) fails the
// run with the journal left before complete; fixed, the same command closes
// the split.
func TestStorageSplitVerifyFailsOnADoctorError(t *testing.T) {
	c := newGuidedSplitCity(t)
	prevPoint := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prevPoint })
	infraEngineCrashPoint = func(p string) error {
		if p == "split "+splitPhaseRigsImported {
			minGCVersionPathProbe = func(*config.City) (string, string, error) { return "/old/gc", "1.5.1", nil }
		}
		return nil
	}
	code, stdout, stderr := c.split(splitOptions{})
	if code == 0 || !strings.Contains(stderr, "storage-split doctor check") {
		t.Fatalf("split with an old gc on the session PATH = %d:\n%s\n%s", code, stdout, stderr)
	}
	if j := c.journal(); j.Phase != splitPhaseRigsImported {
		t.Fatalf("the journal = %s, want rigs_imported", j.Phase)
	}
	infraEngineCrashPoint = prevPoint
	minGCVersionPathProbe = func(*config.City) (string, string, error) { return "", "", nil }
	if code, stdout, stderr := c.split(splitOptions{}); code != 0 {
		t.Fatalf("the rerun = %d:\n%s\n%s", code, stdout, stderr)
	}
	c.assertSplit()
}

// TestSplitCityTOMLAfterProvesTheComposedSplit: the edit is proven against
// the city as it composes on disk, so a [storage] a fragment declares (here
// one the loaded config predates) refuses the edit.
func TestSplitCityTOMLAfterProvesTheComposedSplit(t *testing.T) {
	c := newGuidedSplitCity(t)
	frag := filepath.Join(c.request.CityPath, "storage-frag.toml")
	if err := os.WriteFile(frag, []byte("[storage.bindings.other]\nprovider = \"sqlite-beads\"\npath = \".gc/other\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := "include = [\"storage-frag.toml\"]\n" + c.readTOML()
	if _, err := splitCityTOMLAfter(c.request.CityPath, []byte(root), c.request.Cfg); err == nil {
		t.Fatal("an edit whose city composes another binding beside the split's was proven")
	}
	if _, err := splitCityTOMLAfter(c.request.CityPath, []byte(c.readTOML()), c.request.Cfg); err != nil {
		t.Fatalf("the plain edit = %v", err)
	}
}
