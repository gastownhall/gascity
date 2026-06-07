package events

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Filter specifies predicates for ReadFiltered. Zero values are ignored.
type Filter struct {
	Type     string    // match events with this Type
	Actor    string    // match events with this Actor
	Subject  string    // match events with this Subject
	Since    time.Time // match events at or after this time
	Until    time.Time // match events at or before this time
	AfterSeq uint64    // match events with Seq > AfterSeq (0 = no filter)
	Limit    int       // cap results at this count (0 or negative = unlimited)
}

// matchesFilter reports whether e satisfies all non-zero predicates in f.
// It does not enforce Limit — that is applied by the caller.
func matchesFilter(e Event, f Filter) bool {
	if f.AfterSeq > 0 && e.Seq <= f.AfterSeq {
		return false
	}
	if f.Type != "" && e.Type != f.Type {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Subject != "" && e.Subject != f.Subject {
		return false
	}
	if !f.Since.IsZero() && e.Ts.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.Ts.After(f.Until) {
		return false
	}
	return true
}

// ApplyFilter returns events matching all non-zero predicates in filter.
// It preserves input order and applies a positive Limit after matching.
func ApplyFilter(evts []Event, filter Filter) []Event {
	var result []Event
	for _, e := range evts {
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	return result
}

func limitReached(count int, filter Filter) bool {
	return filter.Limit > 0 && count >= filter.Limit
}

// ReadAll reads all events from the JSONL file at path, transparently
// walking sibling archives produced by rotation. Archives are read in
// seq order before the active file, yielding a single chronological
// stream. Returns (nil, nil) if neither the active file nor any
// archives exist.
func ReadAll(path string) ([]Event, error) {
	return ReadFiltered(path, Filter{})
}

// ReadFiltered reads events from path and sibling archives, returning
// only those matching all non-zero fields in filter. Archives whose
// seq window is fully excluded by the filter's AfterSeq predicate are
// skipped without gunzipping. Returns (nil, nil) if no events exist.
// Scanner errors return the events parsed before the error alongside
// the error.
func ReadFiltered(path string, filter Filter) ([]Event, error) {
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		// Listing the dir failed (most often: dir doesn't exist).
		// Fall through to the active-file path; if that also fails,
		// the caller gets a single error.
		archives = nil
	}

	var result []Event
	for _, info := range archives {
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
			return result, fmt.Errorf("reading archive %q: %w", info.Basename, err)
		}
		if limitReached(len(result), filter) {
			return result, nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if len(result) == 0 {
				return nil, nil
			}
			return result, nil
		}
		return result, fmt.Errorf("reading events: %w", err)
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
		return result, fmt.Errorf("scanning events: %w", err)
	}
	return result, nil
}

// archiveFilesIn lists canonical events archives in dir, sorted by
// FirstSeq ascending so callers can read them in chronological order.
// Files that don't match the canonical name pattern (legacy archives,
// unrelated files) are silently skipped — a corrupt archive in the
// dir must not poison the read path.
func archiveFilesIn(dir string) ([]archiveInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var archives []archiveInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := parseArchiveBasename(e.Name())
		if err != nil {
			continue
		}
		archives = append(archives, info)
	}
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].FirstSeq < archives[j].FirstSeq
	})
	return archives, nil
}

// streamArchive gunzip-streams the file at path, decoding each line
// as an Event and invoking fn for every event. fn returns false to
// abort iteration early. Returns nil if iteration completed cleanly
// or fn requested abort; errors from gzip / scanner are wrapped.
func streamArchive(path string, _ Filter, fn func(Event) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only file

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gr.Close() //nolint:errcheck // read-only stream

	scanner := bufio.NewScanner(gr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if !fn(e) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning archive: %w", err)
	}
	return nil
}

// ReadFilteredTail reads the trailing matching events from path. A positive
// limit returns at most that many events in chronological order; limit <= 0
// falls back to ReadFiltered.
func ReadFilteredTail(path string, filter Filter, limit int) ([]Event, error) {
	if limit <= 0 {
		return ReadFiltered(path, filter)
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading events tail: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat events tail: %w", err)
	}
	return readFilteredTailFromFile(f, info.Size(), filter, limit)
}

func readFilteredTailFromFile(f *os.File, size int64, filter Filter, limit int) ([]Event, error) {
	if size <= 0 {
		return nil, nil
	}
	const chunkSize int64 = 64 * 1024
	var reversed []Event
	var pending []byte
	end := size
	for end > 0 && len(reversed) < limit {
		n := chunkSize
		if end < n {
			n = end
		}
		start := end - n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading events tail: %w", err)
		}
		data := make([]byte, 0, len(chunk)+len(pending))
		data = append(data, chunk...)
		data = append(data, pending...)
		parts := bytes.Split(data, []byte{'\n'})
		firstComplete := 0
		if start > 0 {
			pending = append(pending[:0], parts[0]...)
			firstComplete = 1
		} else {
			pending = nil
		}
		for i := len(parts) - 1; i >= firstComplete && len(reversed) < limit; i-- {
			line := bytes.TrimSuffix(parts[i], []byte{'\r'})
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Event
			if err := json.Unmarshal(line, &e); err != nil {
				continue
			}
			if matchesFilter(e, filter) {
				reversed = append(reversed, e)
			}
		}
		end = start
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, nil
}

// ReadLatestSeq returns the highest complete event Seq visible in the
// active events file or any canonical sibling archive. Event logs are
// append-only and sequence numbers are monotonic, so the active file
// is read backward from the tail and archives contribute their
// filename-encoded LastSeq without being gunzipped.
func ReadLatestSeq(path string) (uint64, error) {
	seq, err := readLatestActiveSeq(path)
	if err != nil {
		return 0, err
	}
	archives, err := archiveFilesIn(filepath.Dir(path))
	if err == nil {
		for _, info := range archives {
			if info.LastSeq > seq {
				seq = info.LastSeq
			}
		}
	}
	return seq, nil
}

func readLatestActiveSeq(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading latest seq: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat events: %w", err)
	}
	return readLatestSeqFromTail(f, info.Size())
}

func readLatestSeqFromTail(f *os.File, size int64) (uint64, error) {
	if size <= 0 {
		return 0, nil
	}
	const chunkSize int64 = 64 * 1024
	var suffix []byte
	end := size
	first := true
	for end > 0 {
		n := chunkSize
		if end < n {
			n = end
		}
		start := end - n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return 0, fmt.Errorf("reading latest seq: %w", err)
		}
		data := make([]byte, 0, len(chunk)+len(suffix))
		data = append(data, chunk...)
		data = append(data, suffix...)
		searchEnd := len(data)
		if first && len(data) > 0 && data[len(data)-1] != '\n' {
			idx := bytes.LastIndexByte(data, '\n')
			if idx < 0 {
				suffix = data
				end = start
				first = false
				continue
			}
			searchEnd = idx
		}
		searchStart := 0
		if start > 0 {
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				suffix = data
				end = start
				first = false
				continue
			}
			searchStart = idx + 1
		}
		if seq, ok := latestSeqInCompleteLines(data[searchStart:searchEnd]); ok {
			return seq, nil
		}
		suffix = data
		end = start
		first = false
	}
	return 0, nil
}

func latestSeqInCompleteLines(data []byte) (uint64, bool) {
	for len(data) > 0 {
		idx := bytes.LastIndexByte(data, '\n')
		var line []byte
		if idx >= 0 {
			line = data[idx+1:]
			data = data[:idx]
		} else {
			line = data
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var header struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &header); err == nil && header.Seq > 0 {
			return header.Seq, true
		}
	}
	return 0, false
}

// ReadFrom reads events starting at the given byte offset in the file.
// Returns the events read, the byte offset after the last complete line,
// and any error. Returns (nil, offset, nil) if no new data is available
// or the file doesn't exist yet. Skips malformed lines (partial writes).
func ReadFrom(path string, offset int64) ([]Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seeking events: %w", err)
	}

	var result []Event
	r := bufio.NewReader(f)
	bytesRead := int64(0)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				// Complete line — safe to advance offset past it.
				bytesRead += int64(len(line))
				trimmed := line[:len(line)-1]
				if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\r' {
					trimmed = trimmed[:len(trimmed)-1]
				}
				var e Event
				if jsonErr := json.Unmarshal(trimmed, &e); jsonErr == nil {
					result = append(result, e)
				}
				// skip malformed lines (partial writes)
			}
			// Partial line (no trailing \n): don't advance offset.
			// The next ReadFrom call will re-read it once complete.
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return result, offset + bytesRead, fmt.Errorf("scanning events: %w", err)
		}
	}
	return result, offset + bytesRead, nil
}
