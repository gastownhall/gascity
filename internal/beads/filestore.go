package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fileData is the on-disk JSON format for the bead store.
type fileData struct {
	Seq   int    `json:"seq"`
	Beads []Bead `json:"beads"`
	Deps  []Dep  `json:"deps,omitempty"`
}

// FileStore is a file-backed Store implementation. It embeds a MemStore for
// all bead logic and adds JSON persistence — load on open, flush on every
// write. Fine for Tutorial 01 volumes.
type FileStore struct {
	*MemStore
	fmu    sync.Mutex // guards mutate-then-save atomicity
	fs     fsys.FS
	path   string
	locker Locker // cross-process file lock; nopLocker when unset
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
			return &FileStore{MemStore: NewMemStore(), fs: fs, path: path, locker: locker}, nil
		}
		return nil, fmt.Errorf("opening file store: %w", err)
	}

	var fd fileData
	if err := json.Unmarshal(data, &fd); err != nil {
		return nil, fmt.Errorf("opening file store: %w", err)
	}
	return &FileStore{MemStore: NewMemStoreFrom(fd.Seq, fd.Beads, fd.Deps), fs: fs, path: path, locker: locker}, nil
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
	fs.restoreFrom(fd.Seq, fd.Beads, fd.Deps)
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

	fd := fileData{Seq: seq, Beads: beads, Deps: deps}
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
	return nil
}
