package events

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// FileRecorder appends events to a JSONL file. It uses O_APPEND for
// cross-process safety and a mutex for in-process serialization.
// Recording errors are written to stderr and never returned.
//
// FileRecorder implements [Provider] — it can both record and read events.
type FileRecorder struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	seq    uint64
	stderr io.Writer
	closed bool
}

// NewFileRecorder opens (or creates) the event log at path. It reads the tail
// sequence from any existing append-only log so new events continue
// monotonically. Parent directories are created as needed.
func NewFileRecorder(path string, stderr io.Writer) (*FileRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating event log directory: %w", err)
	}

	maxSeq, err := ReadLatestSeq(path)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}

	return &FileRecorder{
		path:   path,
		file:   file,
		seq:    maxSeq,
		stderr: stderr,
	}, nil
}

// Record appends an event to the log. It auto-fills Seq and Ts (if zero).
// Errors are written to stderr — never returned.
func (r *FileRecorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}
	if err := syscall.Flock(int(r.file.Fd()), syscall.LOCK_EX); err != nil {
		fmt.Fprintf(r.stderr, "events: lock: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	defer func() {
		if err := syscall.Flock(int(r.file.Fd()), syscall.LOCK_UN); err != nil {
			fmt.Fprintf(r.stderr, "events: unlock: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}()

	if latest, err := ReadLatestSeq(r.path); err == nil && latest > r.seq {
		r.seq = latest
	} else if err != nil {
		fmt.Fprintf(r.stderr, "events: latest seq: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	r.seq++
	e.Seq = r.seq
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

// ListTail returns trailing matching events from the underlying file.
func (r *FileRecorder) ListTail(filter Filter, limit int) ([]Event, error) {
	return ReadFilteredTail(r.path, filter, limit)
}

// LatestSeq returns the highest sequence number in the event log.
func (r *FileRecorder) LatestSeq() (uint64, error) {
	r.mu.Lock()
	seq := r.seq
	r.mu.Unlock()
	return seq, nil
}

// Watch returns a Watcher that polls the event file for new events.
func (r *FileRecorder) Watch(ctx context.Context, afterSeq uint64) (Watcher, error) {
	var offset int64
	r.mu.Lock()
	if afterSeq >= r.seq {
		if info, err := r.file.Stat(); err == nil {
			offset = info.Size()
		}
	}
	r.mu.Unlock()
	return &fileWatcher{
		path:     r.path,
		afterSeq: afterSeq,
		ctx:      ctx,
		poll:     250 * time.Millisecond,
		offset:   offset,
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
