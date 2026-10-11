package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ReadFilteredEach calls fn, in order, for each event ReadFiltered would
// return (the sibling archives oldest-first, then the active log), without
// collecting them, so memory does not grow with the number of matches. It
// stops at fn's first error and returns it. It returns ctx's error when ctx is
// done, checked before each archive and every historyTailCtxCheckLines events
// or lines. A positive Filter.Limit stops it after that many events, as in
// ReadFiltered.
func ReadFilteredEach(ctx context.Context, path string, filter Filter, fn func(Event) error) error {
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		archives = nil // as in ReadFiltered: fall through to the active log
	}
	matched := 0
	var fnErr error
	visit := func(e Event) bool {
		if !matchesFilter(e, filter) {
			return true
		}
		if fnErr = fn(e); fnErr != nil {
			return false
		}
		matched++
		return !limitReached(matched, filter)
	}

	for _, info := range archives {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		var ctxErr error
		calls := 0
		err := streamArchive(filepath.Join(dir, info.Basename), filter, func(e Event) bool {
			if calls++; calls%historyTailCtxCheckLines == 0 {
				if ctxErr = ctx.Err(); ctxErr != nil {
					return false
				}
			}
			return visit(e)
		})
		switch {
		case err != nil:
			return fmt.Errorf("reading archive %q: %w", info.Basename, err)
		case ctxErr != nil:
			return ctxErr
		case fnErr != nil:
			return fnErr
		case limitReached(matched, filter):
			return nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // as ReadFiltered
	for lines := 1; scanner.Scan(); lines++ {
		if lines%historyTailCtxCheckLines == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue // skip malformed lines
		}
		if !visit(e) {
			return fnErr
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning events: %w", err)
	}
	return nil
}
