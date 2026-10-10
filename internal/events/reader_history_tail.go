package events

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// historyTailChunkBytes is the read unit of the backward active-log scan and
// the buffer size of the forward line readers.
const historyTailChunkBytes = 64 * 1024

// historyTailCtxCheckLines is how many lines a forward read takes between ctx
// checks, so an abandoned request stops inside a large archive too.
const historyTailCtxCheckLines = 4096

// HistoryTailProvider is an optional extension for providers that can return
// the newest events matching a filter across the WHOLE retained history
// (active log, in-flight rotation segments and archives) while holding at most
// limit events, and that stop reading when ctx is done.
//
// With count false, matched is len(events); with count true, the whole history
// is read (still holding at most limit events) and matched is the number of
// all matching events, as a full scan would count them.
type HistoryTailProvider interface {
	ListHistoryTail(ctx context.Context, filter Filter, limit int, count bool) (events []Event, matched int, err error)
}

// ListHistoryTail implements [HistoryTailProvider] for the file-backed log.
func (r *FileRecorder) ListHistoryTail(ctx context.Context, filter Filter, limit int, count bool) ([]Event, int, error) {
	return ReadFilteredHistoryTail(ctx, r.path, filter, limit, count)
}

// ReadFilteredHistoryTail returns the newest limit events matching filter, in
// ascending seq order, from the active log at path, its in-flight
// events.jsonl.rotating-* segments and its .gz archives, and matched (see
// HistoryTailProvider). It holds at most limit events however long the history
// is. It stops with ctx's error when ctx is done: ctx is checked before each
// 64 KiB chunk of the backward scan, before each segment, and every
// historyTailCtxCheckLines lines and every chunk of a long line in a forward
// read.
//
// The result is the log as of the moment the active log is opened. That file
// is read through the one descriptor, up to its size at the open, and older
// events come only from segments whose seq window ends below the first seq in
// it. A rotation during the read renames that file and creates segments that
// start at or above that seq, so it can neither drop an event that existed at
// the open nor add a later one. When the opened file holds no event, every
// listed segment is read.
//
// Without count, the active log is scanned backward and the segments
// newest-first, and the read stops once limit events are held. With count,
// the segments in seq order and then the active log are read through a ring
// buffer of limit events. Segments are merged by exact seq, so an archive and
// its not-yet-removed rotating twin add nothing. limit must be positive.
func ReadFilteredHistoryTail(ctx context.Context, path string, filter Filter, limit int, count bool) ([]Event, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("history tail: limit %d is not positive", limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	active, err := openActiveLog(path)
	if err != nil {
		return nil, 0, err
	}
	defer active.close()
	if count {
		return countHistory(ctx, path, active, filter, limit)
	}
	return newestHistory(ctx, path, active, filter, limit)
}

// newestHistory reads the active log backward, then the older segments
// newest-first, and stops once limit events are held.
func newestHistory(ctx context.Context, path string, active activeLog, filter Filter, limit int) ([]Event, int, error) {
	out, reachedStart, err := active.scanBackward(ctx, filter, limit)
	if err != nil || !reachedStart || len(out) >= limit {
		return out, len(out), err
	}
	sources, err := olderSegments(ctx, path, active, filter.AfterSeq)
	if err != nil {
		return out, len(out), err
	}
	read := make(map[eventSeqWindow]struct{}, len(sources))
	// listBackfillSources sorts by first seq ascending: walk it backward.
	for i := len(sources) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return out, len(out), err
		}
		src := sources[i]
		window := eventSeqWindow{first: src.firstSeq, last: src.lastSeq}
		if _, ok := read[window]; ok {
			continue // the twin of a merged segment
		}
		if len(out) >= limit && src.lastSeq < out[0].Seq {
			continue // every event here is older than the full page
		}
		if !segmentOverlaps(src, filter) {
			continue
		}
		ring := newEventRing(limit)
		if err := readSegmentInto(ctx, src, filter, ring); err != nil {
			return out, len(out), err
		}
		read[window] = struct{}{}
		out = mergeEventsBySeq(ring.events(), out)
		if len(out) > limit {
			out = out[len(out)-limit:]
		}
	}
	return out, len(out), nil
}

// countHistory reads every older segment in seq order and then the active log
// through one ring buffer, counting all matches. The ring drops a seq that is
// not above the last one only while it reads the segments: the active log
// starts above their windows, so its lines are taken in file order, as the
// full scan takes them.
func countHistory(ctx context.Context, path string, active activeLog, filter Filter, limit int) ([]Event, int, error) {
	sources, err := olderSegments(ctx, path, active, filter.AfterSeq)
	if err != nil {
		return nil, 0, err
	}
	ring := newEventRing(limit)
	ring.ascending = true
	read := make(map[eventSeqWindow]struct{}, len(sources))
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		window := eventSeqWindow{first: src.firstSeq, last: src.lastSeq}
		if _, ok := read[window]; ok || !segmentOverlaps(src, filter) {
			continue
		}
		if err := readSegmentInto(ctx, src, filter, ring); err != nil {
			return nil, 0, err
		}
		read[window] = struct{}{}
	}
	if active.f != nil {
		ring.ascending = false
		if err := readLinesInto(ctx, active.reader(), filter, ring); err != nil {
			return nil, 0, err
		}
	}
	return ring.events(), ring.seen, nil
}

// activeLog is the active log as opened by one read.
type activeLog struct {
	f    *os.File // nil when there is no active log
	size int64    // its size at the open; the read never goes past it
}

func openActiveLog(path string) (activeLog, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return activeLog{}, nil
		}
		return activeLog{}, fmt.Errorf("reading events: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return activeLog{}, fmt.Errorf("stat events: %w", err)
	}
	return activeLog{f: f, size: info.Size()}, nil
}

func (a activeLog) close() {
	if a.f != nil {
		_ = a.f.Close()
	}
}

// reader reads the opened file forward, up to its size at the open.
func (a activeLog) reader() *bufio.Reader {
	return bufio.NewReaderSize(io.NewSectionReader(a.f, 0, a.size), historyTailChunkBytes)
}

// firstSeq returns the seq of the first event line in the opened file.
func (a activeLog) firstSeq(ctx context.Context) (uint64, bool, error) {
	if a.f == nil {
		return 0, false, nil
	}
	br := a.reader()
	for {
		line, err := readLineContext(ctx, br)
		if len(line) > 0 {
			var head struct {
				Seq uint64 `json:"seq"`
			}
			if json.Unmarshal(trimLine(line), &head) == nil && head.Seq > 0 {
				return head.Seq, true, nil
			}
		}
		if errors.Is(err, io.EOF) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
	}
}

// olderSegments lists, in first-seq order, the rotation segments that hold the
// events older than the opened active log: those whose window ends below the
// first seq in it. It lists after the open, so a rotation before the open is
// in the listing and one after it is filtered out.
func olderSegments(ctx context.Context, path string, active activeLog, afterSeq uint64) ([]backfillSource, error) {
	first, known, err := active.firstSeq(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := listBackfillSources(filepath.Dir(path), afterSeq)
	if err != nil || !known {
		return sources, err
	}
	older := sources[:0]
	for _, src := range sources {
		if src.lastSeq < first {
			older = append(older, src)
		}
	}
	return older, nil
}

// scanBackward collects up to limit of the newest events matching filter from
// the opened file, reading historyTailChunkBytes at a time from its size at the
// open toward its start and checking ctx before every chunk. reachedStart
// reports that it read back to the first byte. Malformed and blank lines are
// skipped, as in ReadFilteredTail.
func (a activeLog) scanBackward(ctx context.Context, filter Filter, limit int) ([]Event, bool, error) {
	if a.f == nil {
		return nil, true, nil
	}
	var newestFirst []Event
	emit := func(line []byte) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			return
		}
		var e Event
		if json.Unmarshal(line, &e) == nil && matchesFilter(e, filter) {
			newestFirst = append(newestFirst, e)
		}
	}
	// parts are the pieces of the line that ends at the current position,
	// nearest the line's end first. Each chunk is a fresh buffer (as in
	// ReadFilteredTail), so the pieces can refer to it without a copy.
	var parts [][]byte
	partsLen := 0
	joinLine := func(head []byte) []byte {
		if len(parts) == 0 {
			return head
		}
		line := make([]byte, 0, len(head)+partsLen)
		line = append(line, head...)
		for j := len(parts) - 1; j >= 0; j-- {
			line = append(line, parts[j]...)
		}
		return line
	}
	end := a.size
	for end > 0 && len(newestFirst) < limit {
		if err := ctx.Err(); err != nil {
			return reverseEvents(newestFirst), false, err
		}
		n := min(int64(historyTailChunkBytes), end)
		start := end - n
		chunk := make([]byte, n)
		if _, err := a.f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return nil, false, fmt.Errorf("reading events tail: %w", err)
		}
		for len(newestFirst) < limit {
			i := bytes.LastIndexByte(chunk, '\n')
			if i < 0 {
				if len(chunk) > 0 {
					parts = append(parts, chunk)
					partsLen += len(chunk)
				}
				break
			}
			emit(joinLine(chunk[i+1:]))
			parts, partsLen = nil, 0
			chunk = chunk[:i]
		}
		end = start
	}
	if end == 0 && len(newestFirst) < limit && partsLen > 0 {
		emit(joinLine(nil)) // the first line of the file
	}
	return reverseEvents(newestFirst), end == 0, nil
}

func reverseEvents(evts []Event) []Event {
	for i, j := 0, len(evts)-1; i < j; i, j = i+1, j-1 {
		evts[i], evts[j] = evts[j], evts[i]
	}
	return evts
}

// eventRing keeps the last max events pushed to it, in push order, and counts
// the pushes it keeps. With ascending set it drops an event whose seq is not
// above the last one it took, so segments whose windows overlap add no
// duplicate when they are read in seq order.
type eventRing struct {
	buf       []Event
	oldest    int // index of the oldest kept event once buf is full
	max       int
	seen      int
	ascending bool
	last      uint64
}

func newEventRing(n int) *eventRing { return &eventRing{max: n} }

func (r *eventRing) push(e Event) {
	if r.ascending {
		if r.seen > 0 && e.Seq <= r.last {
			return
		}
		r.last = e.Seq
	}
	r.seen++
	if len(r.buf) < r.max {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.oldest] = e
	r.oldest = (r.oldest + 1) % r.max
}

func (r *eventRing) events() []Event {
	out := make([]Event, 0, len(r.buf))
	out = append(out, r.buf[r.oldest:]...)
	return append(out, r.buf[:r.oldest]...)
}

// readSegmentInto streams one segment forward into ring.
func readSegmentInto(ctx context.Context, src backfillSource, filter Filter, ring *eventRing) error {
	sr, err := openSegmentReader(src)
	if err != nil {
		return fmt.Errorf("reading event segment %q: %w", filepath.Base(src.path), err)
	}
	if sr == nil {
		return nil // reaped by retention between listing and open
	}
	defer sr.close()
	if err := readLinesInto(ctx, sr.br, filter, ring); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("reading event segment %q: %w", filepath.Base(src.path), err)
	}
	return nil
}

// readLinesInto pushes every event line of br that matches filter into ring.
// Malformed lines are skipped.
func readLinesInto(ctx context.Context, br *bufio.Reader, filter Filter, ring *eventRing) error {
	for lines := 1; ; lines++ {
		if lines%historyTailCtxCheckLines == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		line, err := readLineContext(ctx, br)
		if len(line) > 0 {
			var e Event
			if json.Unmarshal(trimLine(line), &e) == nil && matchesFilter(e, filter) {
				ring.push(e)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// readLineContext returns the next line of br, including its newline, one
// buffered chunk at a time, checking ctx after every chunk that does not end
// the line. A line that fits in one chunk is returned without a copy and is
// valid only until the next read. err is io.EOF after the last line.
func readLineContext(ctx context.Context, br *bufio.Reader) ([]byte, error) {
	var parts [][]byte
	total := 0
	for {
		chunk, err := br.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(parts) == 0 {
				return chunk, err
			}
			line := make([]byte, 0, total+len(chunk))
			for _, p := range parts {
				line = append(line, p...)
			}
			return append(line, chunk...), err
		}
		parts = append(parts, append([]byte(nil), chunk...))
		total += len(chunk)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
}

// segmentOverlaps reports whether src's seq window and rotation time can hold
// events that match filter.
func segmentOverlaps(src backfillSource, filter Filter) bool {
	return archiveOverlapsFilter(archiveInfo{
		FirstSeq:  src.firstSeq,
		LastSeq:   src.lastSeq,
		Timestamp: segmentTimestamp(src),
	}, filter)
}

// segmentTimestamp is the rotation instant encoded in a segment's file name,
// or zero when it cannot be parsed (archiveOverlapsFilter then reads it).
func segmentTimestamp(src backfillSource) time.Time {
	base := filepath.Base(src.path)
	if info, err := parseArchiveBasename(base); err == nil {
		return info.Timestamp
	}
	if ts, _, _, ok := parseRotatingBasename(base); ok {
		return ts
	}
	return time.Time{}
}
