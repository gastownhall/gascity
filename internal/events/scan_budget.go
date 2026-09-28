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

	var newestFirst []Event
	var budgetUsed int64
	var openedArchive bool

	appendNewestTail := func(matches []Event, need int) {
		if len(matches) > need {
			matches = matches[len(matches)-need:]
		}
		for i := len(matches) - 1; i >= 0; i-- {
			// The walk is strictly descending, so a seq at or above the
			// last one appended is a duplicate from an overlapping source.
			if n := len(newestFirst); n > 0 && matches[i].Seq >= newestFirst[n-1].Seq {
				continue
			}
			newestFirst = append(newestFirst, matches[i])
		}
	}

	// seen holds the seq windows this pass has already read: an archive and
	// its not-yet-removed rotating twin share a window and hold the same
	// seqs, so only the first one reached is read.
	seen := make(map[eventSeqWindow]struct{}, len(supplemental))
	for i := len(supplemental) - 1; i >= 0 && len(newestFirst) < fetch; i-- {
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
		if !archiveOverlapsFilter(archiveInfo{FirstSeq: src.firstSeq, LastSeq: src.lastSeq}, filter) {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, false, 0, cerr
		}
		if src.kind == sourceArchive && chargeArchiveBudget(src.path, &budgetUsed, &openedArchive, maxArchiveBytes) {
			return reverseEvents(newestFirst), true, clampResume(src.lastSeq+1, filter), nil
		}
		matches, rerr := readSegmentSourceMatches(src, filter)
		if rerr != nil {
			return reverseEvents(newestFirst), false, 0, fmt.Errorf("reading %q: %w", filepath.Base(src.path), rerr)
		}
		seen[window] = struct{}{}
		appendNewestTail(matches, fetch-len(newestFirst))
	}

	for i := len(archives) - 1; i >= 0 && len(newestFirst) < fetch; i-- {
		info := archives[i]
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, false, 0, cerr
		}
		archivePath := filepath.Join(dir, info.Basename)
		if chargeArchiveBudget(archivePath, &budgetUsed, &openedArchive, maxArchiveBytes) {
			return reverseEvents(newestFirst), true, clampResume(info.LastSeq+1, filter), nil
		}

		var archMatches []Event
		if serr := streamArchive(archivePath, filter, func(e Event) bool {
			if matchesFilter(e, filter) {
				archMatches = append(archMatches, e)
			}
			return true // never early-abort: a partially-needed archive must keep its NEWEST matches, found only by reading it in full
		}); serr != nil {
			return reverseEvents(newestFirst), false, 0, fmt.Errorf("reading archive %q: %w", info.Basename, serr)
		}
		appendNewestTail(archMatches, fetch-len(newestFirst))
	}

	return reverseEvents(newestFirst), false, 0, nil
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

// reverseEvents reverses evts in place and returns it.
func reverseEvents(evts []Event) []Event {
	for i, j := 0, len(evts)-1; i < j; i, j = i+1, j-1 {
		evts[i], evts[j] = evts[j], evts[i]
	}
	return evts
}
