package events

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// readNewestBounded scans archives and any in-flight rotating segment for
// path's log, newest-first, collecting up to fetch filter-matching events
// while charging each archive's on-disk (compressed) size against
// maxArchiveBytes. It does not read the active file: callers try
// TailProvider.ListTail first and only fall back here when that came up
// short, reusing the tail result directly rather than rediscovering it —
// see fetchEventPageAscending in internal/api.
//
// The budget applies only to the second and later archives opened in a
// single call: the first archive considered always proceeds regardless of
// its size, so one archive larger than the configured budget cannot wedge a
// caller in a zero-progress truncated loop — every call must make forward
// progress or a client could never reach older history.
//
// Returns events in ascending Seq order, matching every other Provider
// method. truncated is true only when the budget was exhausted before fetch
// matches were found and before every overlapping archive was scanned:
// finding exactly fetch matches within budget is ordinary pagination
// (truncated=false), not truncation. When truncated, reachedSeq is the
// resume boundary — a follow-up call with Filter.BeforeSeq set to reachedSeq
// continues immediately below the last archive this call did not open, with
// no gap and no overlap, matching matchesFilter's strict BeforeSeq
// semantics.
func readNewestBounded(ctx context.Context, path string, filter Filter, fetch int, maxArchiveBytes int64) ([]Event, bool, uint64, error) {
	dir := filepath.Dir(path)

	archives, aerr := archiveFilesIn(dir)
	if aerr != nil {
		// Listing the dir failed (most often: dir doesn't exist yet).
		archives = nil
	}
	listedArchives := make(map[eventSeqWindow]struct{}, len(archives))
	for _, info := range archives {
		listedArchives[eventSeqWindow{first: info.FirstSeq, last: info.LastSeq}] = struct{}{}
	}

	// Sources invisible to the archives snapshot above: genuinely in-flight
	// rotating files, plus any archive whose promotion landed between the
	// two listings. Both are strictly newer than every archive already in
	// listedArchives — rotation only ever advances forward — so this whole
	// tier is scanned newest-first before the base snapshot below. Mirrors
	// readRotationSources's two-listing order, which closes the same
	// promotion-race gap for ReadFilteredWithInFlight.
	supplemental, serr := listBackfillSources(dir, 0)
	if serr != nil {
		return nil, false, 0, serr
	}

	scan := &boundedScan{dir: dir, filter: filter, fetch: fetch, maxArchiveBytes: maxArchiveBytes}
	if stop := scan.scanSupplemental(ctx, supplemental, listedArchives); stop != nil {
		return scan.result(stop)
	}
	if stop := scan.scanArchives(ctx, archives); stop != nil {
		return scan.result(stop)
	}
	return reverseEvents(scan.newestFirst), false, 0, nil
}

// boundedScan is the running state of one readNewestBounded call: the
// matches collected so far (newest-first) and the archive budget spent.
type boundedScan struct {
	dir             string
	filter          Filter
	fetch           int
	maxArchiveBytes int64

	newestFirst   []Event
	budgetUsed    int64
	openedArchive bool
}

// scanStop ends a boundedScan early. truncated and resume report a budget
// stop; err reports a failure, with partial saying whether the matches
// collected so far are still returned alongside it (a canceled context
// returns none).
type scanStop struct {
	truncated bool
	resume    uint64
	err       error
	partial   bool
}

// result converts an early stop into readNewestBounded's return values.
func (s *boundedScan) result(stop *scanStop) ([]Event, bool, uint64, error) {
	if stop.err != nil && !stop.partial {
		return nil, false, 0, stop.err
	}
	return reverseEvents(s.newestFirst), stop.truncated, stop.resume, stop.err
}

// full reports whether the scan has collected fetch matches.
func (s *boundedScan) full() bool {
	return len(s.newestFirst) >= s.fetch
}

// appendNewestTail appends the newest need events of matches (ascending) to
// newestFirst, newest first.
func (s *boundedScan) appendNewestTail(matches []Event, need int) {
	if len(matches) > need {
		matches = matches[len(matches)-need:]
	}
	for i := len(matches) - 1; i >= 0; i-- {
		// The walk is strictly descending, so a seq at or above the
		// last one appended is a duplicate from an overlapping source.
		if n := len(s.newestFirst); n > 0 && matches[i].Seq >= s.newestFirst[n-1].Seq {
			continue
		}
		s.newestFirst = append(s.newestFirst, matches[i])
	}
}

// scanSupplemental reads the supplemental sources newest-first. seen holds
// the seq windows this pass has already read: an archive and its
// not-yet-removed rotating twin share a window and hold the same seqs, so
// only the first one reached is read. It returns nil when the tier is done.
func (s *boundedScan) scanSupplemental(ctx context.Context, supplemental []backfillSource, listedArchives map[eventSeqWindow]struct{}) *scanStop {
	seen := make(map[eventSeqWindow]struct{}, len(supplemental))
	for i := len(supplemental) - 1; i >= 0 && !s.full(); i-- {
		src := supplemental[i]
		window := eventSeqWindow{first: src.firstSeq, last: src.lastSeq}
		// Any source whose window the base archives snapshot covers is
		// redundant: for an archive it IS that archive, and for a rotating
		// file it is the archive's twin. Mirrors readRotationSources.
		if _, ok := listedArchives[window]; ok {
			continue
		}
		if _, ok := seen[window]; ok {
			continue
		}
		// Same cursor guard as the base loop: a source wholly outside the
		// filter's window must neither spend the first-archive allowance
		// nor mint a resume boundary at or above BeforeSeq.
		if !archiveOverlapsFilter(archiveInfo{FirstSeq: src.firstSeq, LastSeq: src.lastSeq}, s.filter) {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return &scanStop{err: cerr}
		}
		if src.kind == sourceArchive && chargeArchiveBudget(src.path, &s.budgetUsed, &s.openedArchive, s.maxArchiveBytes) {
			return &scanStop{truncated: true, resume: clampResume(src.lastSeq+1, s.filter)}
		}
		matches, rerr := readSegmentSourceMatches(src, s.filter)
		if rerr != nil {
			return &scanStop{err: fmt.Errorf("reading %q: %w", filepath.Base(src.path), rerr), partial: true}
		}
		seen[window] = struct{}{}
		s.appendNewestTail(matches, s.fetch-len(s.newestFirst))
	}
	return nil
}

// scanArchives reads the base archives snapshot newest-first. It returns
// nil when the snapshot is done.
func (s *boundedScan) scanArchives(ctx context.Context, archives []archiveInfo) *scanStop {
	for i := len(archives) - 1; i >= 0 && !s.full(); i-- {
		info := archives[i]
		if !archiveOverlapsFilter(info, s.filter) {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return &scanStop{err: cerr}
		}
		archivePath := filepath.Join(s.dir, info.Basename)
		if chargeArchiveBudget(archivePath, &s.budgetUsed, &s.openedArchive, s.maxArchiveBytes) {
			return &scanStop{truncated: true, resume: clampResume(info.LastSeq+1, s.filter)}
		}

		var archMatches []Event
		if serr := streamArchive(archivePath, s.filter, func(e Event) bool {
			if matchesFilter(e, s.filter) {
				archMatches = append(archMatches, e)
			}
			return true // never early-abort: a partially-needed archive must keep its NEWEST matches, found only by reading it in full
		}); serr != nil {
			return &scanStop{err: fmt.Errorf("reading archive %q: %w", info.Basename, serr), partial: true}
		}
		s.appendNewestTail(archMatches, s.fetch-len(s.newestFirst))
	}
	return nil
}

// clampResume caps a truncation resume boundary at the incoming cursor so a
// follow-up call never re-serves rows at or above filter.BeforeSeq or moves
// pagination backwards.
func clampResume(seq uint64, filter Filter) uint64 {
	if filter.BeforeSeq > 0 && seq > filter.BeforeSeq {
		return filter.BeforeSeq
	}
	return seq
}

// chargeArchiveBudget charges path's on-disk size against the running
// budgetUsed total and reports whether opening it would exceed
// maxArchiveBytes. The very first archive charged (openedArchive still
// false) is always admitted regardless of size — see readNewestBounded's
// doc comment on forward progress. A stat failure (e.g. retention reaped
// the archive between listing and here) is treated as free: the caller's
// subsequent read attempt fails or succeeds on its own terms rather than
// mis-truncating on a phantom charge.
func chargeArchiveBudget(path string, budgetUsed *int64, openedArchive *bool, maxArchiveBytes int64) (truncate bool) {
	st, statErr := os.Stat(path)
	if statErr != nil {
		*openedArchive = true
		return false
	}
	if *openedArchive && *budgetUsed+st.Size() > maxArchiveBytes {
		return true
	}
	*budgetUsed += st.Size()
	*openedArchive = true
	return false
}

// readSegmentSourceMatches fully collects src's filter-matching events in
// ascending order. It cannot stop early: a source can hold more matches
// than the caller currently needs, and only reading it in full reveals
// which ones are newest.
func readSegmentSourceMatches(src backfillSource, filter Filter) ([]Event, error) {
	sr, err := openSegmentReader(src)
	if err != nil {
		return nil, err
	}
	if sr == nil {
		return nil, nil // vanished between listing and open with no fallback left to try
	}
	defer sr.close()
	var out []Event
	var maxSeq uint64
	for {
		done, rerr := sr.readInto(filter, &maxSeq, &out, backfillBatch)
		if rerr != nil {
			return out, rerr
		}
		if done {
			return out, nil
		}
	}
}
