package beads

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	// hqSnapshotFileName is the published snapshot, relative to the store dir.
	hqSnapshotFileName = "snapshot.jsonl.gz"
	// hqSnapshotTmpSuffix is appended to the snapshot path for the in-flight
	// temp file before the atomic rename.
	hqSnapshotTmpSuffix = ".tmp"
	// hqDefaultSnapshotInterval is the background snapshot cadence.
	hqDefaultSnapshotInterval = 5 * time.Second
)

// hqSnapshotMeta is the first JSONL line of a snapshot: store-wide scalars and
// the dependency graph. Each subsequent line is one Bead.
type hqSnapshotMeta struct {
	Kind  string   `json:"kind"` // always "hqstore-snapshot/v1"
	Seq   int      `json:"seq"`
	Order []string `json:"order,omitempty"`
	Deps  []Dep    `json:"deps,omitempty"`
	Count int      `json:"count"` // number of bead lines that follow
}

const hqSnapshotKind = "hqstore-snapshot/v1"

// snapshotPath returns the path to the published snapshot file.
func (s *HQStore) snapshotPath() string {
	return filepath.Join(s.dir, hqSnapshotFileName)
}

// startSnapshotter launches the background snapshot goroutine. A non-positive
// interval disables periodic snapshots (Close still flushes a final snapshot).
func (s *HQStore) startSnapshotter() {
	if s.snapshotInterval <= 0 {
		return
	}
	s.snapStop = make(chan struct{})
	s.snapDone = make(chan struct{})
	go func() {
		defer close(s.snapDone)
		ticker := time.NewTicker(s.snapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.writeSnapshot(); err != nil {
					s.recordSnapshotErr(err)
				}
			case <-s.snapStop:
				return
			}
		}
	}()
}

// stopSnapshotter signals the background goroutine and waits for it to exit.
// It does not itself flush; callers that need a final snapshot must call
// writeSnapshot explicitly (Shutdown does).
func (s *HQStore) stopSnapshotter() {
	if s.snapStop == nil {
		return
	}
	close(s.snapStop)
	<-s.snapDone
	s.snapStop = nil
	s.snapDone = nil
}

// recordSnapshotErr stores the most recent background snapshot error so a
// failing disk does not crash the goroutine but is still observable via
// LastSnapshotErr.
func (s *HQStore) recordSnapshotErr(err error) {
	s.snapErrMu.Lock()
	s.snapErr = err
	s.snapErrMu.Unlock()
}

// LastSnapshotErr returns the most recent background snapshot error, or nil.
func (s *HQStore) LastSnapshotErr() error {
	s.snapErrMu.Lock()
	defer s.snapErrMu.Unlock()
	return s.snapErr
}

// Snapshot forces an immediate, synchronous snapshot flush. It is safe to call
// concurrently with writes and with the background snapshotter; the on-disk
// snapshot is published atomically. Returns an error if the store is closed or
// the write fails.
func (s *HQStore) Snapshot() error {
	s.counters.Snapshot.Add(1)
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return fmt.Errorf("snapshotting hqstore: closed")
	}
	return s.writeSnapshot()
}

// writeSnapshot captures the full store state under a read lock and writes it
// to a gzip-compressed JSONL file, then atomically renames it into place. Only
// one snapshot writes at a time (guarded by snapWriteMu) so periodic and final
// flushes cannot interleave on the same temp file.
func (s *HQStore) writeSnapshot() (err error) {
	defer func() {
		if err != nil {
			s.counters.SnapshotWriteErr.Add(1)
		}
	}()
	s.snapWriteMu.Lock()
	defer s.snapWriteMu.Unlock()

	exp := s.ExportAll()

	path := s.snapshotPath()
	tmpPath := path + hqSnapshotTmpSuffix
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating hqstore snapshot dir: %w", err)
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating hqstore snapshot temp file: %w", err)
	}
	if err := writeSnapshotStream(f, exp); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("syncing hqstore snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing hqstore snapshot: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publishing hqstore snapshot: %w", err)
	}
	return nil
}

// writeSnapshotStream encodes exp as gzip-compressed JSONL to w: a meta line
// followed by one line per bead.
func writeSnapshotStream(w io.Writer, exp HQExport) error {
	gz := gzip.NewWriter(w)
	bw := bufio.NewWriter(gz)
	enc := json.NewEncoder(bw)

	meta := hqSnapshotMeta{
		Kind:  hqSnapshotKind,
		Seq:   exp.Seq,
		Order: exp.Order,
		Deps:  exp.Deps,
		Count: len(exp.Beads),
	}
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("encoding hqstore snapshot meta: %w", err)
	}
	for i := range exp.Beads {
		if err := enc.Encode(exp.Beads[i]); err != nil {
			return fmt.Errorf("encoding hqstore snapshot bead: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flushing hqstore snapshot buffer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("closing hqstore snapshot gzip: %w", err)
	}
	return nil
}

// loadSnapshot reads the published snapshot (if any) and rebuilds in-memory
// state. A missing snapshot is not an error. The caller must hold s.mu or run
// single-threaded (Open does the latter).
func (s *HQStore) loadSnapshot() error {
	f, err := os.Open(s.snapshotPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening hqstore snapshot: %w", err)
	}
	defer f.Close() //nolint:errcheck

	exp, err := readSnapshotStream(f)
	if err != nil {
		return err
	}
	s.loadExportLocked(exp)
	return nil
}

// readSnapshotStream decodes a gzip-compressed JSONL snapshot from r.
func readSnapshotStream(r io.Reader) (HQExport, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return HQExport{}, fmt.Errorf("opening hqstore snapshot gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck

	dec := json.NewDecoder(bufio.NewReader(gz))
	var meta hqSnapshotMeta
	if err := dec.Decode(&meta); err != nil {
		return HQExport{}, fmt.Errorf("decoding hqstore snapshot meta: %w", err)
	}
	if meta.Kind != hqSnapshotKind {
		return HQExport{}, fmt.Errorf("decoding hqstore snapshot: unknown kind %q", meta.Kind)
	}
	exp := HQExport{
		Seq:   meta.Seq,
		Order: meta.Order,
		Deps:  meta.Deps,
		Beads: make([]Bead, 0, meta.Count),
	}
	for {
		var b Bead
		if err := dec.Decode(&b); err != nil {
			if err == io.EOF {
				break
			}
			return HQExport{}, fmt.Errorf("decoding hqstore snapshot bead: %w", err)
		}
		exp.Beads = append(exp.Beads, b)
	}
	return exp, nil
}
