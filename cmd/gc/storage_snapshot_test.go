package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	binding := openMigratedDestination(t, target)
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
func snapshotIDs(t *testing.T, to string) []string {
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
	ids := snapshotIDs(t, to)
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
	before := snapshotIDs(t, city.to)
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
	after := snapshotIDs(t, city.to)
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
	for _, tc := range []struct {
		name     string
		complete []string
		partial  []string
		keep     int
		want     []string
	}{
		{"keeps all within N", []string{"20260101T000000Z", "20260102T000000Z"}, nil, 2, []string{"20260101T000000Z", "20260102T000000Z"}},
		{"drops the oldest", []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z"}, nil, 2, []string{"20260102T000000Z", "20260103T000000Z"}},
		{"suffix orders after its second", []string{"20260101T000000Z", "20260101T000000Z-2", "20260101T000000Z-10"}, nil, 1, []string{"20260101T000000Z-10"}},
		{"interrupted never kept", []string{"20260101T000000Z"}, []string{"20260105T000000Z"}, 3, []string{"20260101T000000Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			to := t.TempDir()
			for _, id := range tc.complete {
				if err := os.MkdirAll(filepath.Join(to, id), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(to, id, storageSnapshotManifestName), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range tc.partial {
				if err := os.MkdirAll(filepath.Join(to, id), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := applySnapshotRetention(to, tc.keep); err != nil {
				t.Fatal(err)
			}
			if got := snapshotIDs(t, to); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
		})
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
// leaves a directory verify refuses and the next snapshot removes.
func TestStorageSnapshotManifestIsWrittenLast(t *testing.T) {
	for _, failAt := range []string{"binding copied", "stored binding", "before manifest"} {
		t.Run(failAt, func(t *testing.T) {
			city := newSnapshotCity(t)
			var seen []string
			prev := storageSnapshotStep
			t.Cleanup(func() { storageSnapshotStep = prev })
			storageSnapshotStep = func(step string) error {
				seen = append(seen, step)
				for _, id := range snapshotIDs(t, city.to) {
					if _, err := os.Stat(filepath.Join(city.to, id, storageSnapshotManifestName)); err == nil {
						t.Errorf("the manifest exists at step %q, before the snapshot is proven", step)
					}
				}
				if step == failAt {
					return errors.New("injected failure")
				}
				return nil
			}
			if code, _, _ := city.snapshot(t, snapshotOptions{Binding: true}); code == 0 {
				t.Fatalf("a run failing at %q succeeded (steps %v)", failAt, seen)
			}
			ids := snapshotIDs(t, city.to)
			if len(ids) != 1 {
				t.Fatalf("snapshot dirs = %v", ids)
			}
			var stdout, stderr bytes.Buffer
			if code := doStorageSnapshotVerify(context.Background(), city.to, ids[0], "gc storage snapshot", &stdout, &stderr); code == 0 {
				t.Fatal("verify passed an interrupted snapshot")
			}
			storageSnapshotStep = prev
			if code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true}); code != 0 {
				t.Fatal(stderr)
			}
			if after := snapshotIDs(t, city.to); len(after) != 1 || after[0] == ids[0] {
				t.Fatalf("the next snapshot did not remove the interrupted one: %v", after)
			}
		})
	}
}

// TestStorageSnapshotProvesItsFilesBeforeTheManifest: a file that changes
// after it was recorded (a disk error, a stray writer) keeps the manifest from
// being written at all.
func TestStorageSnapshotProvesItsFilesBeforeTheManifest(t *testing.T) {
	city := newSnapshotCity(t)
	prev := storageSnapshotStep
	t.Cleanup(func() { storageSnapshotStep = prev })
	storageSnapshotStep = func(step string) error {
		if step == "stored binding" {
			ids := snapshotIDs(t, city.to)
			path := filepath.Join(city.to, ids[0], "binding.jsonl")
			data := mustReadFile(t, path)
			data[len(data)-3] ^= 0x01
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	code, _, stderr := city.snapshot(t, snapshotOptions{Binding: true})
	if code == 0 || !strings.Contains(stderr, "sha256") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	ids := snapshotIDs(t, city.to)
	if _, err := os.Stat(filepath.Join(city.to, ids[0], storageSnapshotManifestName)); err == nil {
		t.Fatal("a manifest was written over a file that no longer matches it")
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
	if ids := snapshotIDs(t, city.to); len(ids) != 0 {
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
	var stderr bytes.Buffer
	if _, err := installSnapshotBinding(context.Background(), src, strings.Repeat("0", 64), city.target.Database, time.Now(), "restore", &stderr); err == nil {
		t.Fatal("a copy whose hash does not match was installed")
	}
	if !bytes.Equal(mustReadFile(t, city.target.Database), before) {
		t.Fatal("the live database moved")
	}
	if left, _ := filepath.Glob(city.target.Database + ".restore-*"); len(left) != 0 {
		t.Fatalf("temp copies left: %v", left)
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
			cmd.Env = append(os.Environ(), storageSnapshotKillHelperEnv+"="+strings.Join([]string{city.request.CityPath, bindingPath, city.to, killAt}, "\t"))
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

			ids := snapshotIDs(t, city.to)
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
			after := snapshotIDs(t, city.to)
			if len(after) != 1 || after[0] == ids[0] {
				t.Fatalf("the killed snapshot was not removed: %v", after)
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

func (s *taggingStore) DoltTagCommit(name string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	for _, tag := range s.tags {
		if tag == name {
			return "abc123", true, nil
		}
	}
	return "", false, nil
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
	if rec.Exact.Method != snapshotMethodDoltTag || rec.Exact.Tag != "gc-storage-snapshot-"+id || rec.Exact.Commit != "abc123" || len(rec.Notes) != 1 {
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
	newest := snapshotIDs(t, city.to)
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
	for _, sid := range snapshotIDs(t, city.to) {
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
