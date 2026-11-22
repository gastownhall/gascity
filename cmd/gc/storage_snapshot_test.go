package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// snapshotCity is a converged split city whose binding holds a small graph:
// rows, an edge carrying a payload and a bare one.
type snapshotCity struct {
	request storageOperatorRequest
	target  infraBindingTarget
	to      string
}

func newSnapshotCity(t *testing.T) snapshotCity {
	t.Helper()
	cityPath, cfg, _, target := convergedInfraCity(t)
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	root := mustCreateInfraBead(t, binding, beads.Bead{Title: "root", Type: "molecule"})
	step := mustCreateInfraBead(t, binding, beads.Bead{Title: "step", Type: "task"})
	other := mustCreateInfraBead(t, binding, beads.Bead{Title: "other", Type: "task"})
	writer, ok := beads.As[beads.DepMetadataWriter](binding)
	if !ok {
		t.Fatal("the SQLite binding has no payload writer")
	}
	if err := writer.DepAddWithMetadata(step.ID, root.ID, "blocks", `{"gate":"waits_for"}`); err != nil {
		t.Fatal(err)
	}
	if err := binding.DepAdd(other.ID, root.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	// Closed now, not at cleanup: a handle left open is another process's
	// connection as far as a restore can tell, and it refuses one.
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatal(err)
	}
	stubInfraControllerPing(t, 0)
	stubSessionInventory(t, nil, true, nil)
	stubOtherCities(t)
	return snapshotCity{
		request: storageOperatorRequest{CityPath: cityPath, Cfg: cfg},
		target:  target,
		to:      filepath.Join(t.TempDir(), "archive"),
	}
}

func (c snapshotCity) snapshot(t *testing.T, opts snapshotOptions) (int, string, string) {
	t.Helper()
	if opts.To == "" {
		opts.To = c.to
	}
	if opts.Keep == 0 {
		opts.Keep = storageSnapshotDefaultKeep
	}
	var stdout, stderr bytes.Buffer
	code := doStorageSnapshot(context.Background(), c.request, opts, "gc storage snapshot", &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// snapshotIDs lists the snapshot directories in to, oldest first, complete
// or not.
func snapshotDirs(t *testing.T, to string) []string {
	t.Helper()
	entries, err := os.ReadDir(to)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && storageSnapshotIDPattern.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Slice(ids, func(i, j int) bool { return snapshotIDLess(ids[i], ids[j]) })
	return ids
}

func onlySnapshot(t *testing.T, to string) (string, snapshotManifest) {
	t.Helper()
	ids := snapshotDirs(t, to)
	if len(ids) != 1 {
		t.Fatalf("snapshots in %s = %v, want one", to, ids)
	}
	m, err := readSnapshotManifest(filepath.Join(to, ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	return ids[0], m
}

func readSnapshotJSONLEntries(t *testing.T, path string) []infraBackupEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var header infraBackupHeader
	if err := decoder.Decode(&header); err != nil {
		t.Fatal(err)
	}
	if header.Format != storageSnapshotJSONLFormat {
		t.Fatalf("JSONL format = %q", header.Format)
	}
	var out []infraBackupEntry
	for decoder.More() {
		var e infraBackupEntry
		if err := decoder.Decode(&e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// TestStorageSnapshotBindingUnderALiveWriter is requirement 1 end to end: a
// controller-like writer keeps committing to the WAL binding while the
// snapshot runs. The copy passes integrity_check, the manifest records it,
// and the JSONL, read from the copy, holds every row the copy holds with its
// edges and the payload one of them carries.
func TestStorageSnapshotBindingUnderALiveWriter(t *testing.T) {
	city := newSnapshotCity(t)
	live, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(live) //nolint:errcheck // test cleanup
	var stop atomic.Bool
	var wg sync.WaitGroup
	var writes atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			if _, err := live.Create(beads.Bead{Title: "written during the snapshot", Type: "task"}); err != nil {
				t.Errorf("live write: %v", err)
				return
			}
			writes.Add(1)
		}
	}()
	// Let the writer get going, so the WAL is live when the copy starts.
	for writes.Load() < 5 {
		runtime.Gosched()
	}
	code, stdout, stderr := city.snapshot(t, snapshotOptions{Binding: true})
	stop.Store(true)
	wg.Wait()
	if code != 0 {
		t.Fatalf("snapshot exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	id, m := onlySnapshot(t, city.to)
	dir := filepath.Join(city.to, id)
	if err := verifySnapshotFiles(context.Background(), dir, m); err != nil {
		t.Fatalf("the snapshot does not verify: %v", err)
	}
	if len(m.Stores) != 1 || m.Stores[0].Name != "binding" || m.Stores[0].Exact.Method != snapshotMethodVacuumInto {
		t.Fatalf("stores = %+v, want the binding by VACUUM INTO", m.Stores)
	}
	rec := m.Stores[0]
	if rec.Payloads != snapshotPayloadsRead {
		t.Fatalf("payloads = %q, want read", rec.Payloads)
	}
	copied, err := beads.OpenSQLiteStore(filepath.Join(dir, "binding"), beads.WithSQLiteStoreReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer closeBeadStoreHandle(copied) //nolint:errcheck // test cleanup
	held := infraStoreFingerprint(t, copied)
	entries := readSnapshotJSONLEntries(t, filepath.Join(dir, rec.JSONL.Path))
	var ids []string
	carried := 0
	for _, e := range entries {
		ids = append(ids, e.Bead.ID)
		for _, d := range e.Deps {
			if d.Carried && d.Payload == `{"gate":"waits_for"}` {
				carried++
			}
		}
	}
	if strings.Join(ids, ",") != strings.Join(held, ",") {
		t.Fatalf("the JSONL holds %v, the copy holds %v", ids, held)
	}
	if rec.Rows != len(held) || rec.Edges != 2 || carried != 1 {
		t.Fatalf("rows %d edges %d carried %d; want %d rows, 2 edges, 1 payload", rec.Rows, rec.Edges, carried, len(held))
	}
}

// TestStorageSnapshotOfAStoppedBindingCarriesItsWAL: a binding a stopped
// writer left with rows only in its -wal (and no -shm) is snapshotted whole,
// and the read-only snapshot leaves both files byte-identical.
func TestStorageSnapshotOfAStoppedBindingCarriesItsWAL(t *testing.T) {
	city := newSnapshotCity(t)
	mainDB, wal := leaveInfraBindingWALResident(t, city.target)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	if !bytes.Equal(mustReadFile(t, city.target.Database), mainDB) || !bytes.Equal(mustReadFile(t, city.target.Database+"-wal"), wal) {
		t.Fatal("the snapshot changed the binding's database or its -wal")
	}
	id, m := onlySnapshot(t, city.to)
	found := false
	for _, e := range readSnapshotJSONLEntries(t, filepath.Join(city.to, id, m.Stores[0].JSONL.Path)) {
		found = found || e.Bead.Title == "a bead that is only in the WAL"
	}
	if !found {
		t.Fatal("the snapshot missed the row that is only in the WAL")
	}
}

// TestStorageSnapshotManifestRecordsTheSnapshot is requirement 3.
func TestStorageSnapshotManifestRecordsTheSnapshot(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, m := onlySnapshot(t, city.to)
	identity, err := currentWorkStoreIdentity(city.request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != id || m.Format != storageSnapshotManifestFormat || m.Identity != identity.String() || m.Identity == "" ||
		m.GCVersion != version+" "+commit || m.City != city.request.CityPath || m.CreatedAt.IsZero() {
		t.Fatalf("manifest = %+v", m)
	}
	rec := m.Stores[0]
	if rec.Exact.File == nil || len(rec.Exact.File.SHA256) != 64 || rec.JSONL.SHA256 == "" || rec.Exact.File.Bytes == 0 {
		t.Fatalf("store record = %+v", rec)
	}
}

// TestStorageSnapshotIfBoundIsInertOnAnUnsplitCity is the order's guarantee:
// on a city with no binding, the order's command writes nothing at all and
// succeeds; without --if-bound the same command refuses.
func TestStorageSnapshotIfBoundIsInertOnAnUnsplitCity(t *testing.T) {
	request := storageTestRequest(t, &config.City{})
	to := filepath.Join(t.TempDir(), "binding-archive")
	var stdout, stderr bytes.Buffer
	code := doStorageSnapshot(context.Background(), request, snapshotOptions{Binding: true, IfBound: true, To: to, Keep: 4}, "gc storage snapshot", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--if-bound on an unsplit city exit %d: %s", code, stderr.String())
	}
	if _, err := os.Stat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--if-bound on an unsplit city created %s (%v)", to, err)
	}
	code = doStorageSnapshot(context.Background(), request, snapshotOptions{Binding: true, To: to, Keep: 4}, "gc storage snapshot", &stdout, &stderr)
	if code == 0 {
		t.Fatal("--binding on an unsplit city succeeded without --if-bound")
	}
	if _, err := os.Stat(to); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused snapshot created %s", to)
	}
}

// TestStorageSnapshotKeepRetainsTheNewest is --keep: the newest N complete
// snapshots stay, older ones and interrupted ones go, and nothing that is not
// named as a snapshot is touched.
func TestStorageSnapshotKeepRetainsTheNewest(t *testing.T) {
	city := newSnapshotCity(t)
	for range 3 {
		if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true, Keep: 5}); code != 0 {
			t.Fatal(stderr)
		}
	}
	before := snapshotDirs(t, city.to)
	if len(before) != 3 {
		t.Fatalf("snapshots = %v", before)
	}
	interrupted := filepath.Join(city.to, "19990101T000000Z")
	foreign := filepath.Join(city.to, "notes")
	for _, d := range []string{interrupted, foreign} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true, Keep: 2}); code != 0 {
		t.Fatal(stderr)
	}
	after := snapshotDirs(t, city.to)
	if len(after) != 2 || after[0] != before[2] {
		t.Fatalf("after --keep 2: %v, want the newest of %v and the new one", after, before)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("retention removed a directory that is not a snapshot: %v", err)
	}
	if _, err := os.Stat(interrupted); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retention kept an interrupted snapshot")
	}
}

func TestApplySnapshotRetentionTable(t *testing.T) {
	owner := snapshotOwner{City: "/cities/a", Identity: workStoreIdentity{ProjectID: "proj-a", Path: "/cities/a"}}
	other := snapshotOwner{City: "/cities/b", Identity: workStoreIdentity{ProjectID: "proj-b", Path: "/cities/b"}}
	// A city recreated at the same path over a new work store is not the
	// city these snapshots were taken of.
	reused := snapshotOwner{City: "/cities/a", Identity: workStoreIdentity{ProjectID: "proj-new", Path: "/cities/a"}}
	for _, tc := range []struct {
		name       string
		complete   []string
		pinned     []string
		others     []string
		reusedPath []string
		unreadable []string
		own        string
		keep       int
		want       []string
	}{
		{name: "keeps all within N", complete: []string{"20260101T000000Z", "20260102T000000Z"}, own: "20260102T000000Z", keep: 2, want: []string{"20260101T000000Z", "20260102T000000Z"}},
		{name: "drops the oldest", complete: []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z"}, own: "20260103T000000Z", keep: 2, want: []string{"20260102T000000Z", "20260103T000000Z"}},
		{name: "suffix orders after its second", complete: []string{"20260101T000000Z", "20260101T000000Z-2", "20260101T000000Z-10", "20260101T000000Z-11"}, own: "20260101T000000Z-11", keep: 2, want: []string{"20260101T000000Z-10", "20260101T000000Z-11"}},
		// R09: a snapshot dated after this run's (a skewed clock, another
		// host) never pushes this run's own snapshot out.
		{name: "own kept though not newest", complete: []string{"20260101T000000Z", "20991231T000000Z"}, own: "20260101T000000Z", keep: 1, want: []string{"20260101T000000Z"}},
		{name: "pinned neither counted nor removed", complete: []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z"}, pinned: []string{"20260101T000000Z"}, own: "20260103T000000Z", keep: 1, want: []string{"20260101T000000Z", "20260103T000000Z"}},
		// A --to several cities share: another city's snapshots, and a
		// complete directory whose manifest does not read, are not this
		// city's to count or remove.
		{name: "another city's left alone", complete: []string{"20260101T000000Z", "20260103T000000Z"}, others: []string{"20260102T000000Z"}, own: "20260103T000000Z", keep: 1, want: []string{"20260102T000000Z", "20260103T000000Z"}},
		{name: "same path, another work store, left alone", complete: []string{"20260101T000000Z", "20260103T000000Z"}, reusedPath: []string{"20260102T000000Z"}, own: "20260103T000000Z", keep: 1, want: []string{"20260102T000000Z", "20260103T000000Z"}},
		{name: "unreadable manifest left alone", complete: []string{"20260101T000000Z", "20260103T000000Z"}, unreadable: []string{"20260102T000000Z"}, own: "20260103T000000Z", keep: 1, want: []string{"20260102T000000Z", "20260103T000000Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			to := t.TempDir()
			write := func(id string, data []byte) {
				if err := os.MkdirAll(filepath.Join(to, id), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(to, id, storageSnapshotManifestName), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			manifest := func(id string, o snapshotOwner) []byte {
				data, err := json.Marshal(snapshotManifest{Format: storageSnapshotManifestFormat, ID: id, City: o.City, Identity: o.Identity.String()})
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			for _, id := range tc.complete {
				write(id, manifest(id, owner))
			}
			for _, id := range tc.others {
				write(id, manifest(id, other))
			}
			for _, id := range tc.reusedPath {
				write(id, manifest(id, reused))
			}
			for _, id := range tc.unreadable {
				write(id, []byte("{"))
			}
			for _, id := range tc.pinned {
				if err := os.WriteFile(filepath.Join(to, id, storageSnapshotPinName), []byte("split\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := applySnapshotRetention(to, tc.keep, tc.own, owner); err != nil {
				t.Fatal(err)
			}
			if got := snapshotDirs(t, to); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := applySnapshotRetention(t.TempDir(), 0, "", owner); err == nil {
		t.Fatal("retention accepted --keep 0, which would remove the snapshot just written")
	}
}

// TestStorageSnapshotRetentionLeavesAnotherCitysSnapshots: two cities
// snapshot into one --to. B snapshots once, then A twice with --keep 2: A
// keeps its two, and B's only snapshot is still there.
func TestStorageSnapshotRetentionLeavesAnotherCitysSnapshots(t *testing.T) {
	a, b := newSnapshotCity(t), newSnapshotCity(t)
	shared := filepath.Join(t.TempDir(), "shared")
	a.to, b.to = shared, shared
	if code, _, stderr := b.snapshot(t, snapshotOptions{Binding: true, Keep: 2}); code != 0 {
		t.Fatal(stderr)
	}
	bOnly := snapshotDirs(t, shared)
	if len(bOnly) != 1 {
		t.Fatalf("after B's snapshot: %v", bOnly)
	}
	for range 2 {
		if code, _, stderr := a.snapshot(t, snapshotOptions{Binding: true, Keep: 2}); code != 0 {
			t.Fatal(stderr)
		}
	}
	all := snapshotDirs(t, shared)
	if !slices.Contains(all, bOnly[0]) {
		t.Fatalf("A's retention removed B's only snapshot %s: %v", bOnly[0], all)
	}
	if len(all) != 3 {
		t.Fatalf("snapshots = %v, want B's one and A's two", all)
	}
}

// TestStorageSnapshotPinKeepsASnapshot: a pinned snapshot (S5-H's mandatory
// one) survives --keep 1 runs, and an unfinished one cannot be pinned.
func TestStorageSnapshotPinKeepsASnapshot(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	pinned, _ := onlySnapshot(t, city.to)
	if err := pinStorageSnapshot(city.to, pinned, "storage split journal"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true, Keep: 1}); code != 0 {
			t.Fatal(stderr)
		}
	}
	dirs := snapshotDirs(t, city.to)
	if len(dirs) != 2 || dirs[0] != pinned {
		t.Fatalf("after two --keep 1 runs: %v, want the pinned %s and the newest", dirs, pinned)
	}
	if err := os.Mkdir(filepath.Join(city.to, "20260101T000000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pinStorageSnapshot(city.to, "20260101T000000Z", "x"); err == nil {
		t.Fatal("an interrupted snapshot was pinned")
	}
}

// TestStorageSnapshotVerify is --verify: a good snapshot verifies; a changed
// JSONL, a copy that fails integrity_check though its recorded hash matches,
// and a directory with no manifest each refuse.
func TestStorageSnapshotVerify(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, m := onlySnapshot(t, city.to)
	dir := filepath.Join(city.to, id)
	verify := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := doStorageSnapshotVerify(context.Background(), city.to, id, "gc storage snapshot", &stdout, &stderr)
		return code, stderr.String()
	}
	if code, stderr := verify(); code != 0 {
		t.Fatalf("a fresh snapshot does not verify: %s", stderr)
	}

	t.Run("changed JSONL", func(t *testing.T) {
		path := filepath.Join(dir, m.Stores[0].JSONL.Path)
		orig := mustReadFile(t, path)
		t.Cleanup(func() { _ = os.WriteFile(path, orig, 0o644) })
		// Same size, one byte changed: only the hash can tell.
		changed := append([]byte{}, orig...)
		i := bytes.LastIndex(changed, []byte(`"title":"`)) + len(`"title":"`)
		changed[i] ^= 0x01
		if err := os.WriteFile(path, changed, 0o644); err != nil {
			t.Fatal(err)
		}
		if code, stderr := verify(); code == 0 || !strings.Contains(stderr, "sha256") {
			t.Fatalf("verify passed a changed JSONL (exit %d): %s", code, stderr)
		}
	})
	t.Run("copy fails integrity_check", func(t *testing.T) {
		path := filepath.Join(dir, m.Stores[0].Exact.File.Path)
		orig := mustReadFile(t, path)
		manifestPath := filepath.Join(dir, storageSnapshotManifestName)
		origManifest := mustReadFile(t, manifestPath)
		t.Cleanup(func() {
			_ = os.WriteFile(path, orig, 0o644)
			_ = os.WriteFile(manifestPath, origManifest, 0o644)
		})
		damaged := append([]byte{}, orig...)
		for i := len(damaged) - 4096; i < len(damaged); i++ {
			damaged[i] = 0xA5
		}
		if err := os.WriteFile(path, damaged, 0o644); err != nil {
			t.Fatal(err)
		}
		// Re-record the hash, so only integrity_check stands between the
		// damaged copy and a passing verify.
		file, err := hashSnapshotFile(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		rewritten := m
		rewritten.Stores = append([]snapshotStoreRecord{}, m.Stores...)
		rewritten.Stores[0].Exact.File = &file
		data, _ := json.Marshal(rewritten)
		if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if code, stderr := verify(); code == 0 || !strings.Contains(stderr, "integrity") {
			t.Fatalf("verify passed a damaged copy (exit %d): %s", code, stderr)
		}
	})
	t.Run("counts disagree with the JSONL", func(t *testing.T) {
		manifestPath := filepath.Join(dir, storageSnapshotManifestName)
		orig := mustReadFile(t, manifestPath)
		t.Cleanup(func() { _ = os.WriteFile(manifestPath, orig, 0o644) })
		rewritten := m
		rewritten.Stores = append([]snapshotStoreRecord{}, m.Stores...)
		rewritten.Stores[0].Rows++
		data, _ := json.Marshal(rewritten)
		if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if code, stderr := verify(); code == 0 || !strings.Contains(stderr, "the JSONL holds") {
			t.Fatalf("verify passed a manifest whose counts disagree (exit %d): %s", code, stderr)
		}
	})
	t.Run("no manifest", func(t *testing.T) {
		manifestPath := filepath.Join(dir, storageSnapshotManifestName)
		orig := mustReadFile(t, manifestPath)
		t.Cleanup(func() { _ = os.WriteFile(manifestPath, orig, 0o644) })
		if err := os.Remove(manifestPath); err != nil {
			t.Fatal(err)
		}
		if code, stderr := verify(); code == 0 || !strings.Contains(stderr, "interrupted") {
			t.Fatalf("verify passed a snapshot with no manifest (exit %d): %s", code, stderr)
		}
	})
}

// TestStorageSnapshotManifestIsWrittenLast is requirement 7: at every step
// before the manifest no manifest exists, and a run that fails at any of them
// removes its directory, leaving nothing that could be taken for a snapshot.
func TestStorageSnapshotManifestIsWrittenLast(t *testing.T) {
	for _, failAt := range []string{"binding copied", "stored binding", "before manifest"} {
		t.Run(failAt, func(t *testing.T) {
			city := newSnapshotCity(t)
			var seen []string
			reached := false
			prev := storageSnapshotStep
			t.Cleanup(func() { storageSnapshotStep = prev })
			storageSnapshotStep = func(step string) error {
				seen = append(seen, step)
				dirs := snapshotDirs(t, city.to)
				if len(dirs) != 1 {
					t.Errorf("at step %q the snapshot directories are %v, want the one being written", step, dirs)
				}
				for _, id := range dirs {
					if _, err := os.Stat(filepath.Join(city.to, id, storageSnapshotManifestName)); err == nil {
						t.Errorf("the manifest exists at step %q, before the snapshot is proven", step)
					}
				}
				if step == failAt {
					reached = true
					return errors.New("injected failure")
				}
				return nil
			}
			if code, _, _ := city.snapshot(t, snapshotOptions{Binding: true}); code == 0 || !reached {
				t.Fatalf("a run failing at %q: exit %d, reached %v (steps %v)", failAt, code, reached, seen)
			}
			if dirs := snapshotDirs(t, city.to); len(dirs) != 0 {
				t.Fatalf("a failed run left %v behind", dirs)
			}
			storageSnapshotStep = prev
			if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
				t.Fatal(stderr)
			}
		})
	}
}

// TestStorageSnapshotProvesItsFilesBeforeTheManifest: a file that changes
// after it was recorded (a disk error, a stray writer) keeps the manifest from
// being written at all, and the run removes its directory.
func TestStorageSnapshotProvesItsFilesBeforeTheManifest(t *testing.T) {
	city := newSnapshotCity(t)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	tampered := false
	storageSnapshotStep = func(step string) error {
		if step == "stored binding" {
			ids := snapshotDirs(t, city.to)
			path := filepath.Join(city.to, ids[0], "binding.jsonl")
			data := mustReadFile(t, path)
			data[len(data)-3] ^= 0x01
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			tampered = true
		}
		return nil
	}
	code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true})
	if !tampered || code == 0 || !strings.Contains(stderr, "sha256") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if dirs := snapshotDirs(t, city.to); len(dirs) != 0 {
		t.Fatalf("a snapshot whose file no longer matches left %v", dirs)
	}
}

// TestStorageSnapshotDiskPreflightRefuses is the disk-space preflight: too
// little room writes nothing.
func TestStorageSnapshotDiskPreflightRefuses(t *testing.T) {
	city := newSnapshotCity(t)
	prev := doltContainerFreeBytesFunc
	t.Cleanup(func() { doltContainerFreeBytesFunc = prev })
	doltContainerFreeBytesFunc = func(string) (int64, error) { return 1024, nil }
	code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true})
	if code == 0 || !strings.Contains(stderr, "bytes free") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if ids := snapshotDirs(t, city.to); len(ids) != 0 {
		t.Fatalf("a refused snapshot wrote %v", ids)
	}
}

// TestStorageSnapshotsIntoOneDirectoryAreSerialized: a held directory lock
// refuses a second snapshot rather than letting its retention remove a
// directory the first is still writing.
func TestStorageSnapshotsIntoOneDirectoryAreSerialized(t *testing.T) {
	city := newSnapshotCity(t)
	if err := os.MkdirAll(city.to, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := lockSnapshotDir(city.to)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code == 0 || !strings.Contains(stderr, "another snapshot") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestSnapshotExactMethodTable(t *testing.T) {
	for _, tc := range []struct {
		provider string
		taggable bool
		want     string
	}{
		{"bd", true, snapshotMethodDoltTag},
		{"bd", false, snapshotMethodBdExport},
		{"exec:/x/gc-beads-bd.sh", false, snapshotMethodBdExport},
		{"file", false, snapshotMethodFileCopy},
		{"exec:/x/custom.sh", false, snapshotMethodNone},
		{"postgres", false, snapshotMethodNone},
	} {
		if got := snapshotExactMethod(tc.provider, tc.taggable); got != tc.want {
			t.Errorf("snapshotExactMethod(%q, %v) = %q, want %q", tc.provider, tc.taggable, got, tc.want)
		}
	}
}

// TestStorageSnapshotWorkStoreBdExport is the bd-backed work store: under the
// window and --fleet-stopped, its exact copy is `bd export`, recorded with
// its hash, and its JSONL holds every row.
func TestStorageSnapshotWorkStoreBdExport(t *testing.T) {
	city := newSnapshotCity(t)
	source := stubInfraMigrationSource(t)
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work", Type: "task"})
	var exported []string
	prev := snapshotBdExport
	t.Cleanup(func() { snapshotBdExport = prev })
	snapshotBdExport = func(cityPath string, _ *config.City, rigDir, out string) error {
		exported = append(exported, cityPath+"|"+rigDir)
		return os.WriteFile(out, []byte(`{"id":"`+work.ID+`"}`+"\n"), 0o644)
	}

	city.request.FleetStopped = false
	if code, _, stderr := city.snapshot(t, snapshotOptions{Work: true}); code == 0 || !strings.Contains(stderr, "--fleet-stopped") {
		t.Fatalf("--work without --fleet-stopped: exit %d: %s", code, stderr)
	}
	city.request.FleetStopped = true
	code, stdout, stderr := city.snapshot(t, snapshotOptions{Binding: true, Work: true})
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, stdout, stderr)
	}
	if len(exported) != 1 || exported[0] != city.request.CityPath+"|" {
		t.Fatalf("bd export calls = %v", exported)
	}
	id, m := onlySnapshot(t, city.to)
	if len(m.Stores) != 2 || m.Stores[1].Name != "work" || m.Stores[1].Exact.Method != snapshotMethodBdExport || m.Stores[1].Exact.File == nil {
		t.Fatalf("stores = %+v", m.Stores)
	}
	entries := readSnapshotJSONLEntries(t, filepath.Join(city.to, id, m.Stores[1].JSONL.Path))
	if len(entries) != m.Stores[1].Rows || m.Stores[1].Rows < 1 {
		t.Fatalf("work JSONL rows %d, manifest %d", len(entries), m.Stores[1].Rows)
	}
}

// TestStorageSnapshotWorkStoreFileCopy is the file store: its file is copied.
func TestStorageSnapshotWorkStoreFileCopy(t *testing.T) {
	city := newSnapshotCity(t)
	stubInfraMigrationSource(t)
	t.Setenv("GC_BEADS", "file")
	if err := os.WriteFile(filepath.Join(city.request.CityPath, ".gc", "beads.json"), []byte(`{"beads":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	city.request.FleetStopped = true
	if code, _, stderr := city.snapshot(t, snapshotOptions{Work: true}); code != 0 {
		t.Fatal(stderr)
	}
	_, m := onlySnapshot(t, city.to)
	if m.Stores[0].Exact.Method != snapshotMethodFileCopy || m.Stores[0].Exact.File.Bytes != int64(len(`{"beads":[]}`)) {
		t.Fatalf("work store = %+v", m.Stores[0])
	}
}

// TestStorageSnapshotRestoreBinding is requirement 5: the binding is put back
// exactly, the replaced database is kept aside, and the restore refuses
// without the attestation or while the controller lock is held.
func TestStorageSnapshotRestoreBinding(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, _ := onlySnapshot(t, city.to)
	snapshotted := bindingFingerprint(t, city.target)
	binding := openMigratedDestination(t, city.target)
	mustCreateInfraBead(t, binding, beads.Bead{Title: "after the snapshot", Type: "task"})
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatal(err)
	}
	restore := func() (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := doStorageSnapshotRestore(context.Background(), city.request, city.to, id, "gc storage snapshot restore", &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	if code, _, stderr := restore(); code == 0 || !strings.Contains(stderr, "--fleet-stopped") {
		t.Fatalf("restore without --fleet-stopped: exit %d: %s", code, stderr)
	}
	city.request.FleetStopped = true
	held, err := acquireControllerLock(city.request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := restore(); code == 0 || !strings.Contains(stderr, "controller lock") {
		t.Fatalf("restore under a held controller lock: exit %d: %s", code, stderr)
	}
	_ = held.Close()
	if got := bindingFingerprint(t, city.target); len(got) != len(snapshotted)+1 {
		t.Fatal("a refused restore changed the binding")
	}

	code, stdout, stderr := restore()
	if code != 0 {
		t.Fatalf("restore exit %d\n%s\n%s", code, stdout, stderr)
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored binding holds %v, the snapshot %v", got, snapshotted)
	}
	aside, _ := filepath.Glob(city.target.Database + ".pre-restore-*")
	if len(aside) == 0 {
		t.Fatal("the replaced database was not kept")
	}
}

// TestStorageSnapshotRestoreRefusesASnapshotThatDoesNotVerify: a snapshot is
// restored only whole. A JSONL that no longer matches its manifest refuses
// the restore, though the binding copy itself is intact, and the live binding
// is left as it was.
func TestStorageSnapshotRestoreRefusesASnapshotThatDoesNotVerify(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, m := onlySnapshot(t, city.to)
	path := filepath.Join(city.to, id, m.Stores[0].JSONL.Path)
	data := mustReadFile(t, path)
	data[len(data)-3] ^= 0x01
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, city.target.Database)
	city.request.FleetStopped = true
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshotRestore(context.Background(), city.request, city.to, id, "restore", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "does not verify") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !bytes.Equal(mustReadFile(t, city.target.Database), before) {
		t.Fatal("a refused restore changed the binding")
	}
}

// TestInstallSnapshotBindingRefusesACopyThatDoesNotMatch: the copy made beside
// the live database is hashed against the snapshot's record before anything
// moves; a mismatch leaves the live database where it is and no temp file.
func TestInstallSnapshotBindingRefusesACopyThatDoesNotMatch(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, m := onlySnapshot(t, city.to)
	before := mustReadFile(t, city.target.Database)
	src := filepath.Join(city.to, id, m.Stores[0].Exact.File.Path)
	if _, err := installSnapshotBinding(context.Background(), city.target, src, strings.Repeat("0", 64), m.Binding.SequenceFloor, infraRestoreRecord{Snapshot: id, StartedAt: time.Now()}); err == nil {
		t.Fatal("a copy whose hash does not match was installed")
	}
	if !bytes.Equal(mustReadFile(t, city.target.Database), before) {
		t.Fatal("the live database moved")
	}
	if left, _ := filepath.Glob(city.target.Database + ".restore-*"); len(left) != 0 {
		t.Fatalf("temp copies left: %v", left)
	}
	if _, err := os.Stat(city.target.RestoreRecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a restore that never moved the database left its record (%v)", err)
	}
}

// TestStorageSnapshotRestoreKilledMidwayFailsClosed: a restore that stops
// after the live database is moved aside leaves no database, which boot reads
// as stale and refuses, never a database separated from its WAL; the same
// command finishes it.
func TestStorageSnapshotRestoreKilledMidwayFailsClosed(t *testing.T) {
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, _ := onlySnapshot(t, city.to)
	snapshotted := bindingFingerprint(t, city.target)
	leaveInfraBindingWALResident(t, city.target)
	city.request.FleetStopped = true

	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	storageSnapshotStep = func(step string) error {
		if strings.HasPrefix(step, "restore moved ") {
			return errors.New("killed")
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshotRestore(context.Background(), city.request, city.to, id, "restore", &stdout, &stderr); code == 0 {
		t.Fatal("the interrupted restore succeeded")
	}
	state, err := readInfraConvergenceState(city.target)
	if err != nil {
		t.Fatal(err)
	}
	if state != infraConvergenceStale {
		t.Fatalf("after an interrupted restore the binding state is %v, want stale (boot refuses)", state)
	}
	if info, err := os.Stat(city.target.Database + "-wal"); err == nil && info.Size() > 0 {
		t.Fatal("a populated -wal was left at the binding path with no database; a read-write open would replay it into a new one")
	}
	storageSnapshotStep = prev
	if code := doStorageSnapshotRestore(context.Background(), city.request, city.to, id, "restore", &stdout, &stderr); code != 0 {
		t.Fatalf("rerun: %s", stderr.String())
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored %v, want %v", got, snapshotted)
	}
}

// TestStorageSnapshotsLiveUnderDotGC: the default location is under .gc, which
// `gc init --from` never copies.
func TestStorageSnapshotsLiveUnderDotGC(t *testing.T) {
	dir := storageSnapshotDefaultDir("/city")
	if dir != filepath.Join("/city", ".gc", "snapshots") {
		t.Fatalf("default dir = %s", dir)
	}
	if !initFromSkip(filepath.Join(".gc", "snapshots", "20260101T000000Z", "binding.jsonl"), false) || !initFromSkip(filepath.Join(".gc", "snapshots"), true) {
		t.Fatal("gc init --from would copy .gc/snapshots")
	}
}

// storageSnapshotKillHelperEnv carries the child's city, binding, archive and
// kill point, tab-separated. The child prints storageSnapshotKillReady when it
// reaches the kill point.
const (
	storageSnapshotKillHelperEnv = "GC_TEST_STORAGE_SNAPSHOT_KILL_HELPER"
	storageSnapshotKillReady     = "storage-snapshot-kill-point-reached"
)

// TestStorageSnapshotKillHelper is the child of
// TestStorageSnapshotSurvivesSIGKILL: it snapshots the binding and, at the
// kill point, says so and waits to be killed.
func TestStorageSnapshotKillHelper(t *testing.T) {
	spec := os.Getenv(storageSnapshotKillHelperEnv)
	if spec == "" {
		t.Skip("run by TestStorageSnapshotSurvivesSIGKILL")
	}
	f := strings.Split(spec, "\t")
	cityPath, bindingPath, to, killAt := f[0], f[1], f[2], f[3]
	storageSnapshotStep = func(step string) error {
		if step == killAt {
			fmt.Fprintln(os.Stdout, storageSnapshotKillReady) //nolint:errcheck // the parent reads it
			select {}
		}
		return nil
	}
	request := storageOperatorRequest{CityPath: cityPath, Cfg: infraSplitConfig(bindingPath)}
	var stdout, stderr bytes.Buffer
	doStorageSnapshot(context.Background(), request, snapshotOptions{Binding: true, To: to, Keep: 4}, "child", &stdout, &stderr)
	t.Fatalf("the child reached no kill point %q: %s", killAt, stderr.String())
}

// TestStorageSnapshotSurvivesSIGKILL is requirement 7 under a real SIGKILL:
// a snapshot process killed at each step leaves a directory that is not a
// snapshot (verify refuses it), and the next snapshot removes it and writes
// a complete one.
func TestStorageSnapshotSurvivesSIGKILL(t *testing.T) {
	if os.Getenv(storageSnapshotKillHelperEnv) != "" {
		t.Skip("child")
	}
	for _, killAt := range []string{"binding copied", "stored binding", "before manifest"} {
		t.Run(killAt, func(t *testing.T) {
			city := newSnapshotCity(t)
			bindingPath := city.request.Cfg.Storage.Bindings["infra"].Path
			cmd := exec.Command(os.Args[0], "-test.run=^TestStorageSnapshotKillHelper$", "-test.count=1")
			// Bazel shards the test binary through TEST_SHARD_*; the child would
			// inherit a shard that filters its own helper out.
			cmd.Env = append(unshardedEnv(), storageSnapshotKillHelperEnv+"="+strings.Join([]string{city.request.CityPath, bindingPath, city.to, killAt}, "\t"))
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// The child prints the ready line at the kill point and then
			// blocks; a child that exits first closes the pipe instead.
			reached := false
			var seen strings.Builder
			for scanner := bufio.NewScanner(stdout); scanner.Scan(); {
				seen.WriteString(scanner.Text() + "\n")
				if scanner.Text() == storageSnapshotKillReady {
					reached = true
					break
				}
			}
			if !reached {
				_ = cmd.Wait()
				t.Fatalf("the child never reached %q:\n%s%s", killAt, seen.String(), stderr.String())
			}
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()

			ids := snapshotDirs(t, city.to)
			if len(ids) != 1 {
				t.Fatalf("after the kill: %v", ids)
			}
			var verifyOut, verifyErr bytes.Buffer
			if code := doStorageSnapshotVerify(context.Background(), city.to, ids[0], "gc storage snapshot", &verifyOut, &verifyErr); code == 0 {
				t.Fatal("a snapshot killed before its manifest verifies")
			}
			if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
				t.Fatal(stderr)
			}
			// The sweep removed the killed one; the one left is complete.
			// (It may reuse the killed one's id, which was free again.)
			after := snapshotDirs(t, city.to)
			if len(after) != 1 {
				t.Fatalf("after the next snapshot: %v, want one", after)
			}
			if code := doStorageSnapshotVerify(context.Background(), city.to, after[0], "gc storage snapshot", &verifyOut, &verifyErr); code != 0 {
				t.Fatalf("the snapshot after the kill does not verify: %s", verifyErr.String())
			}
		})
	}
}

// taggingStore is a work store whose engine can take a Dolt tag, as a native
// Dolt one can (beads.NativeDoltStore.DoltTag runs on a real server in
// internal/beads).
type taggingStore struct {
	*beads.MemStore
	tags []string
	err  error
}

func (s *taggingStore) DoltTag(name, _ string) (beads.DoltTagResult, error) {
	if s.err != nil {
		return beads.DoltTagResult{}, s.err
	}
	s.tags = append(s.tags, name)
	return beads.DoltTagResult{Commit: "abc123", DirtyWorkingSet: true}, nil
}

// TestStorageSnapshotWorkStoreDoltTag is the native-Dolt arm: a store that
// can be tagged is tagged gc-storage-snapshot-<id> rather than exported, the
// manifest records the tag and the commit, and a dirty working set is noted.
func TestStorageSnapshotWorkStoreDoltTag(t *testing.T) {
	city := newSnapshotCity(t)
	store := &taggingStore{MemStore: beads.NewMemStore()}
	mustCreateInfraBead(t, store, beads.Bead{Title: "work", Type: "task"})
	prev := openInfraMigrationSource
	t.Cleanup(func() { openInfraMigrationSource = prev })
	openInfraMigrationSource = func(string) (beads.Store, error) { return store, nil }
	exportPrev := snapshotBdExport
	t.Cleanup(func() { snapshotBdExport = exportPrev })
	snapshotBdExport = func(string, *config.City, string, string) error {
		t.Error("a taggable store was exported instead")
		return nil
	}
	city.request.FleetStopped = true
	if code, _, stderr := city.snapshot(t, snapshotOptions{Work: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, m := onlySnapshot(t, city.to)
	rec := m.Stores[0]
	if rec.Exact.Method != snapshotMethodDoltTag || rec.Exact.Tag != snapshotTagName(id, city.to) || rec.Exact.Commit != "abc123" || len(rec.Notes) != 2 || rec.Notes[0] != snapshotDoltTagNote {
		t.Fatalf("work store = %+v", rec)
	}
	if len(store.tags) != 1 || store.tags[0] != rec.Exact.Tag {
		t.Fatalf("tags taken = %v", store.tags)
	}
	if rec.Rows != 1 {
		t.Fatalf("rows = %d", rec.Rows)
	}

	// A lane that cannot tag (a read-only proxied handle) falls back to bd
	// export for a bd-backed scope.
	exported := 0
	snapshotBdExport = func(_ string, _ *config.City, _, out string) error {
		exported++
		return os.WriteFile(out, []byte("{}\n"), 0o644)
	}
	store.err = beads.ErrDoltTagUnsupported
	if code, _, stderr := city.snapshot(t, snapshotOptions{Work: true, Keep: 5}); code != 0 {
		t.Fatal(stderr)
	}
	newest := snapshotDirs(t, city.to)
	fallback, err := readSnapshotManifest(filepath.Join(city.to, newest[len(newest)-1]))
	if err != nil {
		t.Fatal(err)
	}
	if exported != 1 || fallback.Stores[0].Exact.Method != snapshotMethodBdExport || len(fallback.Stores[0].Notes) != 1 {
		t.Fatalf("fallback = %+v, exports %d", fallback.Stores[0], exported)
	}
	complete := map[string]bool{}
	for _, sid := range newest {
		complete[sid] = true
	}

	// A tag the store refuses for any other reason fails the snapshot: no
	// manifest is written.
	store.err = errors.New("the server refused the tag")
	if code, _, _ := city.snapshot(t, snapshotOptions{Work: true, Keep: 5}); code == 0 {
		t.Fatal("a refused tag produced a snapshot")
	}
	for _, sid := range snapshotDirs(t, city.to) {
		if !complete[sid] {
			if _, err := os.Stat(filepath.Join(city.to, sid, storageSnapshotManifestName)); err == nil {
				t.Fatal("a snapshot whose tag was refused has a manifest")
			}
		}
	}
}

// TestStorageSnapshotRigs snapshots every rig with a store of its own, each
// under its own name, and skips a rig that shares the city's root.
func TestStorageSnapshotRigs(t *testing.T) {
	city := newSnapshotCity(t)
	stubInfraMigrationSource(t)
	rigRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigRoot, ".gc", "beads.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	city.request.Cfg.Rigs = []config.Rig{{Name: "alpha", Path: rigRoot}, {Name: "hq", Path: city.request.CityPath}}
	rigStore := beads.NewMemStore()
	mustCreateInfraBead(t, rigStore, beads.Bead{Title: "rig work", Type: "task"})
	mustCreateInfraBead(t, rigStore, beads.Bead{Title: "rig work 2", Type: "task"})
	var opened []string
	prev := openStorageScopeStore
	t.Cleanup(func() { openStorageScopeStore = prev })
	openStorageScopeStore = func(storePath, _ string) (beads.Store, error) {
		opened = append(opened, storePath)
		return rigStore, nil
	}
	city.request.FleetStopped = true
	if code, _, stderr := city.snapshot(t, snapshotOptions{Rigs: true}); code != 0 {
		t.Fatal(stderr)
	}
	_, m := onlySnapshot(t, city.to)
	if len(m.Stores) != 1 || m.Stores[0].Name != "rig:alpha" || m.Stores[0].Rows != 2 || m.Stores[0].Exact.Method != snapshotMethodFileCopy || m.Stores[0].JSONL.Path != "rig-alpha.jsonl" {
		t.Fatalf("stores = %+v", m.Stores)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %v, want the one rig with its own store", opened)
	}
}

// TestStorageSnapshotCommandSurface pins the operator surface the design and
// the order spell: the flags, the restore subcommand and its --binding gate.
func TestStorageSnapshotCommandSurface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newStorageCmd(&stdout, &stderr)
	snap, _, err := root.Find([]string{storageSnapshotVerb})
	if err != nil || snap.Name() != storageSnapshotVerb {
		t.Fatalf("gc storage snapshot: %v", err)
	}
	for _, flag := range []string{"binding", "work", "rigs", "to", "keep", "verify", "if-bound", storageFleetStoppedFlag, storageForceFlag} {
		if snap.Flags().Lookup(flag) == nil {
			t.Errorf("gc storage snapshot has no --%s", flag)
		}
	}
	if got := snap.Flags().Lookup("keep").DefValue; got != "2" {
		t.Errorf("--keep defaults to %s, want 2", got)
	}
	restore, _, err := root.Find([]string{storageSnapshotVerb, "restore"})
	if err != nil || restore.Name() != "restore" {
		t.Fatalf("gc storage snapshot restore: %v", err)
	}
	root.SetArgs([]string{storageSnapshotVerb, "restore", "20260101T000000Z", "--" + storageFleetStoppedFlag})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err == nil || !strings.Contains(stderr.String(), "pass --binding") {
		t.Fatalf("restore without --binding: %v: %s", err, stderr.String())
	}
}

func (c snapshotCity) restore(t *testing.T, id string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := doStorageSnapshotRestore(context.Background(), c.request, c.to, id, "gc storage snapshot restore", &stdout, &stderr)
	return code, stderr.String()
}

// snapshottedCity is a city with one binding snapshot taken, ready to
// restore: the attestation given, and the binding's rows at the snapshot.
func snapshottedCity(t *testing.T) (snapshotCity, string, []string) {
	t.Helper()
	city := newSnapshotCity(t)
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, _ := onlySnapshot(t, city.to)
	city.request.FleetStopped = true
	return city, id, bindingFingerprint(t, city.target)
}

func addBindingRows(t *testing.T, target infraBindingTarget, n int) []string {
	t.Helper()
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range n {
		ids = append(ids, mustCreateInfraBead(t, binding, beads.Bead{Title: "after the snapshot", Type: "task"}).ID)
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatal(err)
	}
	return ids
}

func isWAL(t *testing.T, path string) bool {
	t.Helper()
	wal, err := beads.SQLiteFileIsWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	return wal
}

// TestStorageSnapshotRestoreLeavesTheBindingInWAL is MUST-1: the snapshot's
// copy is kept in rollback-journal mode (one self-contained file), and the
// restore installs it in WAL mode, so readers do not stall the controller's
// writers. A stray -journal at the binding path is moved aside, never applied
// to the new database.
func TestStorageSnapshotRestoreLeavesTheBindingInWAL(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	_, m := onlySnapshot(t, city.to)
	if isWAL(t, filepath.Join(city.to, id, m.Stores[0].Exact.File.Path)) {
		t.Fatal("the snapshot copy is in WAL mode; the manifest documents rollback-journal")
	}
	if !isWAL(t, city.target.Database) {
		t.Fatal("the fixture's live binding is not in WAL mode")
	}
	if err := os.WriteFile(city.target.Database+"-journal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if !isWAL(t, city.target.Database) {
		t.Fatal("the restored binding is in rollback-journal mode, want WAL")
	}
	if _, err := os.Stat(city.target.Database + "-journal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stray -journal is still at the binding path")
	}
	if aside, _ := filepath.Glob(city.target.Database + "-journal.pre-restore-*"); len(aside) != 1 {
		t.Fatalf("the stray -journal was not kept aside: %v", aside)
	}
}

// TestStorageSnapshotSweepsInterruptedBeforePreflight is MUST-3: a SIGKILLed
// snapshot's directory is removed before free space is measured, so its space
// counts.
func TestStorageSnapshotSweepsInterruptedBeforePreflight(t *testing.T) {
	city := newSnapshotCity(t)
	interrupted := filepath.Join(city.to, "20260101T000000Z")
	if err := os.MkdirAll(interrupted, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := doltContainerFreeBytesFunc
	t.Cleanup(func() { doltContainerFreeBytesFunc = prev })
	measured := false
	doltContainerFreeBytesFunc = func(string) (int64, error) {
		measured = true
		if _, err := os.Stat(interrupted); err == nil {
			t.Error("free space was measured with an interrupted snapshot still on disk")
		}
		return 1 << 50, nil
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 || !measured {
		t.Fatalf("exit %d, measured %v: %s", code, measured, stderr)
	}
}

// TestStorageSnapshotRestoreSweepsStaleTempCopies is MUST-3 (S11): the temp
// copies a killed restore left beside the binding are removed.
func TestStorageSnapshotRestoreSweepsStaleTempCopies(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	stale := city.target.Database + ".restore-123"
	if err := os.WriteFile(stale, []byte("a killed restore's copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stale restore copy is still beside the binding")
	}
}

// TestStorageSnapshotRestoreRefusesAfterTheRecordsChanged is MUST-4. A rig
// import after the snapshot writes the rig's records beside the binding (and
// its cleared note): restoring the pre-import database under them would leave
// the rig's rows in neither store, so the restore refuses, naming them. It
// refuses a snapshot of another city or another work store too.
func TestStorageSnapshotRestoreRefusesAfterTheRecordsChanged(t *testing.T) {
	t.Run("rig import since", func(t *testing.T) {
		city, id, before := snapshottedCity(t)
		addBindingRows(t, city.target, 2)
		for path, body := range map[string]string{
			filepath.Join(city.target.Root, rigGraphImportMarkerName("gp")): "imported\n",
			rigGraphClearedNotePath(city.request.CityPath, "gp"):            `{"complete":true}`,
		} {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		now := bindingFingerprint(t, city.target)
		code, stderr := city.restore(t, id)
		if code == 0 || !strings.Contains(stderr, "binding/infra.rig.gp.migrated") || !strings.Contains(stderr, "storage-infra-cleared.rig.gp.json") {
			t.Fatalf("restore over a rig import since: exit %d: %s", code, stderr)
		}
		if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(now, ",") || len(now) == len(before) {
			t.Fatal("a refused restore changed the binding")
		}
	})
	t.Run("marker changed", func(t *testing.T) {
		city, id, _ := snapshottedCity(t)
		f, err := os.OpenFile(city.target.MarkerPath(), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString("rewritten\n")
		_ = f.Close()
		if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "binding/infra.migrated (changed") {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	})
	for _, tc := range []struct {
		name   string
		edit   func(*snapshotManifest)
		reason string
	}{
		{"another work store", func(m *snapshotManifest) { m.Identity = "project=other database=- path=-" }, "work store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, id, _ := snapshottedCity(t)
			manifestPath := filepath.Join(city.to, id, storageSnapshotManifestName)
			var m snapshotManifest
			if err := json.Unmarshal(mustReadFile(t, manifestPath), &m); err != nil {
				t.Fatal(err)
			}
			tc.edit(&m)
			data, _ := json.Marshal(m)
			if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, tc.reason) {
				t.Fatalf("exit %d: %s", code, stderr)
			}
		})
	}
}

// TestStorageSnapshotRestoreRefusesAForeignConnection is SHOULD-6 (R08): a
// process still holding the binding open (here, a store) keeps its -wal and
// -shm alive past this command's connection; the restore refuses and changes
// nothing, then succeeds once it is closed.
func TestStorageSnapshotRestoreRefusesAForeignConnection(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	held, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateInfraBead(t, held, beads.Bead{Title: "written by the holder", Type: "task"})
	code, stderr := city.restore(t, id)
	if code == 0 || !strings.Contains(stderr, "another process has the binding") {
		t.Fatalf("restore under a live connection: exit %d: %s", code, stderr)
	}
	if aside, _ := filepath.Glob(city.target.Database + "*.pre-restore-*"); len(aside) != 0 {
		t.Fatalf("a refused restore moved files aside: %v", aside)
	}
	if err := closeBeadStoreHandle(held); err != nil {
		t.Fatal(err)
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
}

// TestStorageSnapshotRestoreOverAStaleWAL is R04: a binding whose newest rows
// are only in a -wal (a stopped writer's) is restored without that WAL ever
// meeting the new database: the binding holds exactly the snapshot's rows,
// and the -wal is folded into the database kept aside.
func TestStorageSnapshotRestoreOverAStaleWAL(t *testing.T) {
	city, id, snapshotted := snapshottedCity(t)
	leaveInfraBindingWALResident(t, city.target)
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored %v, want the snapshot's %v", got, snapshotted)
	}
	aside, _ := filepath.Glob(city.target.Database + ".pre-restore-*")
	if len(aside) != 1 {
		t.Fatalf("aside = %v", aside)
	}
	ids, err := beads.SQLiteBeadIDs(context.Background(), aside[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(snapshotted)+1 {
		t.Fatalf("the database kept aside holds %d rows, want the snapshot's %d plus the WAL's one", len(ids), len(snapshotted))
	}
}

// TestStorageSnapshotRestoreExclusion is R02, R03 and SHOULD-7: a restore
// refuses while another migrator holds the guard, while a controller answers
// the ping, and while a snapshot holds the directory, changing nothing.
func TestStorageSnapshotRestoreExclusion(t *testing.T) {
	check := func(t *testing.T, city snapshotCity, id, want string) {
		t.Helper()
		before := mustReadFile(t, city.target.Database)
		code, stderr := city.restore(t, id)
		if code == 0 || !strings.Contains(stderr, want) {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		if !bytes.Equal(mustReadFile(t, city.target.Database), before) {
			t.Fatal("a refused restore changed the binding")
		}
	}
	t.Run("migration guard", func(t *testing.T) {
		city, id, _ := snapshottedCity(t)
		guard, err := acquireStorageMigrationGuard(context.Background(), city.request.CityPath)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Release() //nolint:errcheck // test cleanup
		check(t, city, id, "another storage migration")
	})
	t.Run("controller ping", func(t *testing.T) {
		city, id, _ := snapshottedCity(t)
		stubInfraControllerPing(t, 4242)
		check(t, city, id, "controller PID 4242")
	})
	t.Run("snapshot directory", func(t *testing.T) {
		city, id, _ := snapshottedCity(t)
		held, err := lockSnapshotDir(city.to)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close() //nolint:errcheck // test cleanup
		check(t, city, id, "another snapshot or restore")
	})
}

// TestStorageSnapshotRefusesWhileTheBindingIsBeingWritten is SHOULD-7: a live
// binding snapshot takes the migration guard, and refuses a binding a re-copy
// or a rig import is writing.
func TestStorageSnapshotRefusesWhileTheBindingIsBeingWritten(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, city snapshotCity) func()
		want  string
	}{
		{"migration guard", func(t *testing.T, city snapshotCity) func() {
			guard, err := acquireStorageMigrationGuard(context.Background(), city.request.CityPath)
			if err != nil {
				t.Fatal(err)
			}
			return func() { _ = guard.Release() }
		}, "another storage command (a migration, a recovery, a purge or a restore) holds this city"},
		{"re-copy", func(t *testing.T, city snapshotCity) func() {
			if err := os.WriteFile(city.target.RecopyRecordPath(), []byte("re-copy\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}, "re-copy of the binding is in flight"},
		{"rig import", func(t *testing.T, city snapshotCity) func() {
			if err := os.WriteFile(filepath.Join(city.target.Root, rigGraphInFlightName("gp")), []byte("importing\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return func() {}
		}, "rig import is in flight"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := newSnapshotCity(t)
			release := tc.setup(t, city)
			defer release()
			code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true})
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if _, err := os.Stat(city.to); err == nil {
				if dirs := snapshotDirs(t, city.to); len(dirs) != 0 {
					t.Fatalf("a refused snapshot wrote %v", dirs)
				}
			}
		})
	}
}

// TestStorageSnapshotRestoreRaisesTheIDFloor is SHOULD-12: rows minted after
// the snapshot are gone from the restored database, and their ids are never
// minted again.
func TestStorageSnapshotRestoreRaisesTheIDFloor(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	gone := addBindingRows(t, city.target, 5)
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	reminted := addBindingRows(t, city.target, 5)
	for _, a := range reminted {
		for _, b := range gone {
			if a == b {
				t.Fatalf("the restored binding minted %s again, an id the replaced database held", a)
			}
		}
	}
}

// TestStorageSnapshotInterruptedRestoreNamesItself is SHOULD-11: a restore
// stopped once the live database has moved leaves a stale binding whose boot
// refusal and status name the restore, not a re-copy.
func TestStorageSnapshotInterruptedRestoreNamesItself(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	storageSnapshotStep = func(step string) error {
		if strings.HasPrefix(step, "restore moved ") {
			return errors.New("killed")
		}
		return nil
	}
	if code, _ := city.restore(t, id); code == 0 {
		t.Fatal("the interrupted restore succeeded")
	}
	remedy := infraStaleBindingRemedy(city.target)
	if !strings.Contains(remedy, "gc storage snapshot restore "+id+" --binding --fleet-stopped") {
		t.Fatalf("the stale binding's remedy is %q, want the interrupted restore", remedy)
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(city.request, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "snapshot restore "+id) {
		t.Fatalf("status after an interrupted restore (exit %d):\n%s", code, stdout.String())
	}
	storageSnapshotStep = prev
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if _, err := os.Stat(city.target.RestoreRecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a finished restore left its record")
	}
}

// TestSnapshotDiskNeedAndVerdict is SHOULD-13 (R05c, R06): the estimate counts
// the copy, the JSONL and the WAL's growth, and the verdict leaves the
// managed-Dolt critical floor free and warns under its warning floor.
func TestSnapshotDiskNeedAndVerdict(t *testing.T) {
	const mib = int64(1 << 20)
	for _, tc := range []struct {
		db, wal, want int64
	}{
		{0, 0, 64 * mib},
		{100 * mib, 0, 200*mib + 64*mib},
		{100 * mib, 10 * mib, 220*mib + 64*mib},
		{100 * mib, 200 * mib, 600*mib + 200*mib},
	} {
		if got := snapshotDiskNeed(tc.db, tc.wal); got != tc.want {
			t.Errorf("snapshotDiskNeed(%d, %d) = %d, want %d", tc.db, tc.wal, got, tc.want)
		}
	}
	const critical, warn = 500 * mib, 2048 * mib
	for _, tc := range []struct {
		free, need  int64
		refuse, low bool
	}{
		{free: 3000 * mib, need: 100 * mib},
		{free: 2000 * mib, need: 100 * mib, low: true},
		{free: 600 * mib, need: 100 * mib, low: true},
		{free: 599 * mib, need: 100 * mib, refuse: true, low: true},
		{free: 100 * mib, need: 200 * mib, refuse: true, low: true},
	} {
		refuse, low := snapshotDiskVerdict(tc.free, tc.need, critical, warn)
		if refuse != tc.refuse || low != tc.low {
			t.Errorf("snapshotDiskVerdict(free %d, need %d) = (%v, %v), want (%v, %v)", tc.free, tc.need, refuse, low, tc.refuse, tc.low)
		}
	}
}

// TestStorageSnapshotFlagCombinations covers the NITs: --if-bound goes with
// --binding alone, --verify takes no other flag, and two directories' tags for
// one id differ.
func TestStorageSnapshotFlagCombinations(t *testing.T) {
	request := storageTestRequest(t, &config.City{})
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshot(context.Background(), request, snapshotOptions{Work: true, IfBound: true, Keep: 1, To: t.TempDir()}, "x", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "--if-bound goes with --binding alone") {
		t.Fatalf("--if-bound --work: exit %d: %s", code, stderr.String())
	}
	root := newStorageCmd(&stdout, &stderr)
	stderr.Reset()
	root.SetArgs([]string{storageSnapshotVerb, "--verify", "20260101T000000Z", "--keep", "3"})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err == nil || !strings.Contains(stderr.String(), "--keep means nothing with it") {
		t.Fatalf("--verify --keep: %v: %s", err, stderr.String())
	}
	if snapshotTagName("20260101T000000Z", "/a") == snapshotTagName("20260101T000000Z", "/b") {
		t.Fatal("one id snapshotted into two directories takes one tag")
	}
}

// TestStorageSnapshotRestoreMovesALeftoverWALAside is R04's other half: a
// populated -wal with no database beside it (an earlier restore killed after
// its first move, over a WAL that would not checkpoint) is moved aside before
// the new database lands, never replayed into it.
func TestStorageSnapshotRestoreMovesALeftoverWALAside(t *testing.T) {
	city, id, snapshotted := snapshottedCity(t)
	_, wal := leaveInfraBindingWALResident(t, city.target)
	if err := os.Rename(city.target.Database, city.target.Database+".killed-run"); err != nil {
		t.Fatal(err)
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored %v, want the snapshot's %v", got, snapshotted)
	}
	aside, _ := filepath.Glob(city.target.Database + "-wal.pre-restore-*")
	if len(aside) != 1 || !bytes.Equal(mustReadFile(t, aside[0]), wal) {
		t.Fatalf("the leftover -wal was not kept aside whole: %v", aside)
	}
}

// TestStorageSnapshotRestoreRefusesABusyCheckpoint is R08's other half: a
// process reading the binding (an open read transaction over a WAL with
// frames past it) blocks the checkpoint, and the restore refuses rather than
// move a database another process is using.
func TestStorageSnapshotRestoreRefusesABusyCheckpoint(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	reader, err := sql.Open("sqlite", "file:"+city.target.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck // test cleanup
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // test cleanup
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM beads").Scan(&n); err != nil {
		t.Fatal(err)
	}
	writer, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateInfraBead(t, writer, beads.Bead{Title: "past the reader's snapshot", Type: "task"})
	code, stderr := city.restore(t, id)
	_ = closeBeadStoreHandle(writer)
	if code == 0 || !strings.Contains(stderr, "its WAL could not be folded in") {
		t.Fatalf("restore under a reader: exit %d: %s", code, stderr)
	}
}

// TestStorageSnapshotRestoreRaisesALaggingFloor is SHOULD-12 where it bites:
// the floor sidecar lags the database it is replaced with (lost, or restored
// from elsewhere), so only the replaced database's high-water keeps its ids
// from being minted again.
func TestStorageSnapshotRestoreRaisesALaggingFloor(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	gone := addBindingRows(t, city.target, 5)
	floors, _ := filepath.Glob(filepath.Join(city.target.Dir, "*seqfloor*"))
	if len(floors) == 0 {
		t.Fatal("the fixture's binding has no floor sidecar")
	}
	for _, f := range floors {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	// The allocator hands ids out of reserved blocks, so a collision would
	// take a block's worth of mints to show; the property is that every id
	// the restored binding mints is above the replaced database's high-water.
	suffix := func(id string) int {
		n, err := strconv.Atoi(id[strings.LastIndex(id, "-")+1:])
		if err != nil {
			t.Fatalf("id %q has no sequence suffix", id)
		}
		return n
	}
	high := 0
	for _, b := range gone {
		high = max(high, suffix(b))
	}
	for _, a := range addBindingRows(t, city.target, 5) {
		if suffix(a) <= high {
			t.Fatalf("the restored binding minted %s, at or below gcg-%d, the replaced database's high-water: its ids can be minted again", a, high)
		}
	}
}

// TestStorageSnapshotRestoreHoldsTheBindingStaleWhileCopying is round-3 A:
// from the moment the restore starts copying, the binding counts as stale, so
// a gc that boots during the copy refuses it (and names the restore) instead
// of writing a row the restore then moves aside.
func TestStorageSnapshotRestoreHoldsTheBindingStaleWhileCopying(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	sawStale := false
	storageSnapshotStep = func(step string) error {
		if step != "restore copied" {
			return nil
		}
		state, err := readInfraConvergenceState(city.target)
		if err != nil {
			t.Fatal(err)
		}
		sawStale = state == infraConvergenceStale
		var stdout, stderr bytes.Buffer
		if code := doStorageStatus(city.request, &stdout, &stderr); code == 0 || !strings.Contains(stdout.String(), "snapshot restore "+id) {
			t.Errorf("status during the copy (exit %d) does not refuse naming the restore:\n%s", code, stdout.String())
		}
		return nil
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if !sawStale {
		t.Fatal("the binding was not stale during the restore's copy: a gc booting then would write to a database about to be moved aside")
	}
	if state, err := readInfraConvergenceState(city.target); err != nil || state != infraConvergenceMarked {
		t.Fatalf("after the restore the binding is %v (%v), want marked", state, err)
	}
}

// TestStorageSnapshotRestoreRefusesAWriterThatOpenedDuringTheCopy is round-3
// A: a process that opened the binding during the copy, past any boot gate,
// and still has it open, is refused before anything moves; its row stays in
// the live binding, and the restore's record goes.
func TestStorageSnapshotRestoreRefusesAWriterThatOpenedDuringTheCopy(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	var late beads.Store
	var row beads.Bead
	storageSnapshotStep = func(step string) error {
		if step != "restore copied" {
			return nil
		}
		var err error
		if late, err = openInfraDestination(city.target); err != nil {
			t.Fatal(err)
		}
		row = mustCreateInfraBead(t, late, beads.Bead{Title: "written during the copy", Type: "task"})
		return nil
	}
	code, stderr := city.restore(t, id)
	if late != nil {
		_ = closeBeadStoreHandle(late)
	}
	if code == 0 || !strings.Contains(stderr, "another process has the binding") || !strings.Contains(stderr, "Nothing was moved") {
		t.Fatalf("restore with a writer that opened during the copy: exit %d: %s", code, stderr)
	}
	if aside, _ := filepath.Glob(city.target.Database + "*.pre-restore-*"); len(aside) != 0 {
		t.Fatalf("a refused restore moved %v", aside)
	}
	if _, err := os.Stat(city.target.RestoreRecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused restore left its record, holding the binding stale")
	}
	found := false
	for _, got := range bindingFingerprint(t, city.target) {
		found = found || got == row.ID
	}
	if !found {
		t.Fatalf("the row written during the copy (%s) is not in the live binding", row.ID)
	}
}

// TestStorageSnapshotRestoreSupersedesAnAbandonedRecord: a restore killed
// before its first move leaves its record over the old database. The binding
// is stale, its message says the database is still in place and how to keep
// it, and a later restore supersedes the record rather than leaving it to name
// the old snapshot.
func TestStorageSnapshotRestoreSupersedesAnAbandonedRecord(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	if err := writeInfraRestoreRecord(city.target, infraRestoreRecord{Snapshot: "20200101T000000Z", Database: city.target.Database, StartedAt: time.Now().UTC(), Command: snapshotRestoreInstruction("20200101T000000Z", city.to)}); err != nil {
		t.Fatal(err)
	}
	if state, _ := readInfraConvergenceState(city.target); state != infraConvergenceStale {
		t.Fatalf("a binding under an unfinished restore's record is %v, want stale", state)
	}
	if cause := infraStaleCause(city.target); !strings.Contains(cause, "nothing has moved") || !strings.Contains(cause, "remove "+city.target.RestoreRecordPath()) {
		t.Fatalf("stale cause = %q", cause)
	}
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	storageSnapshotStep = func(step string) error {
		if step == "restore copied" {
			rec, _, err := readInfraRestoreRecord(city.target)
			if err != nil || rec.Snapshot != id {
				t.Errorf("during the restore the record names %q (%v), want %s", rec.Snapshot, err, id)
			}
		}
		return nil
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if _, err := os.Stat(city.target.RestoreRecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the superseded record survived the restore")
	}
}

// TestStorageSnapshotRestoreAcceptsCompatibleChanges is round-3 B: changes a
// restore of the snapshot's database stays true under do not refuse it: a rig
// marked clean since, a marker that only gained its identity line, the city
// moved to another path, and the work store's path changed under the same
// project.
func TestStorageSnapshotRestoreAcceptsCompatibleChanges(t *testing.T) {
	t.Run("rig marked clean since", func(t *testing.T) {
		city, id, _ := snapshottedCity(t)
		if err := os.WriteFile(filepath.Join(city.target.Root, rigGraphCleanMarkerName("quiet")), []byte("clean\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, stderr := city.restore(t, id); code != 0 {
			t.Fatal(stderr)
		}
	})
	t.Run("marker gained its identity line", func(t *testing.T) {
		city := newSnapshotCity(t)
		marker := string(mustReadFile(t, city.target.MarkerPath()))
		var legacy []string
		for _, line := range strings.SplitAfter(marker, "\n") {
			if !strings.HasPrefix(line, workStoreIdentityLinePrefix) {
				legacy = append(legacy, line)
			}
		}
		if strings.Join(legacy, "") == marker {
			t.Fatal("the fixture's marker has no identity line to take out")
		}
		if err := os.WriteFile(city.target.MarkerPath(), []byte(strings.Join(legacy, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
			t.Fatal(stderr)
		}
		id, _ := onlySnapshot(t, city.to)
		if err := os.WriteFile(city.target.MarkerPath(), []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
		city.request.FleetStopped = true
		if code, stderr := city.restore(t, id); code != 0 {
			t.Fatal(stderr)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*snapshotManifest)
	}{
		{"city moved", func(m *snapshotManifest) { m.City = "/where/the/city/was" }},
		{"work-store path changed", func(m *snapshotManifest) {
			id, err := parseWorkStoreIdentity(m.Identity)
			if err != nil {
				t.Fatal(err)
			}
			id.Path = "/where/the/work/store/was"
			m.Identity = id.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, id, _ := snapshottedCity(t)
			manifestPath := filepath.Join(city.to, id, storageSnapshotManifestName)
			var m snapshotManifest
			if err := json.Unmarshal(mustReadFile(t, manifestPath), &m); err != nil {
				t.Fatal(err)
			}
			tc.edit(&m)
			data, _ := json.Marshal(m)
			if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if code, stderr := city.restore(t, id); code != 0 {
				t.Fatal(stderr)
			}
		})
	}
}

// TestStorageSnapshotRestoreRefusesAGoneRecord is M4f: a record the snapshot
// recorded that is gone since (a rig import's marker moved aside) refuses.
func TestStorageSnapshotRestoreRefusesAGoneRecord(t *testing.T) {
	city := newSnapshotCity(t)
	gp := filepath.Join(city.target.Root, rigGraphImportMarkerName("gp"))
	if err := os.WriteFile(gp, []byte("imported\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, _ := onlySnapshot(t, city.to)
	if err := os.Remove(gp); err != nil {
		t.Fatal(err)
	}
	city.request.FleetStopped = true
	if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "binding/infra.rig.gp.migrated (gone since the snapshot)") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

// TestStorageSnapshotRestoreUsesTheRecordedFloor is M4i: the floor the
// snapshot recorded counts even when the live sidecar is gone and neither
// database reaches it, so ids the binding had reserved up to it are not
// minted again.
func TestStorageSnapshotRestoreUsesTheRecordedFloor(t *testing.T) {
	city := newSnapshotCity(t)
	const recorded = 5000
	if _, err := beads.RaiseSQLiteSequenceFloorPast(city.target.Dir, "gcg", []int64{recorded}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, m := onlySnapshot(t, city.to)
	if m.Binding.SequenceFloor != recorded {
		t.Fatalf("the manifest records floor %d, want %d", m.Binding.SequenceFloor, recorded)
	}
	floors, _ := filepath.Glob(filepath.Join(city.target.Dir, "*seqfloor*"))
	for _, f := range floors {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	city.request.FleetStopped = true
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	for _, a := range addBindingRows(t, city.target, 3) {
		n, err := strconv.Atoi(a[strings.LastIndex(a, "-")+1:])
		if err != nil || n <= recorded {
			t.Fatalf("the restored binding minted %s, at or below the recorded floor %d", a, recorded)
		}
	}
}

// TestStorageSnapshotRestoreRaisesTheFloorPastAWrappedRange is round-3 C end
// to end: the replaced binding holds a wrapped (negative) auto id, its floor
// sidecar is gone, and the restored binding still mints, past the wrapped id.
func TestStorageSnapshotRestoreRaisesTheFloorPastAWrappedRange(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	const wrapped = int64(-9223372036853998976)
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	creator, ok := beads.As[beads.ForeignIDCreator](binding)
	if !ok {
		t.Fatal("the binding cannot pin an id")
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: fmt.Sprintf("gcg-%d", wrapped), Title: "minted by a wrapping build", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	floors, _ := filepath.Glob(filepath.Join(city.target.Dir, "*seqfloor*"))
	for _, f := range floors {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	if floor, err := beads.SQLiteSequenceFloorAt(city.target.Dir, "gcg"); err != nil || floor != wrapped {
		t.Fatalf("floor after the restore = %d (%v), want the wrapped %d", floor, err, wrapped)
	}
	minted := addBindingRows(t, city.target, 1)
	if minted[0] != fmt.Sprintf("gcg-%d", wrapped+1) {
		t.Fatalf("the restored binding minted %s, want the id after the wrapped one", minted[0])
	}
}

// TestStorageSnapshotNamesAnInconsistentRow is round-3 D: a row stored under
// an id its bead does not carry fails the snapshot, naming the row, instead
// of a count mismatch with no culprit.
func TestStorageSnapshotNamesAnInconsistentRow(t *testing.T) {
	city := newSnapshotCity(t)
	db, err := sql.Open("sqlite", "file:"+city.target.Database)
	if err != nil {
		t.Fatal(err)
	}
	var victim string
	if err := db.QueryRow("SELECT id FROM beads ORDER BY id LIMIT 1").Scan(&victim); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE beads SET bead_json = json_set(bead_json, '$.id', 'gcg-damaged') WHERE id = ?", victim); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true})
	if code == 0 || !strings.Contains(stderr, victim) || !strings.Contains(stderr, "inconsistent") || strings.Contains(stderr, "as )") || strings.Contains(stderr, "as ),") {
		t.Fatalf("snapshot over an inconsistent row %s: exit %d: %s", victim, code, stderr)
	}
}

// TestSnapshotDiskPreflightCountsTheWAL is R06: the preflight measures the
// live WAL, so a binding whose WAL is large refuses where its database alone
// would fit.
func TestSnapshotDiskPreflightCountsTheWAL(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "beads.sqlite")
	for path, size := range map[string]int64{database: 1 << 20, database + "-wal": 100 << 20} {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	withWAL, withoutWAL := snapshotDiskNeed(1<<20, 100<<20), snapshotDiskNeed(1<<20, 0)
	free := doltDiskMinFreeBytes() + (withWAL+withoutWAL)/2
	prev := doltContainerFreeBytesFunc
	t.Cleanup(func() { doltContainerFreeBytesFunc = prev })
	doltContainerFreeBytesFunc = func(string) (int64, error) { return free, nil }
	var stderr bytes.Buffer
	if err := snapshotDiskPreflight(dir, database, "x", &stderr); err == nil {
		t.Fatalf("the preflight passed %d free bytes for a binding that needs %d with its WAL", free, withWAL)
	}
	if err := os.Remove(database + "-wal"); err != nil {
		t.Fatal(err)
	}
	if err := snapshotDiskPreflight(dir, database, "x", &stderr); err != nil {
		t.Fatalf("without the WAL the same free space should fit: %v", err)
	}
}

// writeRigImportedRows writes rows into the binding as a rig import leaves
// them: under the rig's ids and created_at, with their metadata, stamped as
// imported from rig.
func writeRigImportedRows(t *testing.T, target infraBindingTarget, rig string, rows ...beads.Bead) {
	t.Helper()
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatal(err)
	}
	creator, ok := beads.As[beads.ForeignIDCreator](binding)
	if !ok {
		t.Fatal("the binding cannot pin an id")
	}
	for _, b := range rows {
		b.Metadata = maps.Clone(b.Metadata)
		if b.Metadata == nil {
			b.Metadata = map[string]string{}
		}
		b.Metadata[infraMigrationStampKey] = rigImportStamp(rig)
		if _, err := creator.CreateWithForeignID(b); err != nil {
			t.Fatal(err)
		}
	}
	// Closed now: a handle left open is another process's connection as far
	// as a restore can tell.
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatal(err)
	}
}

// stubRigScopeStore serves store as every rig's own store and records the
// store paths opened.
func stubRigScopeStore(t *testing.T, store beads.Store) *[]string {
	t.Helper()
	var opened []string
	prev := openStorageScopeStore
	t.Cleanup(func() { openStorageScopeStore = prev })
	openStorageScopeStore = func(storePath, _ string) (beads.Store, error) {
		opened = append(opened, storePath)
		return store, nil
	}
	return &opened
}

// TestStorageSnapshotRestoreRefusesRowsARigImportLeftBehind is the content
// check behind the record check: an operator moved a rig import's records
// aside without first restoring the rig's store. The live binding still holds
// the rows imported from the rig (stamped rig:<R>), which neither the snapshot
// nor the rig's own store holds; restoring would leave them in no store, so it
// refuses, naming the rig and the rows, once it has read the rig's store.
func TestStorageSnapshotRestoreRefusesRowsARigImportLeftBehind(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	city.request.Cfg.Rigs = []config.Rig{{Name: "gp", Path: t.TempDir()}}
	writeRigImportedRows(t, city.target, "gp", beads.Bead{ID: "gp-aaa1", Title: "imported from gp", Type: "task"})
	opened := stubRigScopeStore(t, beads.NewMemStore())
	before := mustReadFile(t, city.target.Database)
	code, stderr := city.restore(t, id)
	if code == 0 || !strings.Contains(stderr, "rig gp: 1 row(s), e.g. gp-aaa1") || !strings.Contains(stderr, "neither the snapshot nor the rig's own store") {
		t.Fatalf("restore over rows a rig import left behind: exit %d: %s", code, stderr)
	}
	if len(*opened) != 1 {
		t.Fatalf("rig stores read: %v, want gp's", *opened)
	}
	if !bytes.Equal(mustReadFile(t, city.target.Database), before) {
		t.Fatal("a refused restore changed the binding")
	}
}

// TestStorageSnapshotRestoreAcceptsRowsTheRigHoldsAgain drives the runbook's
// "Rolling back past a rig import" to the end: the rig's own store holds the
// imported rows again under their ids and created_at, one of each tier (a
// committed row, an ephemeral one and a no-history one), and the import's
// records were moved aside. The city is stopped, so the restore reads the
// rig's store for the rows the snapshot lacks inside a maintenance window,
// which starts the provider under the controller lock and stops it again,
// finds each, and restores; a cross-store gate the import minted in the
// binding was never the rig's row, and is not looked for. A rig reset to a
// Dolt tag holds its committed rows only: the refusal says so for the others
// and names the import's backup, which holds them. A row the rig holds under
// another created_at is another row, and still refuses; so does every row of
// a rig whose store cannot be read, or that the config no longer names, even
// when no record names it either. A rig with no such row is not read, and no
// provider starts.
func TestStorageSnapshotRestoreAcceptsRowsTheRigHoldsAgain(t *testing.T) {
	rigged := func(t *testing.T) (snapshotCity, string, []string, *beads.MemStore, []beads.Bead) {
		t.Helper()
		city, id, snapshotted := snapshottedCity(t)
		city.request.Cfg.Rigs = []config.Rig{{Name: "gp", Path: t.TempDir()}}
		rig := beads.NewMemStore()
		rig.HonorExplicitIDs = true
		rows := []beads.Bead{
			mustCreateInfraBead(t, rig, beads.Bead{ID: "gp-aaa1", Title: "imported from gp", Type: "task"}),
			mustCreateInfraBead(t, rig, beads.Bead{ID: "gp-aaa2", Title: "imported from gp, ephemeral", Type: "task", Ephemeral: true}),
			mustCreateInfraBead(t, rig, beads.Bead{ID: "gp-aaa3", Title: "imported from gp, no history", Type: "task", NoHistory: true}),
		}
		return city, id, snapshotted, rig, rows
	}
	t.Run("the rig holds them again", func(t *testing.T) {
		city, id, snapshotted, rig, rows := rigged(t)
		writeRigImportedRows(t, city.target, "gp", rows...)
		provider := &migrationWindowProvider{down: true}
		stubMigrationWindowProvider(t, provider)
		var inWindow []bool
		prev := openStorageScopeStore
		t.Cleanup(func() { openStorageScopeStore = prev })
		openStorageScopeStore = func(string, string) (beads.Store, error) {
			inWindow = append(inWindow, provider.starts == 1 && provider.stops == 0)
			return rig, nil
		}
		if code, stderr := city.restore(t, id); code != 0 {
			t.Fatalf("restore once the rig's store holds the imported rows again: exit %d: %s", code, stderr)
		}
		if len(inWindow) != 1 || !inWindow[0] {
			t.Fatalf("rig store opens inside the window: %v, want gp's, once, after the provider started and before it stopped", inWindow)
		}
		if provider.starts != 1 || provider.stops != 1 || !provider.lockHeldAtStart || !provider.lockHeldAtStop {
			t.Fatalf("provider starts = %d, stops = %d, lock held at start = %v, at stop = %v; want the stopped provider started once and stopped once, both under the controller lock", provider.starts, provider.stops, provider.lockHeldAtStart, provider.lockHeldAtStop)
		}
		if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
			t.Fatalf("restored %v, want %v", got, snapshotted)
		}
	})
	t.Run("with a gate the import minted", func(t *testing.T) {
		// An open blocks edge onto rig work became a cross-store gate in the
		// binding, stamped as the import's. The rig never held it: the restore
		// drops it with the binding, and the edge goes back to the rig with
		// its owner.
		city, id, snapshotted, rig, rows := rigged(t)
		writeRigImportedRows(t, city.target, "gp", rows...)
		binding, err := openInfraDestination(city.target)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := binding.Get(rows[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		gate, err := createRigImportGate(binding, rigImportStamp("gp"), rigScopeRef("gp"), rigImportGate{Owner: owner, Blocker: "gp-work1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := closeBeadStoreHandle(binding); err != nil {
			t.Fatal(err)
		}
		stubRigScopeStore(t, rig)
		if code, stderr := city.restore(t, id); code != 0 {
			t.Fatalf("restore over the import's cross-store gate %s: exit %d: %s", gate.ID, code, stderr)
		}
		if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
			t.Fatalf("restored %v, want %v", got, snapshotted)
		}
	})
	t.Run("a rig reset to a Dolt tag", func(t *testing.T) {
		backup := func(city snapshotCity) string {
			return newRigImportLocation(city.target, city.request.CityPath, "gp").BackupPath()
		}
		t.Run("holds the committed row", func(t *testing.T) {
			city, id, _, rig, rows := rigged(t)
			writeRigImportedRows(t, city.target, "gp", rows...)
			for _, b := range rows[1:] {
				if err := rig.Delete(b.ID); err != nil {
					t.Fatal(err)
				}
			}
			stubRigScopeStore(t, rig)
			code, stderr := city.restore(t, id)
			want := "rig gp: 2 row(s), e.g. gp-aaa2, gp-aaa3 (all of them are ephemeral or no-history rows, which bd keeps out of Dolt history, so no Dolt tag of the rig holds them; they are in the rig import's backup, " + backup(city) + ")."
			if code == 0 || !strings.Contains(stderr, want) {
				t.Fatalf("restore over a rig reset to a Dolt tag: exit %d: %s\nwant it to contain: %s", code, stderr, want)
			}
		})
		t.Run("holds none of them", func(t *testing.T) {
			city, id, _, _, rows := rigged(t)
			writeRigImportedRows(t, city.target, "gp", rows...)
			stubRigScopeStore(t, beads.NewMemStore())
			code, stderr := city.restore(t, id)
			want := "rig gp: 3 row(s), e.g. gp-aaa1, gp-aaa2, gp-aaa3 (2 of them, e.g. gp-aaa2, gp-aaa3, are ephemeral or no-history rows, which bd keeps out of Dolt history, so no Dolt tag of the rig holds them; they are in the rig import's backup, " + backup(city) + ")."
			if code == 0 || !strings.Contains(stderr, want) {
				t.Fatalf("restore over a rig holding none of its imported rows: exit %d: %s\nwant it to contain: %s", code, stderr, want)
			}
		})
	})
	t.Run("one the rig holds under another created_at", func(t *testing.T) {
		city, id, _, rig, rows := rigged(t)
		rows[0].CreatedAt = rows[0].CreatedAt.Add(time.Hour)
		writeRigImportedRows(t, city.target, "gp", rows...)
		stubRigScopeStore(t, rig)
		if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "rig gp: 1 row(s), e.g. gp-aaa1.") {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	})
	t.Run("a rig store that cannot be read", func(t *testing.T) {
		city, id, _, _, rows := rigged(t)
		writeRigImportedRows(t, city.target, "gp", rows...)
		prev := openStorageScopeStore
		t.Cleanup(func() { openStorageScopeStore = prev })
		openStorageScopeStore = func(string, string) (beads.Store, error) {
			return nil, errors.New("the rig's Dolt is unreachable")
		}
		if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "rig gp: 3 row(s), e.g. gp-aaa1, gp-aaa2, gp-aaa3 (its store cannot be read: the rig's Dolt is unreachable).") {
			t.Fatalf("exit %d: %s", code, stderr)
		}
	})
	t.Run("a rig the config no longer names", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			cleanRecord bool
		}{
			// A clean record written since names the rig and is compatible.
			{name: "named by a clean record", cleanRecord: true},
			// Runbook step 2 moved its records aside: only its rows'
			// stamps name it.
			{name: "named by its rows alone"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				city, id, _, rig, rows := rigged(t)
				if tc.cleanRecord {
					if err := os.WriteFile(filepath.Join(city.target.Root, rigGraphCleanMarkerName("gone")), []byte("clean\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				writeRigImportedRows(t, city.target, "gone", rows[0])
				provider := &migrationWindowProvider{down: true}
				stubMigrationWindowProvider(t, provider)
				opened := stubRigScopeStore(t, rig)
				if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "rig gone: 1 row(s), e.g. gp-aaa1 (its store cannot be read: the rig is not in this city's config)") {
					t.Fatalf("exit %d: %s", code, stderr)
				}
				if len(*opened) != 0 || provider.starts != 0 {
					t.Fatalf("rig stores read: %v, provider starts: %d; want none (gp holds no imported row, and no store of gone can be read)", *opened, provider.starts)
				}
			})
		}
	})
	t.Run("a rig with no imported row", func(t *testing.T) {
		city, id, snapshotted, rig, _ := rigged(t)
		provider := &migrationWindowProvider{down: true}
		stubMigrationWindowProvider(t, provider)
		opened := stubRigScopeStore(t, rig)
		if code, stderr := city.restore(t, id); code != 0 {
			t.Fatalf("restore on a city whose rig imported nothing since the snapshot: exit %d: %s", code, stderr)
		}
		if len(*opened) != 0 || provider.starts != 0 {
			t.Fatalf("rig stores read: %v, provider starts: %d; want none: no imported row needs the rig's store", *opened, provider.starts)
		}
		if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
			t.Fatalf("restored %v, want %v", got, snapshotted)
		}
	})
}

// TestStorageSnapshotRestoreAcceptsARetryOfAnImportedWorkflow: a rig import
// carries the rig's workflows in flight into the binding, and the controller
// runs them from there. After the snapshot, a transient failure of an imported
// run makes the dispatcher mint the next attempt in the binding, cloned from
// the imported rows. The clones are the binding's own rows, under its ids,
// which the import did not write and no rig store or import backup holds, so
// they carry no import stamp: the restore drops them with the binding's other
// writes since the snapshot, reading no rig store, rather than refusing rows
// no rollback can put back.
func TestStorageSnapshotRestoreAcceptsARetryOfAnImportedWorkflow(t *testing.T) {
	city := newSnapshotCity(t)
	city.request.Cfg.Rigs = []config.Rig{{Name: "gp", Path: t.TempDir()}}
	writeRigImportedRows(t, city.target, "gp",
		beads.Bead{ID: "gp-root", Title: "workflow", Type: "task", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
		}},
		beads.Bead{ID: "gp-logical", Title: "build", Type: "task", Metadata: map[string]string{
			"gc.kind": "retry", "gc.root_bead_id": "gp-root", "gc.step_ref": "demo.build",
			"gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
		}},
		beads.Bead{ID: "gp-run1", Title: "build attempt 1", Type: "task", Status: "closed", Assignee: "worker-2", Labels: []string{"pool:worker"}, Metadata: map[string]string{
			"gc.kind": "retry-run", "gc.root_bead_id": "gp-root", "gc.step_ref": "demo.build.run.1",
			"gc.logical_bead_id": "gp-logical", "gc.attempt": "1", "gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
			"gc.outcome": "fail", "gc.failure_class": "transient", "gc.failure_reason": "rate_limited",
		}},
		beads.Bead{ID: "gp-eval1", Title: "build eval 1", Type: "task", Metadata: map[string]string{
			"gc.kind": "retry-eval", "gc.root_bead_id": "gp-root", "gc.step_ref": "demo.build.eval.1",
			"gc.logical_bead_id": "gp-logical", "gc.attempt": "1", "gc.max_attempts": "3", "gc.on_exhausted": "hard_fail",
		}},
	)
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{{"gp-logical", "gp-eval1"}, {"gp-eval1", "gp-run1"}} {
		if err := binding.DepAdd(edge[0], edge[1], "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, _ := onlySnapshot(t, city.to)
	city.request.FleetStopped = true
	snapshotted := bindingFingerprint(t, city.target)

	binding, err = openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	eval1, err := binding.Get("gp-eval1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := dispatch.ProcessControl(beadstest.Graph(binding), eval1, dispatch.ProcessOptions{
		Work:           beadstest.Work(binding),
		RecycleSession: func(beads.Bead) error { return nil },
	})
	if err != nil || !result.Processed || result.Action != "retry" {
		t.Fatalf("ProcessControl(transient retry-eval) = %+v, %v; want a processed retry", result, err)
	}
	minted, err := binding.List(beads.ListQuery{Metadata: map[string]string{"gc.logical_bead_id": "gp-logical"}, IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	var clones []string
	for _, b := range minted {
		switch b.Metadata["gc.step_ref"] {
		case "demo.build.run.2", "demo.build.eval.2":
			clones = append(clones, b.ID)
			if stamp := b.Metadata[infraMigrationStampKey]; stamp != "" {
				t.Errorf("retry clone %s carries the import stamp %q of the row it was cloned from", b.ID, stamp)
			}
		}
	}
	if len(clones) != 2 {
		t.Fatalf("attempt 2 rows minted: %v, want the retry's run and eval", clones)
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatal(err)
	}

	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	opened := stubRigScopeStore(t, beads.NewMemStore())
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatalf("restore after a retry of a workflow imported from gp minted %v: exit %d: %s", clones, code, stderr)
	}
	if len(*opened) != 0 || provider.starts != 0 {
		t.Fatalf("rig stores read: %v, provider starts: %d; want none: every row imported from gp is in the snapshot", *opened, provider.starts)
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored %v, want %v", got, snapshotted)
	}
}

// TestStorageSnapshotRestoreOverADamagedBindingWithARig: damage is what a
// restore is often for. On a city with a rig, the content check cannot open a
// live binding whose header is gone; the restore warns that its rig imports
// went unchecked and restores, keeping the damaged database aside. It reads
// no rig store, so it starts no provider.
func TestStorageSnapshotRestoreOverADamagedBindingWithARig(t *testing.T) {
	city, id, snapshotted := snapshottedCity(t)
	city.request.Cfg.Rigs = []config.Rig{{Name: "gp", Path: t.TempDir()}}
	provider := &migrationWindowProvider{down: true}
	stubMigrationWindowProvider(t, provider)
	opened := stubRigScopeStore(t, beads.NewMemStore())
	damaged := mustReadFile(t, city.target.Database)
	copy(damaged, bytes.Repeat([]byte{0xff}, 100))
	if err := os.WriteFile(city.target.Database, damaged, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshotRestore(context.Background(), city.request, city.to, id, "restore", &stdout, &stderr); code != 0 {
		t.Fatalf("restore over a damaged binding on a city with a rig: exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: the live binding cannot be read") || !strings.Contains(stderr.String(), "not checked against the snapshot") {
		t.Errorf("the restore did not say the damaged binding's rig imports went unchecked: %s", stderr.String())
	}
	if len(*opened) != 0 || provider.starts != 0 {
		t.Errorf("rig stores read: %v, provider starts: %d; want none", *opened, provider.starts)
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored %v, want %v", got, snapshotted)
	}
	aside, _ := filepath.Glob(city.target.Database + ".pre-restore-*")
	if len(aside) != 1 || !bytes.Equal(mustReadFile(t, aside[0]), damaged) {
		t.Fatalf("the damaged database is not kept aside as it was: %v", aside)
	}
	if !strings.Contains(stdout.String(), aside[0]) {
		t.Errorf("the restore does not name %s, where it kept the damaged database:\n%s", aside[0], stdout.String())
	}
}

// TestStorageSnapshotRestoreSupersedesAnUnfinishedRecopy: a re-copy that
// stopped part way leaves infra.recopying, which holds the binding stale under
// a remedy (the re-copy) that refuses the populated database a restore
// installs. The installed snapshot is a whole binding, so the restore removes
// that record, before its own: the binding converges. A restore stopped
// between the two removals is still the one the stale binding names, and
// running it again finishes.
func TestStorageSnapshotRestoreSupersedesAnUnfinishedRecopy(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(fmt.Sprintf("killed between the removals: %v", killed), func(t *testing.T) {
			city, id, snapshotted := snapshottedCity(t)
			if err := writeInfraRecopyRecord(city.target, infraMigrationFromBackup); err != nil {
				t.Fatal(err)
			}
			if cause := infraStaleCause(city.target); !strings.Contains(cause, "re-copy") {
				t.Fatalf("the fixture's binding is not held stale by the re-copy: %q", cause)
			}
			if killed {
				prev := storageSnapshotStep
				t.Cleanup(func() { storageSnapshotStep = prev })
				storageSnapshotStep = func(step string) error {
					if step == "restore installed" {
						return errors.New("killed")
					}
					return nil
				}
				if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "held stale until this restore finishes") {
					t.Fatalf("exit %d: %s", code, stderr)
				}
				if _, err := os.Stat(city.target.RecopyRecordPath()); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("the re-copy's record outlived the install")
				}
				if remedy := infraStaleBindingRemedy(city.target); !strings.Contains(remedy, "gc storage snapshot restore "+id) {
					t.Fatalf("a restore stopped past the install leaves the remedy %q, want the restore", remedy)
				}
				storageSnapshotStep = prev
			}
			if code, stderr := city.restore(t, id); code != 0 {
				t.Fatal(stderr)
			}
			if state, err := readInfraConvergenceState(city.target); err != nil || state != infraConvergenceMarked {
				t.Fatalf("after the restore the binding is %v (%v), want marked", state, err)
			}
			for _, record := range []string{city.target.RecopyRecordPath(), city.target.RestoreRecordPath()} {
				if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s survived the restore", record)
				}
			}
			if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
				t.Fatalf("restored %v, want %v", got, snapshotted)
			}
		})
	}
}

// TestStorageSnapshotRestoreRerunKeepsTheEarlierRunsFiles: a restore stopped
// after it moved the live database aside leaves the only record naming that
// file. A re-run that stops before it moves anything must not lose it:
// killed, its own record still names the file; failing, it puts the earlier
// record back. The run that finishes names it among the files kept, and the
// rows written since the snapshot are in it.
func TestStorageSnapshotRestoreRerunKeepsTheEarlierRunsFiles(t *testing.T) {
	city, id, snapshotted := snapshottedCity(t)
	since := addBindingRows(t, city.target, 1)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	storageSnapshotStep = func(step string) error {
		if step == "restore moved beads.sqlite" {
			return errors.New("killed")
		}
		return nil
	}
	if code, _ := city.restore(t, id); code == 0 {
		t.Fatal("the killed restore succeeded")
	}
	moved, _ := filepath.Glob(city.target.Database + ".pre-restore-*")
	if len(moved) != 1 {
		t.Fatalf("files aside after the first run: %v, want the database", moved)
	}
	first := mustReadFile(t, city.target.RestoreRecordPath())

	var killedCause string
	storageSnapshotStep = func(step string) error {
		if step == "restore copied" {
			killedCause = infraStaleCause(city.target)
			return errors.New("killed")
		}
		return nil
	}
	if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "Nothing was moved") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(killedCause, "moved aside as "+moved[0]) {
		t.Errorf("a re-run killed before its moves leaves the cause %q, which does not name %s", killedCause, moved[0])
	}
	if !bytes.Equal(mustReadFile(t, city.target.RestoreRecordPath()), first) {
		t.Error("a re-run that failed before its moves did not put the earlier record back")
	}
	storageSnapshotStep = prev

	prevTemp := snapshotRestoreCreateTemp
	t.Cleanup(func() { snapshotRestoreCreateTemp = prevTemp })
	snapshotRestoreCreateTemp = func(string, string) (*os.File, error) { return nil, errors.New("no space left on device") }
	if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "Nothing was moved") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !bytes.Equal(mustReadFile(t, city.target.RestoreRecordPath()), first) {
		t.Error("a re-run that could not make its copy did not put the earlier record back")
	}
	if cause := infraStaleCause(city.target); !strings.Contains(cause, "moved aside as "+moved[0]) {
		t.Errorf("after the failed re-run the cause is %q, which does not name %s", cause, moved[0])
	}
	if remedy := infraStaleBindingRemedy(city.target); !strings.Contains(remedy, "gc storage snapshot restore "+id) {
		t.Errorf("after the failed re-run the remedy is %q, want the restore", remedy)
	}
	snapshotRestoreCreateTemp = prevTemp

	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshotRestore(context.Background(), city.request, city.to, id, "restore", &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	if !strings.Contains(stdout.String(), moved[0]) {
		t.Errorf("the finished restore does not name %s, the database the first run moved aside:\n%s", moved[0], stdout.String())
	}
	if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(snapshotted, ",") {
		t.Fatalf("restored %v, want %v", got, snapshotted)
	}
	ids, err := beads.SQLiteBeadIDs(context.Background(), moved[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, since[0]) {
		t.Fatalf("%s does not hold %s, the row written since the snapshot", moved[0], since[0])
	}
}

// TestStorageSnapshotRestoreNamesAMissingSnapshot: an id with no directory in
// --to (the binding-snapshot order's archive is not the default directory) is
// reported as not there, with the snapshots that are, not as an interrupted
// snapshot; a --to that does not exist says so. Verify answers the same.
func TestStorageSnapshotRestoreNamesAMissingSnapshot(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	const absent = "20991231T235959Z"
	if code, stderr := city.restore(t, absent); code == 0 || !strings.Contains(stderr, "there is no snapshot "+absent+" in "+city.to+", which holds "+id) || strings.Contains(stderr, "interrupted") {
		t.Fatalf("restore of a snapshot not in --to: exit %d: %s", code, stderr)
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageSnapshotVerify(context.Background(), city.to, absent, "verify", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "there is no snapshot "+absent+" in "+city.to) {
		t.Fatalf("verify of a snapshot not in --to: exit %d: %s", code, stderr.String())
	}
	city.to = filepath.Join(t.TempDir(), "binding-archive")
	if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, city.to+" does not exist") {
		t.Fatalf("restore from a --to that does not exist: exit %d: %s", code, stderr)
	}
}

// TestStorageMigrateRefusesARecopyWhileARestoreIsRecorded is R3-7: a stale
// binding under a restore's record is the restore's to finish. A re-copy would
// race it or copy over the database it installs, so migrate refuses, naming
// the restore, and writes nothing.
func TestStorageMigrateRefusesARecopyWhileARestoreIsRecorded(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	if err := writeInfraRestoreRecord(city.target, infraRestoreRecord{Snapshot: id, Database: city.target.Database, StartedAt: time.Now().UTC(), Command: snapshotRestoreInstruction(id, city.to)}); err != nil {
		t.Fatal(err)
	}
	before := mustReadFile(t, city.target.Database)
	var log bytes.Buffer
	report := migrateInfraClasses(t, city.request.CityPath, city.request.Cfg, &log)
	if report.Outcome == infraMigrationConverged || report.Fault == nil || !strings.Contains(report.Fault.Error(), "gc storage snapshot restore "+id) {
		t.Fatalf("a re-copy under a restore's record: outcome %v, fault %v\n%s", report.Outcome, report.Fault, log.String())
	}
	if _, err := os.Stat(city.target.RecopyRecordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refused re-copy wrote its record")
	}
	if !bytes.Equal(mustReadFile(t, city.target.Database), before) {
		t.Fatal("the refused re-copy changed the binding")
	}
}

// TestStorageSnapshotRestoreRefusesAChangedCleanRecord is R3-3: a .clean
// record added since is compatible, but one the snapshot recorded that has
// changed since is not.
func TestStorageSnapshotRestoreRefusesAChangedCleanRecord(t *testing.T) {
	city := newSnapshotCity(t)
	clean := filepath.Join(city.target.Root, rigGraphCleanMarkerName("quiet"))
	if err := os.WriteFile(clean, []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
		t.Fatal(stderr)
	}
	id, _ := onlySnapshot(t, city.to)
	if err := os.WriteFile(clean, []byte("clean, then rewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	city.request.FleetStopped = true
	if code, stderr := city.restore(t, id); code == 0 || !strings.Contains(stderr, "binding/infra.rig.quiet.clean (changed since the snapshot)") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

// TestStorageSnapshotRestoreFailurePathsLeaveNoRecord is R3-8 and R3-9: a
// restore that fails creating its temp copy, or moving the live database
// aside, has changed nothing, so it leaves no record holding the binding
// stale.
func TestStorageSnapshotRestoreFailurePathsLeaveNoRecord(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T)
	}{
		{"temp copy", func(t *testing.T) {
			prev := snapshotRestoreCreateTemp
			t.Cleanup(func() { snapshotRestoreCreateTemp = prev })
			snapshotRestoreCreateTemp = func(string, string) (*os.File, error) { return nil, errors.New("disk full") }
		}},
		{"first move", func(t *testing.T) {
			prev := snapshotRestoreRename
			t.Cleanup(func() { snapshotRestoreRename = prev })
			snapshotRestoreRename = func(string, string) error { return errors.New("read-only filesystem") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, id, snapshotted := snapshottedCity(t)
			addBindingRows(t, city.target, 1)
			before := bindingFingerprint(t, city.target)
			tc.breakIt(t)
			code, stderr := city.restore(t, id)
			if code == 0 || !strings.Contains(stderr, "Nothing was moved") {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if _, err := os.Stat(city.target.RestoreRecordPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a restore that changed nothing left its record, holding the binding stale")
			}
			if state, _ := readInfraConvergenceState(city.target); state != infraConvergenceMarked {
				t.Fatalf("the binding is %v after a restore that changed nothing, want marked", state)
			}
			if got := bindingFingerprint(t, city.target); strings.Join(got, ",") != strings.Join(before, ",") || len(got) == len(snapshotted) {
				t.Fatal("the binding changed")
			}
		})
	}
}

// TestStorageSnapshotRestoresInOneSecondKeepEveryAsideFile: two restores in
// the same second each keep the database they replaced, under distinct names.
func TestStorageSnapshotRestoresInOneSecondKeepEveryAsideFile(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	prev := snapshotRestoreNow
	t.Cleanup(func() { snapshotRestoreNow = prev })
	snapshotRestoreNow = func() time.Time { return at }
	first := addBindingRows(t, city.target, 1)
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	second := addBindingRows(t, city.target, 1)
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	aside, _ := filepath.Glob(city.target.Database + ".pre-restore-*")
	sort.Strings(aside)
	if len(aside) != 2 {
		t.Fatalf("aside files = %v, want one per restore", aside)
	}
	for i, want := range [][]string{first, second} {
		ids, err := beads.SQLiteBeadIDs(context.Background(), aside[i])
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(ids, want[0]) {
			t.Errorf("%s does not hold %s, the row only its restore replaced", aside[i], want[0])
		}
	}
}

// TestStorageSnapshotRefusesWhileARestoreIsRecorded: a snapshot during a
// restore would copy a binding mid-replacement, and its retention could
// remove the snapshot the restore reads.
func TestStorageSnapshotRefusesWhileARestoreIsRecorded(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	if err := writeInfraRestoreRecord(city.target, infraRestoreRecord{Snapshot: id, Database: city.target.Database, StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true, Keep: 1}); code == 0 || !strings.Contains(stderr, "a restore of the binding is in flight") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if dirs := snapshotDirs(t, city.to); len(dirs) != 1 || dirs[0] != id {
		t.Fatalf("snapshots = %v, want the restore's %s untouched", dirs, id)
	}
}

// TestStorageSnapshotRestoreStaleMessagesTellTheTruth covers the three crash
// points' messages: nothing moved, moved aside (naming the files, for someone
// abandoning it), and installed with only the record left.
func TestStorageSnapshotRestoreStaleMessagesTellTheTruth(t *testing.T) {
	city, id, _ := snapshottedCity(t)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	var moved string
	storageSnapshotStep = func(step string) error {
		if step == "restore moved beads.sqlite" {
			moved = infraStaleCause(city.target)
			return errors.New("killed")
		}
		return nil
	}
	if code, _ := city.restore(t, id); code == 0 {
		t.Fatal("the killed restore succeeded")
	}
	if !strings.Contains(moved, "moved aside as") || !strings.Contains(moved, ".pre-restore-") || !strings.Contains(moved, "move those files back") {
		t.Fatalf("message after the first move = %q", moved)
	}
	storageSnapshotStep = prev
	if code, stderr := city.restore(t, id); code != 0 {
		t.Fatal(stderr)
	}
	// Installed, with only the record left: put a record back by hand, as a
	// crash between the install and the record's removal leaves it.
	rec := infraRestoreRecord{Snapshot: id, Database: city.target.Database, StartedAt: time.Now().UTC()}
	aside, _ := filepath.Glob(city.target.Database + ".pre-restore-*")
	if len(aside) == 0 {
		t.Fatal("no aside file")
	}
	rec.AsideSuffix = aside[0][len(city.target.Database):]
	if err := writeInfraRestoreRecord(city.target, rec); err != nil {
		t.Fatal(err)
	}
	if cause := infraStaleCause(city.target); !strings.Contains(cause, "the snapshot's database is installed") || strings.Contains(cause, "nothing has moved") {
		t.Fatalf("message after the install = %q", cause)
	}
}
