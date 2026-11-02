package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"time"
)

// sinceScanSkew is the margin a backward walk answering a Since filter keeps
// behind filter.Since. Records older than Since minus the margin are past the
// window, and the walk stops only on a contiguous run of them whose own
// timestamps span the margin (see sinceWindow).
//
// Seq is monotonic in append order; Ts is not. The recorder stamps a zero Ts
// under the log's lock, so auto-stamped records are in order, but it keeps a
// caller-supplied Ts, and some producers record a moment they observed
// earlier: endpointCapacityGuard.flush records each breaker transition at the
// end of a reconcile tick with the transition's own time, and
// StoreMaintenanceLoop records a run at its FinishedAt. A backward step of the
// wall clock the records are stamped from does the same to every record after
// it.
//
// The walk relies on this invariant: records whose own timestamps span
// sinceScanSkew never all trail a record appended before them by more than
// sinceScanSkew. A lone backdated record, or a burst stamped at one instant,
// can trail by more, so neither proves anything. Two layouts break the
// invariant, and only the forward scan answers them correctly: a backward
// clock step larger than twice the margin followed by at least the margin's
// worth of appends, and a single flush whose backdated timestamps themselves
// span the margin, which takes a reconcile tick longer than twice the margin.
const sinceScanSkew = 5 * time.Minute

// walkChunkSize is how much of the file a backward walk reads at a time.
const walkChunkSize int64 = 64 * 1024

// sinceWindow decides when a backward walk has left a Since window for good.
// A nil *sinceWindow, for a filter without Since, never closes.
type sinceWindow struct {
	horizon time.Time // Since minus sinceScanSkew
	// newest and oldest bound the Ts of the current run of records older
	// than horizon; both are zero when no run is in progress.
	newest, oldest time.Time
}

// newSinceWindow returns the window for since, or nil when since is zero.
func newSinceWindow(since time.Time) *sinceWindow {
	if since.IsZero() {
		return nil
	}
	return &sinceWindow{horizon: since.Add(-sinceScanSkew)}
}

// closedBy records the next record's Ts, newest first, and reports whether
// the run of records older than the horizon now spans sinceScanSkew. A zero
// Ts neither extends nor breaks the run; a Ts at or after the horizon starts
// it over.
func (w *sinceWindow) closedBy(ts time.Time) bool {
	if w == nil || ts.IsZero() {
		return false
	}
	if !ts.Before(w.horizon) {
		w.newest, w.oldest = time.Time{}, time.Time{}
		return false
	}
	if w.newest.IsZero() || ts.After(w.newest) {
		w.newest = ts
	}
	if w.oldest.IsZero() || ts.Before(w.oldest) {
		w.oldest = ts
	}
	return w.newest.Sub(w.oldest) >= sinceScanSkew
}

// walkBackward visits the events in the first size bytes of r newest first,
// until visit returns true, the walk passes byte 0, or it has read maxBytes
// (0 or negative = unbounded). It reports whether visit stopped the walk, and
// stops with ctx's error when ctx is done, checked before each chunk. Blank
// and malformed lines are skipped, as the forward scan skips them. A line is
// carried across chunk reads whole, so unlike the forward scan's
// bufio.Scanner the walk has no 1 MB line limit.
func walkBackward(ctx context.Context, r io.ReaderAt, size, maxBytes int64, visit func(Event) bool) (bool, error) {
	var pending []byte
	for end := size; end > 0 && (maxBytes <= 0 || size-end < maxBytes); {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n := min(walkChunkSize, end)
		// Clamp the read to what remains of the byte budget so a maxBytes
		// that is not a walkChunkSize multiple stops the walk mid-chunk
		// rather than overscanning by up to one full chunk. The loop
		// condition guarantees the remainder is positive, and the chunk is
		// read at start = end - n, so a smaller n simply moves the walk's
		// stopping point without misaligning pending.
		if maxBytes > 0 {
			n = min(n, maxBytes-(size-end))
		}
		start := end - n
		chunk := make([]byte, n, n+int64(len(pending)))
		if _, err := r.ReadAt(chunk, start); err != nil && err != io.EOF {
			return false, err
		}
		lines := bytes.Split(append(chunk, pending...), []byte{'\n'})
		pending = nil
		if start > 0 {
			// The first line may begin before start; finish it next read.
			pending, lines = lines[0], lines[1:]
		}
		if visitLinesBackward(lines, visit) {
			return true, nil
		}
		end = start
	}
	return false, nil
}

// visitLinesBackward visits the lines that parse as events, last line first,
// and reports whether visit asked to stop.
func visitLinesBackward(lines [][]byte, visit func(Event) bool) bool {
	for i := len(lines) - 1; i >= 0; i-- {
		var e Event
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		if visit(e) {
			return true
		}
	}
	return false
}

// headTs returns the Ts of the first line in r, or the zero time when that
// line has no Ts, does not parse, or is longer than seqProbeBufSize.
func headTs(r io.ReaderAt, size int64) (time.Time, error) {
	buf := make([]byte, min(size, seqProbeBufSize))
	n, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return time.Time{}, err
	}
	line, _, found := bytes.Cut(buf[:n], []byte{'\n'})
	if !found && int64(n) < size {
		return time.Time{}, nil
	}
	var head struct {
		Ts time.Time `json:"ts"`
	}
	if json.Unmarshal(line, &head) != nil {
		return time.Time{}, nil
	}
	return head.Ts, nil
}

// sinceWalk walks the first size bytes of r, an active log, backward for a
// Since filter. It passes each match to keep, newest first, and stops when
// keep returns true or the window closes (see sinceWindow), and it reports
// whether it stopped for either reason. Only a closed window proves that the
// records appended before that point, the older segments' and archives'
// included, hold no match. It reports false without walking when the filter
// has no Since or the log's first record is too recent for the window to
// close, and false after a walk that reaches byte 0 still open; the caller
// must then run the full read.
func sinceWalk(ctx context.Context, r io.ReaderAt, size int64, filter Filter, keep func(Event) bool) (bool, error) {
	window := newSinceWindow(filter.Since)
	if window == nil {
		return false, nil
	}
	// Closing takes a run of records older than the horizon that spans
	// sinceScanSkew, so the run's oldest record predates
	// horizon-sinceScanSkew. When the log's first record is younger than
	// that, only records backdated behind it could close the window, so
	// skip the walk: the caller's full read answers correctly and would
	// otherwise repeat every line the walk parsed.
	head, err := headTs(r, size)
	if err != nil {
		return false, err
	}
	if !head.IsZero() && !head.Before(window.horizon.Add(-sinceScanSkew)) {
		return false, nil
	}
	return walkBackward(ctx, r, size, 0, func(e Event) bool {
		if matchesFilter(e, filter) && keep(e) {
			return true
		}
		return window.closedBy(e.Ts)
	})
}

// readFilteredSince answers a Since filter from the active log alone by
// walking it backward from EOF (see sinceWalk). The second return reports
// whether the answer is complete, which holds only when the walk closed the
// window. When the filter has no Since, the log is missing or empty, or the
// walk cannot close or does not, it returns (nil, false, nil) and the caller
// must run the full read. A read error, or ctx's error, returns the matches
// parsed before it with the error, never reported complete.
//
// It applies filter.Limit itself, to the ascending result, matching
// readFilteredTracked, which takes the FIRST Limit matches in log order
// rather than the last. Those are the last matches the walk reaches, so it
// holds every match in the window until the walk stops, where the forward
// scan holds at most Limit.
func readFilteredSince(ctx context.Context, path string, filter Filter) ([]Event, bool, error) {
	if filter.Since.IsZero() {
		return nil, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No active log is not the same as no history: archives may
			// exist, so leave the answer to the full read.
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("stat events: %w", err)
	}
	if info.Size() <= 0 {
		return nil, false, nil
	}
	return readFilteredSinceFrom(ctx, f, info.Size(), filter)
}

// readFilteredSinceFrom is readFilteredSince over the first size bytes of r.
func readFilteredSinceFrom(ctx context.Context, r io.ReaderAt, size int64, filter Filter) ([]Event, bool, error) {
	var reversed []Event
	closed, err := sinceWalk(ctx, r, size, filter, func(e Event) bool {
		reversed = append(reversed, e)
		return false
	})
	if err != nil {
		if ctx.Err() == nil {
			err = fmt.Errorf("reading events: %w", err)
		}
		return ascending(reversed, filter.Limit), false, err
	}
	if !closed {
		return nil, false, nil
	}
	return ascending(reversed, filter.Limit), true, nil
}

// ascending puts events collected newest first back in log order and keeps
// the first limit of them (0 or negative = all).
func ascending(reversed []Event, limit int) []Event {
	slices.Reverse(reversed)
	if limit > 0 && len(reversed) > limit {
		return reversed[:limit]
	}
	return reversed
}
