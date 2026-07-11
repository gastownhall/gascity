package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// backfillConcurrency bounds how many watchers may walk archives concurrently.
// A cold resume gunzips archives; without a cap, N simultaneous resumes multiply
// the CPU/IO cost. Excess resumes queue on this semaphore rather than piling on.
const backfillConcurrency = 2

var backfillSlots = make(chan struct{}, backfillConcurrency)

func acquireBackfillSlot(ctx context.Context) error {
	select {
	case backfillSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func releaseBackfillSlot() { <-backfillSlots }

// backfillSourceKind distinguishes a gzip archive from a plain-JSONL segment
// (an in-flight rotating-* file or the captured active file) so the streamer
// picks the right decoder.
type backfillSourceKind int

const (
	sourceArchive backfillSourceKind = iota
	sourceRotating
)

// backfillSource is one on-disk segment contributing to a resume backfill,
// ordered by its first sequence. Archives and rotating files carry a
// filename-encoded seq window used both to order them and to skip segments
// wholly at or below the resume cursor.
type backfillSource struct {
	path     string
	kind     backfillSourceKind
	firstSeq uint64
	lastSeq  uint64
}

// listBackfillSources returns the .gz archives and in-flight rotating-* files in
// dir whose seq window may contain events with Seq > afterSeq, sorted by first
// sequence. A .gz archive and its not-yet-removed rotating source share a seq
// window (equal FirstSeq); the streamed monotonic guard drops the duplicate, so
// both are safe to include. The active file is appended by the caller (it reads
// from a captured fd, not by path, to stay correct across a mid-backfill
// rotation).
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
			_, first, last, ok := parseRotatingBasename(name)
			if !ok {
				continue // legacy rotating file without a window; reaper promotes it
			}
			if afterSeq > 0 && last <= afterSeq {
				continue
			}
			srcs = append(srcs, backfillSource{
				path: filepath.Join(dir, name), kind: sourceRotating,
				firstSeq: first, lastSeq: last,
			})
		}
	}
	sort.Slice(srcs, func(i, j int) bool {
		if srcs[i].firstSeq != srcs[j].firstSeq {
			return srcs[i].firstSeq < srcs[j].firstSeq
		}
		// A rotating file and its promoted .gz share FirstSeq; order is
		// irrelevant to correctness (the monotonic guard dedupes), but keep it
		// deterministic.
		return srcs[i].kind < srcs[j].kind
	})
	return srcs, nil
}

// isCanonicalArchiveBasename reports whether name is a canonical .gz archive
// (not a legacy pre-seq-window one, which parseArchiveBasename rejects anyway).
func isCanonicalArchiveBasename(name string) bool {
	return strings.HasPrefix(name, "events.jsonl.archive-") && strings.HasSuffix(name, ".gz")
}

// streamPlainJSONL streams filter-matching events from a plain-JSONL events file
// (a rotating-* segment or the active file), invoking fn per event until fn
// returns false or the file ends. It is the plain-text sibling of streamArchive.
// A missing file yields no events and no error (a rotating file may be promoted
// and removed between listing and read; its .gz counterpart covers the window).
func streamPlainJSONL(path string, filter Filter, fn func(Event) bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close() //nolint:errcheck // read-only file
	return scanJSONL(f, filter, fn)
}

// scanJSONL streams filter-matching events from r (a plain-JSONL reader),
// stopping when fn returns false. Malformed lines are skipped.
func scanJSONL(r interface{ Read([]byte) (int, error) }, filter Filter, fn func(Event) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if !matchesFilter(e, filter) {
			continue
		}
		if !fn(e) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning events: %w", err)
	}
	return nil
}

// streamBackfillSource streams one source's filter-matching events with Seq
// strictly greater than *lastDelivered into out, advancing *lastDelivered. It
// stops early (returning ctx/done error) on cancellation.
func streamBackfillSource(ctx context.Context, done <-chan struct{}, src backfillSource, filter Filter, lastDelivered *uint64, out *[]Event) error {
	guard := func(e Event) bool {
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return false
		default:
		}
		if e.Seq <= *lastDelivered {
			return true // dedupe / below cursor; keep scanning
		}
		*lastDelivered = e.Seq
		*out = append(*out, e)
		return true
	}
	// Apply the AfterSeq floor so the archive/rotating scan can skip cheaply.
	f := filter
	if f.AfterSeq < *lastDelivered {
		f.AfterSeq = *lastDelivered
	}
	var err error
	switch src.kind {
	case sourceArchive:
		err = streamArchive(src.path, f, guard)
	default:
		err = streamPlainJSONL(src.path, f, guard)
	}
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return errWatcherClosed
	default:
		return nil
	}
}
