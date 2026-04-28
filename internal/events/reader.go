package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Filter specifies predicates for ReadFiltered. Zero values are ignored.
type Filter struct {
	Type     string    // match events with this Type
	Actor    string    // match events with this Actor
	Since    time.Time // match events at or after this time
	AfterSeq uint64    // match events with Seq > AfterSeq (0 = no filter)
}

// ReadAll reads all events from the JSONL file at path.
// Returns (nil, nil) if the file is missing or empty.
func ReadAll(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	var events []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		events = append(events, e)
	}
	if err := scanner.Err(); err != nil {
		return events, fmt.Errorf("scanning events: %w", err)
	}
	return events, nil
}

// ReadFiltered reads events from path and returns only those matching
// all non-zero fields in filter. Returns (nil, nil) if the file is
// missing or empty.
func ReadFiltered(path string, filter Filter) ([]Event, error) {
	all, err := ReadAll(path)
	if err != nil {
		return nil, err
	}

	var result []Event
	for _, e := range all {
		if filter.AfterSeq > 0 && e.Seq <= filter.AfterSeq {
			continue
		}
		if filter.Type != "" && e.Type != filter.Type {
			continue
		}
		if filter.Actor != "" && e.Actor != filter.Actor {
			continue
		}
		if !filter.Since.IsZero() && e.Ts.Before(filter.Since) {
			continue
		}
		result = append(result, e)
	}
	return result, nil
}

// ReadLatestSeq returns the latest complete event Seq in the events file, or
// 0 if the file is missing or empty. Event logs are append-only and sequence
// numbers are monotonic, so this reads backward from the tail instead of
// parsing historical events on every recorder open.
func ReadLatestSeq(path string) (uint64, error) {
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
