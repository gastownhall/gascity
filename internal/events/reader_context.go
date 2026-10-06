package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReadFilteredContext is ReadFiltered but aborts the archive scan as soon as
// ctx is canceled, instead of always scanning every overlapping archive to
// completion. Cancellation is checked once per archive in the loop, so a
// caller that disconnects mid-scan gets back the events found so far plus
// ctx.Err(), rather than waiting for the full (potentially large) fallback
// scan to finish.
func ReadFilteredContext(ctx context.Context, path string, filter Filter) ([]Event, error) {
	result, _, err := readFilteredTrackedContext(ctx, path, filter)
	return result, err
}

// readFilteredTrackedContext is readFilteredTracked plus ctx cancellation
// checked as the first step of each archive-loop iteration. See
// readFilteredTracked for the archive-window tracking this returns.
func readFilteredTrackedContext(ctx context.Context, path string, filter Filter) ([]Event, map[eventSeqWindow]struct{}, error) {
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		// Listing the dir failed (most often: dir doesn't exist).
		// Fall through to the active-file path; if that also fails,
		// the caller gets a single error.
		archives = nil
	}

	var result []Event
	listed := make(map[eventSeqWindow]struct{}, len(archives))
	for _, info := range archives {
		listed[eventSeqWindow{first: info.FirstSeq, last: info.LastSeq}] = struct{}{}
	}
	for _, info := range archives {
		if err := ctx.Err(); err != nil {
			return result, listed, err
		}
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		archivePath := filepath.Join(dir, info.Basename)
		err := streamArchive(archivePath, filter, func(e Event) bool {
			if !matchesFilter(e, filter) {
				return true
			}
			result = append(result, e)
			return !limitReached(len(result), filter)
		})
		if err != nil {
			return result, listed, fmt.Errorf("reading archive %q: %w", info.Basename, err)
		}
		if limitReached(len(result), filter) {
			return result, listed, nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if len(result) == 0 {
				return nil, listed, nil
			}
			return result, listed, nil
		}
		return result, listed, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return result, listed, fmt.Errorf("scanning events: %w", err)
	}
	return result, listed, nil
}

// ReadFilteredWithInFlightContext is ReadFilteredWithInFlight but propagates
// ctx cancellation through the underlying archive scan, the same way
// ReadFilteredContext does for ReadFiltered.
func ReadFilteredWithInFlightContext(ctx context.Context, path string, filter Filter) ([]Event, error) {
	base, listedArchives, baseErr := readFilteredTrackedContext(ctx, path, filter)
	rotated, rotationErr := readRotationSources(path, filter, listedArchives)
	if len(rotated) == 0 {
		if baseErr == nil {
			return base, rotationErr
		}
		return base, baseErr
	}
	merged := mergeEventsBySeq(base, rotated)
	if baseErr != nil {
		return merged, baseErr
	}
	return merged, rotationErr
}
