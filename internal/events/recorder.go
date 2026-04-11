package events

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileRecorder appends events to a JSONL file. It uses O_APPEND for
// cross-process safety of the data file and a sidecar counter file
// (events.jsonl.seq) protected by an advisory file lock for allocating
// globally unique, monotonic sequence numbers across processes.
// Recording errors are written to stderr and never returned.
//
// FileRecorder implements [Provider] — it can both record and read events.
type FileRecorder struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	counter *seqCounter
	stderr  io.Writer
	closed  bool
}

// NewFileRecorder opens (or creates) the event log at path. It scans any
// existing file to find the maximum sequence number so new events continue
// monotonically, then initializes a sidecar counter file
// (<path>.seq) used to allocate sequence numbers under an advisory file
// lock. Parent directories are created as needed.
//
// The sidecar counter is the source of truth for the next seq. The
// max-seq scan is only used to seed the sidecar the first time it is
// created, preserving backwards compatibility with pre-existing
// events.jsonl files that have no sidecar.
func NewFileRecorder(path string, stderr io.Writer) (*FileRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating event log directory: %w", err)
	}

	// Scan existing file for max seq so we can seed the sidecar counter
	// the first time it is created.
	var maxSeq uint64
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) == nil && e.Seq > maxSeq {
				maxSeq = e.Seq
			}
		}
		if err := scanner.Err(); err != nil {
			f.Close() //nolint:errcheck // closing after scan error
			return nil, fmt.Errorf("scanning event log: %w", err)
		}
		f.Close() //nolint:errcheck // read-only scan
	}

	counter, err := newSeqCounter(seqCounterPath(path), maxSeq)
	if err != nil {
		return nil, fmt.Errorf("initializing seq counter: %w", err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}

	return &FileRecorder{
		path:    path,
		file:    file,
		counter: counter,
		stderr:  stderr,
	}, nil
}

// Record appends an event to the log. It auto-fills Seq and Ts (if zero).
// Errors are written to stderr — never returned.
//
// Seq is allocated via the sidecar counter under an advisory file lock,
// making it safe to have multiple FileRecorder instances (in the same
// or different processes) writing to the same events.jsonl without
// producing duplicate sequence numbers.
func (r *FileRecorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	next, err := r.counter.Next()
	if err != nil {
		fmt.Fprintf(r.stderr, "events: seq: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	e.Seq = next
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}

	data, err := json.Marshal(e)
	if err != nil {
		fmt.Fprintf(r.stderr, "events: marshal: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	data = append(data, '\n')
	if _, err := r.file.Write(data); err != nil {
		fmt.Fprintf(r.stderr, "events: write: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// List returns events matching the filter from the underlying file.
func (r *FileRecorder) List(filter Filter) ([]Event, error) {
	return ReadFiltered(r.path, filter)
}

// LatestSeq returns the highest sequence number in the event log. It
// reads the sidecar counter under the same advisory lock used by
// Record, so it reflects allocations made by other processes as well
// as this one.
func (r *FileRecorder) LatestSeq() (uint64, error) {
	return ReadLatestSeq(r.path)
}

// Watch returns a Watcher that polls the event file for new events.
func (r *FileRecorder) Watch(ctx context.Context, afterSeq uint64) (Watcher, error) {
	return &fileWatcher{
		path:     r.path,
		afterSeq: afterSeq,
		ctx:      ctx,
		poll:     250 * time.Millisecond,
		done:     make(chan struct{}),
	}, nil
}

// Close closes the underlying file. It is safe to call multiple times;
// subsequent calls after the first return nil.
func (r *FileRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}

// fileWatcher polls a JSONL file for new events.
type fileWatcher struct {
	path      string
	afterSeq  uint64
	ctx       context.Context
	poll      time.Duration
	offset    int64
	buf       []Event // buffered events from last poll
	done      chan struct{}
	closeOnce sync.Once
}

// Next blocks until the next event is available or the context is canceled.
func (w *fileWatcher) Next() (Event, error) {
	for {
		// Drain buffer first.
		if len(w.buf) > 0 {
			e := w.buf[0]
			w.buf = w.buf[1:]
			return e, nil
		}

		// Check context and close.
		select {
		case <-w.ctx.Done():
			return Event{}, w.ctx.Err()
		case <-w.done:
			return Event{}, fmt.Errorf("watcher closed")
		default:
		}

		// Poll for new events.
		evts, newOffset, err := ReadFrom(w.path, w.offset)
		if err != nil {
			return Event{}, err
		}
		w.offset = newOffset

		// Filter to events after our cursor.
		for _, e := range evts {
			if e.Seq > w.afterSeq {
				w.afterSeq = e.Seq
				w.buf = append(w.buf, e)
			}
		}

		if len(w.buf) > 0 {
			continue // drain buffer on next iteration
		}

		// No new events — wait and retry.
		select {
		case <-w.ctx.Done():
			return Event{}, w.ctx.Err()
		case <-w.done:
			return Event{}, fmt.Errorf("watcher closed")
		case <-time.After(w.poll):
		}
	}
}

// Close unblocks any pending Next call.
func (w *fileWatcher) Close() error {
	w.closeOnce.Do(func() { close(w.done) })
	return nil
}
