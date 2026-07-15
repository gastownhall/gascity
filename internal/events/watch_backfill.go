package events

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// backfillConcurrency bounds how many watchers may actively read archive/rotating
// segments concurrently. A cold resume gunzips archives; without a cap, N
// simultaneous resumes multiply the CPU/IO cost. Excess resumes queue on this
// semaphore. The slot is held only during a single bounded batch read (not
// across the consumer's drain), so it caps concurrent decode work without
// pinning memory.
const backfillConcurrency = 2

// backfillBatch bounds how many events one backfill step buffers before yielding
// to the consumer. It caps a watcher's resident backfill memory to O(batch)
// events (not a whole archive segment), so many concurrent cold resumes cannot
// aggregate into an OOM.
const backfillBatch = 256

var backfillSlots = make(chan struct{}, backfillConcurrency)

// acquireBackfillSlot blocks for a decode slot, returning early if ctx is
// canceled or done is closed (a Close mid-wait must unblock Next).
func acquireBackfillSlot(ctx context.Context, done <-chan struct{}) error {
	select {
	case backfillSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return errWatcherClosed
	}
}

func releaseBackfillSlot() { <-backfillSlots }

// backfillSourceKind distinguishes a gzip archive from a plain-JSONL segment.
type backfillSourceKind int

const (
	sourceArchive backfillSourceKind = iota
	sourceRotating
)

// backfillSource is one on-disk segment contributing to a resume backfill,
// ordered by its first sequence. fallbackPath, set for rotating files, is the
// canonical .gz archive the recorder promotes the file to: the promotion
// (rename .gz into place, THEN remove the rotating source) can land between the
// directory listing and the open, in which case the archive did not exist at
// list time and the rotating file no longer does — reading the derived archive
// path is the only way not to lose that window.
type backfillSource struct {
	path         string
	fallbackPath string
	kind         backfillSourceKind
	firstSeq     uint64
	lastSeq      uint64
}

// listBackfillSources returns the .gz archives and in-flight rotating-* files in
// dir whose seq window may contain events with Seq > afterSeq, sorted by first
// sequence. A .gz archive and its not-yet-removed rotating source share a seq
// window; the streamed monotonic guard drops the duplicate, so both are safe to
// include.
func listBackfillSources(dir string, afterSeq uint64) ([]backfillSource, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var srcs []backfillSource
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case isCanonicalArchiveBasename(name):
			info, perr := parseArchiveBasename(name)
			if perr != nil {
				continue
			}
			if afterSeq > 0 && info.LastSeq <= afterSeq {
				continue
			}
			srcs = append(srcs, backfillSource{
				path: filepath.Join(dir, name), kind: sourceArchive,
				firstSeq: info.FirstSeq, lastSeq: info.LastSeq,
			})
		case hasRotatingPrefix(name):
			ts, first, last, ok := parseRotatingBasename(name)
			if !ok {
				continue // legacy rotating file without a window; reaper promotes it
			}
			if afterSeq > 0 && last <= afterSeq {
				continue
			}
			srcs = append(srcs, backfillSource{
				path:         filepath.Join(dir, name),
				fallbackPath: filepath.Join(dir, formatArchiveBasename(ts, first, last)),
				kind:         sourceRotating,
				firstSeq:     first, lastSeq: last,
			})
		}
	}
	sort.Slice(srcs, func(i, j int) bool {
		if srcs[i].firstSeq != srcs[j].firstSeq {
			return srcs[i].firstSeq < srcs[j].firstSeq
		}
		return srcs[i].kind < srcs[j].kind
	})
	return srcs, nil
}

// isCanonicalArchiveBasename reports whether name is a canonical .gz archive.
func isCanonicalArchiveBasename(name string) bool {
	return strings.HasPrefix(name, "events.jsonl.archive-") && strings.HasSuffix(name, ".gz")
}

// segmentReader streams events from one open backfill segment line by line via
// bufio.Reader.ReadBytes — which, unlike bufio.Scanner, imposes no maximum line
// length, so an event larger than 1 MiB cannot poison the resume. It owns the
// underlying file (and gzip stream, for archives) and is closed exactly once.
type segmentReader struct {
	f  *os.File
	gz *gzip.Reader
	br *bufio.Reader
}

// openSegmentReader opens one backfill segment. A rotating file that vanished
// between listing and open was promoted (the recorder renames the .gz into place
// BEFORE removing the rotating source), so its derived archive path is read
// instead — skipping would silently lose the window whenever the promotion lands
// in that gap, because the .gz did not exist at list time. A missing archive
// yields (nil, nil): archives are only removed by retention reaping, which never
// touches windows a live backfill can still need (and the monotonic cursor makes
// a re-listed duplicate harmless).
func openSegmentReader(src backfillSource) (*segmentReader, error) {
	f, err := os.Open(src.path)
	kind := src.kind
	if err != nil && os.IsNotExist(err) && src.fallbackPath != "" {
		f, err = os.Open(src.fallbackPath)
		kind = sourceArchive
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sr := &segmentReader{f: f}
	var r io.Reader = f
	if kind == sourceArchive {
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			_ = f.Close()
			return nil, gerr
		}
		sr.gz = gz
		r = gz
	}
	sr.br = bufio.NewReaderSize(r, 64*1024)
	return sr, nil
}

// activeSegmentReader wraps a captured active fd (0..size) as a segment reader.
func activeSegmentReader(f *os.File, size int64) (*segmentReader, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return &segmentReader{f: nil, br: bufio.NewReaderSize(&io.LimitedReader{R: f, N: size}, 64*1024)}, nil
}

// readInto reads up to batch filter-matching events with Seq > *maxSeq from the
// segment into out, advancing *maxSeq. It returns done=true at end of segment.
// Malformed and oversized-but-unparseable lines are skipped, never fatal.
func (sr *segmentReader) readInto(filter Filter, maxSeq *uint64, out *[]Event, batch int) (bool, error) {
	added := 0
	for added < batch {
		line, err := sr.br.ReadBytes('\n')
		if len(line) > 0 {
			var e Event
			if json.Unmarshal(trimLine(line), &e) == nil && matchesFilter(e, filter) && e.Seq > *maxSeq {
				*maxSeq = e.Seq
				*out = append(*out, e)
				added++
			}
		}
		if err != nil {
			if err == io.EOF {
				return true, nil
			}
			return false, err
		}
	}
	return false, nil
}

func trimLine(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// close releases the segment's gzip stream and file. Safe to call once; the
// owning fileWatcher is single-consumer, so no concurrent close occurs.
func (sr *segmentReader) close() {
	if sr == nil {
		return
	}
	if sr.gz != nil {
		_ = sr.gz.Close()
	}
	// A nil f means the reader wraps a caller-owned active fd (closed elsewhere).
	if sr.f != nil {
		_ = sr.f.Close()
	}
}
