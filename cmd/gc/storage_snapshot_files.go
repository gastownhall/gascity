package main

// The files of a storage snapshot (storage_snapshot.go): the manifest and its
// proof, the binding root's records, ids, the directory's lock, the sweep of
// interrupted snapshots, retention and its pins, and the disk preflight.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// storageSnapshotManifestName is written last; its presence is what makes
	// a snapshot directory a snapshot.
	storageSnapshotManifestName = "manifest.json"
	// storageSnapshotManifestFormat versions manifest.json.
	storageSnapshotManifestFormat = "gc.storage.snapshot.manifest/v1"
	// storageSnapshotJSONLFormat versions the universal JSONL.
	storageSnapshotJSONLFormat = "gc.storage.snapshot/v1"
	// storageSnapshotLockName serializes snapshots, their sweep, retention
	// and a restore reading one, in one directory.
	storageSnapshotLockName = ".snapshot.lock"
	// storageSnapshotPinName in a snapshot's directory keeps retention from
	// counting or removing it (S5-H pins the split's mandatory snapshot).
	storageSnapshotPinName = "pinned"
	// storageSnapshotWALGrowthFloor is the least WAL growth a binding
	// snapshot allows for: a live writer keeps appending while the copy's read
	// transaction holds the checkpoint back.
	storageSnapshotWALGrowthFloor = 64 << 20
)

// snapshotJSONLNote is the manifest's account of what the JSONL is for.
const snapshotJSONLNote = "the JSONL files are a record of every row, edge and payload; no tool restores a store from them yet, so for a store with no exact copy (a generic exec store) this snapshot is not a way back to it"

// storageSnapshotIDPattern is a snapshot id: a UTC second, and a suffix when
// two land in one second. Sweep and retention touch nothing else.
var storageSnapshotIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]+)?$`)

// snapshotManifest is manifest.json.
type snapshotManifest struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// GCVersion is the binary that wrote the snapshot.
	GCVersion string `json:"gc_version"`
	City      string `json:"city"`
	// Identity is the city work store's identity tuple (storage_store_identity.go).
	Identity string `json:"identity"`
	// JSONLRestorable is false: no tool restores a store from the JSONL yet.
	JSONLRestorable bool `json:"jsonl_restorable"`
	// Binding is the binding root's records when the binding was snapshotted;
	// a restore refuses when they have changed since.
	Binding *snapshotBindingRecords `json:"binding,omitempty"`
	Stores  []snapshotStoreRecord   `json:"stores"`
	Notes   []string                `json:"notes,omitempty"`
}

// snapshotBindingRecords is what the binding's database is only half of: the
// records beside it and the city's cleared notes, which say what the database
// holds and what it was carried from, and the id floor.
type snapshotBindingRecords struct {
	Records       []snapshotRecord `json:"records"`
	SequenceFloor int64            `json:"sequence_floor"`
}

// snapshotRecord is one record file by its label and sha256.
type snapshotRecord struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// snapshotStoreRecord is one store in a snapshot.
type snapshotStoreRecord struct {
	// Name is binding, work, or rig:<name>.
	Name     string        `json:"name"`
	Provider string        `json:"provider"`
	Exact    snapshotExact `json:"exact"`
	JSONL    snapshotFile  `json:"jsonl"`
	Rows     int           `json:"rows"`
	Edges    int           `json:"edges"`
	Payloads string        `json:"payloads"`
	Notes    []string      `json:"notes,omitempty"`
}

// snapshotExact is a store's exact copy.
type snapshotExact struct {
	Method string        `json:"method"`
	File   *snapshotFile `json:"file,omitempty"`
	Tag    string        `json:"tag,omitempty"`
	Commit string        `json:"commit,omitempty"`
}

// snapshotFile is one file in a snapshot, relative to its directory.
type snapshotFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// readSnapshotBindingRecords reads the binding root's records (the
// convergence marker, the copy manifest, every rig's import records) and the
// city's cleared notes by sha256, and the binding's id floor. The caller holds
// the migration guard, so none of them is being written.
func readSnapshotBindingRecords(cityPath string, target infraBindingTarget) (snapshotBindingRecords, error) {
	var out snapshotBindingRecords
	paths := map[string]string{
		"binding/" + infraMigratedMarkerName:  target.MarkerPath(),
		"binding/" + infraCopyManifestName:    target.ManifestPath(),
		"city/.gc/storage-infra-cleared.json": infraClearedNotePath(cityPath),
	}
	for _, pattern := range []string{
		filepath.Join(target.Root, rigGraphImportMarkerName("*")),
		filepath.Join(target.Root, rigGraphCleanMarkerName("*")),
		filepath.Join(target.Root, rigGraphInFlightName("*")),
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			return out, err
		}
		for _, p := range found {
			paths["binding/"+filepath.Base(p)] = p
		}
	}
	notes, err := filepath.Glob(rigGraphClearedNotePath(cityPath, "*"))
	if err != nil {
		return out, err
	}
	for _, p := range notes {
		paths["city/.gc/"+filepath.Base(p)] = p
	}
	for name, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("reading the binding record %s: %w", p, err)
		}
		out.Records = append(out.Records, snapshotRecord{Name: name, SHA256: snapshotRecordSum(name, data)})
	}
	sort.Slice(out.Records, func(i, j int) bool { return out.Records[i].Name < out.Records[j].Name })
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		return out, err
	}
	if out.SequenceFloor, err = beads.SQLiteSequenceFloorAt(target.Dir, prefix); err != nil {
		return out, fmt.Errorf("reading the binding's id floor: %w", err)
	}
	return out, nil
}

// snapshotRecordSum is a record's sha256, the convergence marker's without
// its work-store identity line: the first converged boot back-fills that line
// into a legacy marker (backfillMarkerWorkStoreIdentity), which records nothing
// new about what the database holds, so it must not make a snapshot of the
// same binding unrestorable.
func snapshotRecordSum(name string, data []byte) string {
	if name == "binding/"+infraMigratedMarkerName {
		var kept []string
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if !strings.HasPrefix(line, workStoreIdentityLinePrefix) {
				kept = append(kept, line)
			}
		}
		data = []byte(strings.Join(kept, ""))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// diffSnapshotRecords lists every record that was added, removed or changed
// between recorded and current.
func diffSnapshotRecords(recorded, current []snapshotRecord) []string {
	was := make(map[string]string, len(recorded))
	for _, r := range recorded {
		was[r.Name] = r.SHA256
	}
	var out []string
	for _, r := range current {
		sum, ok := was[r.Name]
		switch {
		case !ok:
			out = append(out, r.Name+" (written since the snapshot)")
		case sum != r.SHA256:
			out = append(out, r.Name+" (changed since the snapshot)")
		}
		delete(was, r.Name)
	}
	for name := range was {
		out = append(out, name+" (gone since the snapshot)")
	}
	sort.Strings(out)
	return out
}

// writeSnapshotJSONL writes one store's universal JSONL in the snapshot.
func writeSnapshotJSONL(dir, name, store string, n int, produce func(emit func(infraBackupEntry) error) error) (snapshotFile, error) {
	written, err := writeInfraBackupFile(dir, filepath.Join(dir, name), storageSnapshotJSONLFormat, store, "snapshot JSONL", n, produce)
	if err != nil {
		return snapshotFile{}, err
	}
	return snapshotFile{Path: name, Bytes: written.Bytes, SHA256: written.SHA256}, nil
}

// countSnapshotJSONL counts the rows and edges of a universal JSONL.
func countSnapshotJSONL(path string) (rows, edges int, err error) {
	rows, present, err := scanInfraBackupFile(path, storageSnapshotJSONLFormat, "snapshot JSONL", func(entry infraBackupEntry) error {
		edges += len(entry.Deps) + len(entry.Dependents)
		return nil
	})
	if err == nil && !present {
		err = fmt.Errorf("%s is missing", path)
	}
	return rows, edges, err
}

// hashSnapshotFile records a file already in the snapshot.
func hashSnapshotFile(dir, path string) (snapshotFile, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return snapshotFile{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return snapshotFile{}, err
	}
	defer f.Close() //nolint:errcheck // read-only
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return snapshotFile{}, fmt.Errorf("hashing %s: %w", path, err)
	}
	return snapshotFile{Path: rel, Bytes: n, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// syncAndHashSnapshotFile fsyncs a file another program wrote into the
// snapshot, then records it.
func syncAndHashSnapshotFile(dir, path string) (snapshotFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return snapshotFile{}, err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return snapshotFile{}, err
	}
	return hashSnapshotFile(dir, path)
}

// copySnapshotFile copies src into the snapshot at rel, fsynced.
func copySnapshotFile(dir, src, rel string) (snapshotFile, error) {
	dst := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return snapshotFile{}, err
	}
	if err := copyFileSynced(src, dst); err != nil {
		return snapshotFile{}, err
	}
	return hashSnapshotFile(dir, dst)
}

func copyFileSynced(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	return nil
}

// publishSnapshotManifest proves every file the manifest names, re-read from
// disk, and only then writes the manifest, atomically, and reads it back.
func publishSnapshotManifest(ctx context.Context, dir string, manifest snapshotManifest) error {
	if err := verifySnapshotFiles(ctx, dir, manifest); err != nil {
		return err
	}
	if err := storageSnapshotStep("before manifest"); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, storageSnapshotManifestName+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	path := filepath.Join(dir, storageSnapshotManifestName)
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing the manifest: %w", err)
	}
	syncInfraDir(dir)
	syncInfraDir(filepath.Dir(dir))
	back, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the manifest back: %w", err)
	}
	if !bytes.Equal(back, data) {
		return fmt.Errorf("the manifest %s does not read back as written", path)
	}
	return nil
}

// readSnapshotManifest reads a snapshot's manifest. A directory without one
// is an interrupted snapshot; no directory at all is no snapshot.
func readSnapshotManifest(dir string) (snapshotManifest, error) {
	var m snapshotManifest
	data, err := os.ReadFile(filepath.Join(dir, storageSnapshotManifestName))
	if errors.Is(err, os.ErrNotExist) {
		if err := snapshotPresent(filepath.Dir(dir), filepath.Base(dir)); err != nil {
			return m, err
		}
		return m, fmt.Errorf("%s has no %s: it is an interrupted snapshot, not a snapshot", dir, storageSnapshotManifestName)
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("reading %s: %w", filepath.Join(dir, storageSnapshotManifestName), err)
	}
	if m.Format != storageSnapshotManifestFormat {
		return m, fmt.Errorf("%s: manifest format %q, want %q", dir, m.Format, storageSnapshotManifestFormat)
	}
	if m.ID != filepath.Base(dir) {
		return m, fmt.Errorf("%s: manifest names snapshot %q", dir, m.ID)
	}
	return m, nil
}

// verifySnapshotFiles proves every file a manifest names: size and sha256,
// integrity_check on a SQLite copy, and the JSONL's header and counts.
func verifySnapshotFiles(ctx context.Context, dir string, m snapshotManifest) error {
	if len(m.Stores) == 0 {
		return errors.New("the manifest records no store")
	}
	for _, rec := range m.Stores {
		if rec.Exact.File != nil {
			if err := verifySnapshotFile(dir, *rec.Exact.File); err != nil {
				return fmt.Errorf("%s: %w", rec.Name, err)
			}
			if rec.Exact.Method == snapshotMethodVacuumInto {
				if err := beads.SQLiteIntegrityCheck(ctx, filepath.Join(dir, rec.Exact.File.Path)); err != nil {
					return fmt.Errorf("%s: %w", rec.Name, err)
				}
			}
		} else if rec.Exact.Method != snapshotMethodDoltTag && rec.Exact.Method != snapshotMethodNone {
			return fmt.Errorf("%s: %s records no file", rec.Name, rec.Exact.Method)
		}
		if err := verifySnapshotFile(dir, rec.JSONL); err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		rows, edges, err := countSnapshotJSONL(filepath.Join(dir, rec.JSONL.Path))
		if err != nil {
			return fmt.Errorf("%s: %w", rec.Name, err)
		}
		if rows != rec.Rows || edges != rec.Edges {
			return fmt.Errorf("%s: the JSONL holds %d row(s) and %d edge(s), the manifest records %d and %d", rec.Name, rows, edges, rec.Rows, rec.Edges)
		}
	}
	return nil
}

func verifySnapshotFile(dir string, want snapshotFile) error {
	if want.Path == "" || filepath.IsAbs(want.Path) || strings.HasPrefix(filepath.Clean(want.Path), "..") {
		return fmt.Errorf("the manifest names %q, which is not a file inside the snapshot", want.Path)
	}
	got, err := hashSnapshotFile(dir, filepath.Join(dir, want.Path))
	if err != nil {
		return err
	}
	if got.Bytes != want.Bytes || got.SHA256 != want.SHA256 {
		return fmt.Errorf("%s is %d bytes, sha256 %s; the manifest records %d bytes, sha256 %s", want.Path, got.Bytes, got.SHA256, want.Bytes, want.SHA256)
	}
	return nil
}

// doStorageSnapshotVerify re-reads one snapshot and proves it. It writes
// nothing and takes no lock.
func doStorageSnapshotVerify(ctx context.Context, to, id, logPrefix string, stdout, stderr io.Writer) int {
	if !storageSnapshotIDPattern.MatchString(id) {
		fmt.Fprintf(stderr, "%s: %q is not a snapshot id\n", logPrefix, id) //nolint:errcheck // best-effort stderr
		return 1
	}
	dir := filepath.Join(to, id)
	m, err := readSnapshotManifest(dir)
	if err == nil {
		err = verifySnapshotFiles(ctx, dir, m)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: snapshot %s does NOT verify: %v\n", logPrefix, id, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Snapshot %s verifies: %d store(s), written %s by gc %s.\n", id, len(m.Stores), m.CreatedAt.Format(time.RFC3339), m.GCVersion) //nolint:errcheck // best-effort stdout
	for _, rec := range m.Stores {
		if rec.Exact.Method == snapshotMethodDoltTag {
			fmt.Fprintf(stdout, "  %s: the Dolt tag %s lives in the store and is not re-checked here\n", rec.Name, rec.Exact.Tag) //nolint:errcheck // best-effort stdout
		}
	}
	return 0
}

// newSnapshotID is now's id, suffixed when a snapshot of that second exists.
func newSnapshotID(to string, now time.Time) (string, error) {
	base := now.UTC().Format("20060102T150405Z")
	id := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(filepath.Join(to, id)); errors.Is(err, os.ErrNotExist) {
			return id, nil
		} else if err != nil {
			return "", err
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

// lockSnapshotDir takes the directory's snapshot flock, refusing when another
// snapshot or a restore holds it.
func lockSnapshotDir(to string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(to, storageSnapshotLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another snapshot or restore is using %s; run this again when it is done", to)
	}
	return f, nil
}

// snapshotIDs lists the snapshot directories in to and whether each is
// complete (has a manifest).
func snapshotIDs(to string) (complete, interrupted []string, err error) {
	entries, err := os.ReadDir(to)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !storageSnapshotIDPattern.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(filepath.Join(to, e.Name(), storageSnapshotManifestName)); err == nil {
			complete = append(complete, e.Name())
		} else {
			interrupted = append(interrupted, e.Name())
		}
	}
	return complete, interrupted, nil
}

// snapshotPresent refuses an id with no directory in to, naming the complete
// snapshots there, newest first: a snapshot looked for in the wrong directory
// is missing, not interrupted.
func snapshotPresent(to, id string) error {
	if _, err := os.Stat(filepath.Join(to, id)); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	complete, _, err := snapshotIDs(to)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("there is no snapshot %s: %s does not exist. Pass --to with the directory the snapshot was written to", id, to)
	case err != nil:
		return fmt.Errorf("there is no snapshot %s in %s, which cannot be listed: %w", id, to, err)
	case len(complete) == 0:
		return fmt.Errorf("there is no snapshot %s in %s, which holds none. Pass --to with the directory the snapshot was written to", id, to)
	}
	sort.Slice(complete, func(i, j int) bool { return snapshotIDLess(complete[j], complete[i]) })
	return fmt.Errorf("there is no snapshot %s in %s, which holds %s", id, to, infraStrandedIDList(complete))
}

// sweepInterruptedSnapshots removes every snapshot directory in to that has
// no manifest: a run a SIGKILL ended. The caller holds the directory's lock,
// so none of them is still being written.
func sweepInterruptedSnapshots(to string) ([]string, error) {
	_, interrupted, err := snapshotIDs(to)
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, name := range interrupted {
		if err := os.RemoveAll(filepath.Join(to, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

// snapshotOwner is the city a snapshot belongs to: its path and its work
// store's identity, as the manifest records them.
type snapshotOwner struct {
	City     string
	Identity workStoreIdentity
}

// owns reports whether m is a snapshot of this owner's city: the same city
// path and the same work store, compared as boot compares a marker
// (workStoreIdentityMismatch).
func (o snapshotOwner) owns(m snapshotManifest) bool {
	if !samePath(m.City, o.City) {
		return false
	}
	recorded, err := parseWorkStoreIdentity(m.Identity)
	if err != nil {
		return false
	}
	return workStoreIdentityMismatch(recorded, o.Identity) == ""
}

// applySnapshotRetention keeps own (the snapshot this run wrote) and the
// newest keep-1 of owner's other complete, unpinned snapshots in to, and
// removes owner's other unpinned ones. Snapshots of another city or another
// work store (a --to several cities share), and complete directories whose
// manifest does not read, are neither counted nor removed: retention removes
// only what it can prove is this city's. A pinned snapshot is never counted
// or removed. Only directories named as snapshot ids are touched, under the
// directory's lock.
func applySnapshotRetention(to string, keep int, own string, owner snapshotOwner) ([]string, error) {
	if keep < 1 {
		return nil, fmt.Errorf("retention keeps at least the snapshot just written; --keep %d", keep)
	}
	complete, _, err := snapshotIDs(to)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, name := range complete {
		if name == own {
			continue
		}
		if _, err := os.Stat(filepath.Join(to, name, storageSnapshotPinName)); err == nil {
			continue
		}
		if m, err := readSnapshotManifest(filepath.Join(to, name)); err != nil || !owner.owns(m) {
			continue
		}
		candidates = append(candidates, name)
	}
	sort.Slice(candidates, func(i, j int) bool { return snapshotIDLess(candidates[j], candidates[i]) })
	if len(candidates) <= keep-1 {
		return nil, nil
	}
	var removed []string
	var errs []error
	for _, name := range candidates[keep-1:] {
		if err := os.RemoveAll(filepath.Join(to, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

// pinStorageSnapshot pins a complete snapshot so retention keeps it. It has
// no caller in this slice on purpose: S5-H's journal pins the split's
// mandatory snapshot with it until the split is done, and retention honors
// the pin from now, so an operator's hand-written pin works today. The pin is
// a file, so an operator can see and remove it.
func pinStorageSnapshot(to, id, reason string) error {
	dir := filepath.Join(to, id)
	if _, err := readSnapshotManifest(dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, storageSnapshotPinName), []byte(strings.TrimSpace(reason)+"\n"), 0o644)
}

// snapshotIDLess orders ids by time, then by suffix.
func snapshotIDLess(a, b string) bool {
	baseA, sufA, _ := strings.Cut(a, "-")
	baseB, sufB, _ := strings.Cut(b, "-")
	if baseA != baseB {
		return baseA < baseB
	}
	if len(sufA) != len(sufB) {
		return len(sufA) < len(sufB)
	}
	return sufA < sufB
}

// snapshotDiskNeed is the space a binding snapshot takes: the copy (at most
// the database and its WAL), the JSONL (estimated at that again), and the WAL
// a live writer grows while the copy's read transaction holds the checkpoint
// back (at least storageSnapshotWALGrowthFloor, or the WAL's current size).
func snapshotDiskNeed(db, wal int64) int64 {
	return 2*(db+wal) + max(wal, storageSnapshotWALGrowthFloor)
}

// snapshotDiskVerdict judges free space against need, leaving the managed-Dolt
// critical floor free after the snapshot (a Dolt on the same filesystem stops
// below it) and warning under its warning floor.
func snapshotDiskVerdict(free, need, critical, warn int64) (refuse, low bool) {
	left := free - need
	return left < critical, left < warn
}

// snapshotDiskPreflight refuses a binding snapshot that would not fit. It runs
// after the sweep, so the interrupted snapshots it removed count as free.
func snapshotDiskPreflight(to, database, logPrefix string, stderr io.Writer) error {
	var db, wal int64
	if info, err := os.Stat(database); err == nil {
		db = info.Size()
	}
	if info, err := os.Stat(database + "-wal"); err == nil {
		wal = info.Size()
	}
	free, err := doltContainerFreeBytesFunc(to)
	if err != nil {
		return nil // fail open, as the managed-Dolt preflight does
	}
	need := snapshotDiskNeed(db, wal)
	critical, warn := doltDiskMinFreeBytes(), doltDiskWarnFreeBytes()
	refuse, low := snapshotDiskVerdict(free, need, critical, warn)
	if refuse {
		return fmt.Errorf("%s has %d bytes free; a binding snapshot needs about %d (twice the database and its WAL, plus WAL growth) and must leave %d free for a managed Dolt on the same filesystem. Free space or pass --to elsewhere. Nothing was written", to, free, need, critical)
	}
	if low {
		fmt.Fprintf(stderr, "%s: disk WARN: after this snapshot %s will have about %d bytes free, under the %d warning floor\n", logPrefix, to, free-need, warn) //nolint:errcheck // best-effort stderr
	}
	return nil
}
