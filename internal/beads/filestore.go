package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fileData is the on-disk JSON format for the bead store.
type fileData struct {
	Seq   int    `json:"seq"`
	Beads []Bead `json:"beads"`
	Deps  []Dep  `json:"deps,omitempty"`
	// Revisions persists each bead's ConditionalWriter revision out of band,
	// because Bead.Revision is json:"-" and never survives the on-disk []Bead.
	// Without this, every reloadFromDisk (which runs before each write in
	// cross-process flock mode) would reset all revisions to 0, breaking the
	// monotonic-never-reused contract. Absent (legacy files) ≡ all zero.
	Revisions map[string]int64 `json:"revisions,omitempty"`
	// RevisionsSealed marks a file written by a revisions-aware binary. An
	// OLDER binary's full rewrite drops both this marker and the revisions
	// map while keeping the beads — the exact state in which fresh-from-zero
	// revisions would REUSE previously issued tokens and break the
	// monotonic-never-reused contract. Loading an unsealed file with beads
	// therefore re-seeds every revision at a deterministic floor far above
	// any counter a prior writer could have issued (see
	// applyBeadRevisionsSealed).
	RevisionsSealed bool `json:"revisions_sealed,omitempty"`
}

// beadRevisions extracts the out-of-band revision map for persistence. Zero
// revisions are omitted (absent ≡ 0 on reload), so legacy files round-trip.
func beadRevisions(beads []Bead) map[string]int64 {
	revs := make(map[string]int64, len(beads))
	for _, b := range beads {
		if b.Revision != 0 {
			revs[b.ID] = b.Revision
		}
	}
	if len(revs) == 0 {
		return nil
	}
	return revs
}

// applyBeadRevisions stamps persisted revisions back onto beads decoded from
// disk, whose Revision fields are all 0 because of the json:"-" tag. Beads with
// no entry keep revision 0, matching files that predate the revisions map.
func applyBeadRevisions(beads []Bead, revs map[string]int64) {
	if len(revs) == 0 {
		return
	}
	for i := range beads {
		if r, ok := revs[beads[i].ID]; ok {
			beads[i].Revision = r
		}
	}
}

// revisionContinuityFloor is the minimum re-seed value for revisions on an
// unsealed file. Any prior writer issued small per-mutation counters, so a
// floor around 2^40 (≈10^12) can never collide with a previously issued
// token, while staying far below int64 overflow for subsequent +1 bumps.
const revisionContinuityFloor = int64(1) << 40

// applyBeadRevisionsSealed applies the persisted revisions and, when the file
// is UNSEALED but carries beads (a pre-revisions legacy file, or a file an
// older binary rewrote — dropping the revisions map and the seal), re-seeds
// every bead's revision at a deterministic floor derived from the file's own
// timestamps. Determinism matters: the seed must be identical across
// processes and repeated reloads of the same bytes, or the reload-before-
// write path would invent a new revision on every pass and spuriously fail
// in-flight fences. The floor guarantees no previously issued token is ever
// reused; per-bead monotonicity resumes with ordinary +1 bumps from there.
func applyBeadRevisionsSealed(fd *fileData) {
	applyBeadRevisions(fd.Beads, fd.Revisions)
	if fd.RevisionsSealed || len(fd.Beads) == 0 {
		return
	}
	seed := revisionContinuityFloor
	for _, b := range fd.Beads {
		if v := b.UpdatedAt.UnixNano(); !b.UpdatedAt.IsZero() && v > seed {
			seed = v
		}
		if v := b.CreatedAt.UnixNano(); !b.CreatedAt.IsZero() && v > seed {
			seed = v
		}
	}
	for i := range fd.Beads {
		if fd.Beads[i].Revision < seed {
			fd.Beads[i].Revision = seed
		}
	}
}

// FileStore is a file-backed Store implementation. It embeds a MemStore for
// all bead logic and adds JSON persistence — load on open, flush on every
// write. Fine for Tutorial 01 volumes.
type FileStore struct {
	*MemStore
	fmu       sync.Mutex // guards mutate-then-save atomicity
	fs        fsys.FS
	path      string
	locker    Locker // cross-process file lock; nopLocker when unset
	freshness fileFreshness
}

var _ ConditionalAssignmentReleaser = (*FileStore)(nil)

type fileFreshness struct {
	known   bool
	exists  bool
	size    int64
	modTime time.Time
}

func (f fileFreshness) same(other fileFreshness) bool {
	if !f.known || !other.known {
		return false
	}
	if f.exists != other.exists {
		return false
	}
	if !f.exists {
		return true
	}
	return f.size == other.size && f.modTime.Equal(other.modTime)
}

// OpenFileStore opens or creates a file-backed bead store at path. All file
// I/O goes through fs for testability. If the file exists, its contents are
// loaded into memory. If it doesn't exist, the store starts empty. Parent
// directories are created as needed.
func OpenFileStore(fs fsys.FS, path string) (*FileStore, error) {
	if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	locker := Locker(nopLocker{})
	if _, ok := fs.(fsys.OSFS); ok {
		locker = NewFileFlock(path + ".lock")
	}

	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileStore{
				MemStore:  NewMemStore(),
				fs:        fs,
				path:      path,
				locker:    locker,
				freshness: fileFreshness{known: true},
			}, nil
		}
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}
	applyBeadRevisionsSealed(&fd)
	store := &FileStore{
		MemStore: NewMemStoreFrom(fd.Seq, fd.Beads, fd.Deps),
		fs:       fs,
		path:     path,
		locker:   locker,
	}
	// The JSON we just loaded and the file's current freshness can diverge if
	// another handle rewrites the store between ReadFile and a follow-up Stat.
	// Leave the cache unknown so the first read revalidates against disk.
	store.freshness = fileFreshness{}
	return store, nil
}

// SetLocker sets a cross-process Locker (typically a FileFlock). When set,
// every mutating operation acquires the lock and reloads from disk before
// writing — preventing ID collisions between the CLI and controller daemon.
func (fs *FileStore) SetLocker(l Locker) {
	fs.locker = l
}

// reloadFromDisk re-reads the store file and replaces the in-memory state.
// Must be called with fmu held. Used after acquiring a cross-process flock to
// pick up changes made by other processes since we last read.
func (fs *FileStore) reloadFromDisk() error {
	data, err := fs.fs.ReadFile(fs.path)
	if err != nil {
		if os.IsNotExist(err) {
			// File hasn't been created yet — keep current in-memory state.
			return nil
		}
		return fmt.Errorf("reloading file store: %w", err)
	}
	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return fmt.Errorf("reloading file store: %w", err)
	}
	applyBeadRevisionsSealed(&fd)
	fs.restoreFrom(fd.Seq, fd.Beads, fd.Deps)
	return nil
}

func (fs *FileStore) currentFreshness() (fileFreshness, error) {
	fi, err := fs.fs.Stat(fs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileFreshness{known: true}, nil
		}
		return fileFreshness{}, fmt.Errorf("stating file store: %w", err)
	}
	return fileFreshness{
		known:   true,
		exists:  true,
		size:    fi.Size(),
		modTime: fi.ModTime(),
	}, nil
}

func (fs *FileStore) refreshFreshnessCache() {
	current, err := fs.currentFreshness()
	if err != nil {
		fs.freshness = fileFreshness{}
		return
	}
	fs.freshness = current
}

// refreshReadStateLocked favors cross-process correctness for long-lived
// readers, but uses an mtime+size fast path to avoid full JSON reloads on
// every read. The remaining per-read Stat cost is acceptable for now; if
// polling latency becomes measurable, we can replace it with a lighter seq hint.
// Read wrappers intentionally skip the cross-process locker because writers
// publish complete JSON files with temp-file-plus-rename atomic replacement.
func (fs *FileStore) refreshReadStateLocked() error {
	current, err := fs.currentFreshness()
	if err != nil {
		if err := fs.reloadFromDisk(); err != nil {
			return err
		}
		fs.freshness = fileFreshness{}
		return nil
	}
	if fs.freshness.same(current) {
		return nil
	}
	if !current.exists {
		fs.restoreFrom(0, nil, nil)
		fs.freshness = current
		return nil
	}
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	fs.freshness = current
	return nil
}

// Create delegates to MemStore.Create and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back to keep
// the MemStore and file in sync.
func (fs *FileStore) Create(b Bead) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return Bead{}, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return Bead{}, err
	}
	snap := fs.snapshotLocked()
	result, err := fs.MemStore.Create(b)
	if err != nil {
		return Bead{}, err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return Bead{}, err
	}
	return result, nil
}

// Update delegates to MemStore.Update and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Update(id string, opts UpdateOpts) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.Update(id, opts); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still
// has the expected assignee, while holding the file-store write lock.
func (fs *FileStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return false, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return false, err
	}
	snap := fs.snapshotLocked()
	released, err := fs.MemStore.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil || !released {
		return released, err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return false, err
	}
	return true, nil
}

// Close delegates to MemStore.Close and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Close(id string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.Close(id); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// Reopen delegates to MemStore.Reopen and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Reopen(id string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.Reopen(id); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// Delete delegates to MemStore.Delete and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) Delete(id string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.Delete(id); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// CloseAll closes multiple beads and sets metadata, then flushes once.
func (fs *FileStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return 0, err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return 0, err
	}
	snap := fs.snapshotLocked()
	closed, err := fs.MemStore.CloseAll(ids, metadata)
	if err != nil {
		return 0, err
	}
	if closed > 0 {
		if err := fs.save(); err != nil {
			fs.restoreFrom(snap.seq, snap.beads, snap.deps)
			return 0, err
		}
	}
	return closed, nil
}

// SetMetadata delegates to MemStore.SetMetadata and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) SetMetadata(id, key, value string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.SetMetadata(id, key, value); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// SetMetadataBatch delegates to MemStore.SetMetadataBatch and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) SetMetadataBatch(id string, kvs map[string]string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// Tx executes fn sequentially against the FileStore.
func (fs *FileStore) Tx(_ string, fn func(Tx) error) error {
	return runSequentialTx(fs, fn)
}

// Get reloads the on-disk store before reading a bead by ID.
func (fs *FileStore) Get(id string) (Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return Bead{}, err
	}
	return fs.MemStore.Get(id)
}

// List reloads the on-disk store before listing beads that match the query.
func (fs *FileStore) List(query ListQuery) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.List(query)
}

// ListOpen reloads the on-disk store before listing open beads.
func (fs *FileStore) ListOpen(status ...string) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListOpen(status...)
}

// Ready reloads the on-disk store before listing ready beads.
func (fs *FileStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.Ready(query...)
}

// Children reloads the on-disk store before listing child beads.
func (fs *FileStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.Children(parentID, opts...)
}

// ListByLabel reloads the on-disk store before listing beads for a label.
func (fs *FileStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListByLabel(label, limit, opts...)
}

// ListByAssignee reloads the on-disk store before listing beads for an assignee.
func (fs *FileStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListByAssignee(assignee, status, limit)
}

// ListByMetadata reloads the on-disk store before listing beads by metadata.
func (fs *FileStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.ListByMetadata(filters, limit, opts...)
}

// Ping checks that the store file is accessible.
func (fs *FileStore) Ping() error {
	if err := fs.MemStore.Ping(); err != nil {
		return err
	}
	if _, err := fs.fs.ReadFile(fs.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pinging file store: %w", err)
	}
	return nil
}

// DepAdd delegates to MemStore.DepAdd and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) DepAdd(issueID, dependsOnID, depType string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// DepRemove delegates to MemStore.DepRemove and flushes to disk.
// If the disk flush fails, the in-memory mutation is rolled back.
func (fs *FileStore) DepRemove(issueID, dependsOnID string) error {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	if err := fs.MemStore.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// DepList reloads the on-disk store before listing dependencies.
func (fs *FileStore) DepList(id, direction string) ([]Dep, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.DepList(id, direction)
}

// DepListBatch reloads the on-disk store before listing batched dependencies.
func (fs *FileStore) DepListBatch(ids []string) (map[string][]Dep, error) {
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.refreshReadStateLocked(); err != nil {
		return nil, err
	}
	return fs.MemStore.DepListBatch(ids)
}

// memSnapshot holds a snapshot of MemStore state for rollback.
type memSnapshot struct {
	seq   int
	beads []Bead
	deps  []Dep
}

// snapshotLocked takes a snapshot of MemStore state for rollback.
// Must be called with fmu held.
func (fs *FileStore) snapshotLocked() memSnapshot {
	fs.mu.Lock()
	seq, beads, deps := fs.snapshot()
	fs.mu.Unlock()
	return memSnapshot{seq: seq, beads: beads, deps: deps}
}

// save writes the full store state to disk atomically (temp file + rename).
// Called with fmu held, so snapshot under MemStore.mu then release before I/O.
func (fs *FileStore) save() error {
	fs.mu.Lock()
	seq, beads, deps := fs.snapshot()
	fs.mu.Unlock()

	fd := fileData{Seq: seq, Beads: beads, Deps: deps, Revisions: beadRevisions(beads), RevisionsSealed: true}
	data, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}

	tmp := fs.path + ".tmp"
	if err := fs.fs.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	if err := fs.fs.Rename(tmp, fs.path); err != nil {
		return fmt.Errorf("saving file store: %w", err)
	}
	fs.refreshFreshnessCache()
	return nil
}
