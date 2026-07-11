package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Default rotation tunables. Operators can override these via the
// functional options below; the defaults match the architect's NFRs
// and were chosen so a busy city rotates roughly once per day at
// steady-state throughput.
const (
	defaultRotationMaxSize       = 256 * 1024 * 1024 // 256 MiB
	defaultRotationCheckRecords  = 1024
	defaultRotationCheckInterval = 60 * time.Second

	// recordFlockTimeout caps cross-process flock acquisition in Record.
	// Local-FS flock release latency is sub-millisecond on darwin/linux;
	// 250 ms is well above any reasonable single-write critical section
	// yet far below a user-perceptible stall. A dead writer that held the
	// lock is reaped by the kernel asynchronously — blocking on it can
	// pile up hundreds of stuck "gc event emit" processes.
	recordFlockTimeout = 250 * time.Millisecond
	// recordFlockRetryInterval is the fixed cadence between non-blocking
	// flock attempts. Fixed over exponential because contention is short
	// and uniform timing simplifies test assertions; 5 ms guarantees the
	// loop sees a freed lock within one cadence after a healthy release.
	recordFlockRetryInterval = 5 * time.Millisecond
)

// FileRecorder appends events to a JSONL file. It uses O_APPEND for
// cross-process safety, a mutex for in-process serialization, and a
// bounded-wait advisory file lock (flock) for cross-process serialization.
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

	// rotations tracks in-flight rotation goroutines so Close can
	// drain them. Without this, callers that read events.jsonl
	// immediately after Close() can miss events that are still in
	// rotating-* files awaiting gzip+rename.
	rotations sync.WaitGroup

	// Rotation tunables. Zero MaxSize disables size-triggered
	// rotation; ForceRotate continues to work regardless. The check
	// fields amortize the cost of stat-ing the active file: Record
	// only consults size when at least one of (recordCount %
	// rotationCheckRecords == 0) or (now - lastSizeCheck >=
	// rotationCheckInterval) holds.
	maxSize               int64
	rotationCheckRecords  int
	rotationCheckInterval time.Duration
	archiveRetainAge      time.Duration
	recordCount           uint64
	lastSizeCheck         time.Time
}

// FileRecorderOption customizes a FileRecorder at construction time.
// Use With* helpers to set specific tunables; an unmodified recorder
// keeps the defaults documented above.
type FileRecorderOption func(*FileRecorder)

// WithMaxSize sets the size threshold (in bytes) above which Record
// auto-rotates the active log. A non-positive value disables
// size-triggered rotation; ForceRotate continues to work.
func WithMaxSize(bytes int64) FileRecorderOption {
	return func(r *FileRecorder) { r.maxSize = bytes }
}

// WithRotationCheckRecords sets how often (in records) Record checks
// the active file's size against MaxSize. A larger interval reduces
// stat syscalls at the cost of overshooting the threshold by up to
// one window of records. Defaults to 1024.
func WithRotationCheckRecords(n int) FileRecorderOption {
	return func(r *FileRecorder) { r.rotationCheckRecords = n }
}

// WithRotationCheckInterval sets the time-based backstop for size
// checks: even on low-traffic cities that never reach
// rotationCheckRecords, Record will stat the active file at least
// once per interval. Defaults to 60s.
func WithRotationCheckInterval(d time.Duration) FileRecorderOption {
	return func(r *FileRecorder) { r.rotationCheckInterval = d }
}

// WithArchiveRetainAge sets the maximum age of canonical archive
// files kept after a successful rotation. A non-positive value keeps
// all archives forever.
func WithArchiveRetainAge(d time.Duration) FileRecorderOption {
	return func(r *FileRecorder) { r.archiveRetainAge = d }
}

// RotationResult is returned by ForceRotate (and B-3's API endpoint)
// describing the outcome of a single rotation. Field-stable contract:
// downstream wire layers depend on these names.
type RotationResult struct {
	// Rotated is true when an archive was produced; false on the
	// no-op path (empty active log).
	Rotated bool

	// Reason is populated only when Rotated is false; it explains
	// why the rotation was skipped.
	Reason string

	// ArchivePath is the absolute path to the canonical .gz archive
	// that this rotation produced. Empty when Rotated is false.
	ArchivePath string

	// FirstSeq, LastSeq is the seq window covered by the archive,
	// inclusive on both ends.
	FirstSeq uint64
	LastSeq  uint64

	// AnchorSeq is the seq of the events.rotated event written as
	// the first record of the new active log.
	AnchorSeq uint64

	// AnchorTimestamp is the timestamp on the anchor event.
	AnchorTimestamp time.Time

	// CompressionPending is true on success: the rename of the old
	// active file is synchronous, but gzip compression runs in a
	// background goroutine. Use Done to wait for completion.
	CompressionPending bool

	// Done is closed when the background gzip + rename completes
	// (whether the gzip itself succeeded or failed). Nil when
	// Rotated is false. Not serialized on the wire.
	Done <-chan struct{} `json:"-"`
}

// NewFileRecorder opens (or creates) the event log at path. It reads the tail
// sequence from any existing append-only log so new events continue
// monotonically. Parent directories are created as needed. Optional
// FileRecorderOption values configure rotation behavior; defaults
// are documented on each option.
//
// On open, the constructor performs a one-shot sweep on the log
// directory: legacy events.jsonl.archive-YYYYMMDD.gz files are
// renamed to the seq-stamped convention using the migration time as
// their retention timestamp, events.jsonl.rotating-* files left from a
// crashed rotation are gzipped into canonical archive names, and
// *.gz.tmp files are removed. Sweep failures are logged to stderr and
// do not block the recorder from opening.
func NewFileRecorder(path string, stderr io.Writer, opts ...FileRecorderOption) (*FileRecorder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating event log directory: %w", err)
	}

	if err := reapOrphanedRotatingFiles(filepath.Dir(path), stderr); err != nil {
		fmt.Fprintf(stderr, "events: rotation: orphan sweep: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	maxSeq, err := ReadLatestSeq(path)
	if err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}

	r := &FileRecorder{
		path:                  path,
		file:                  file,
		seq:                   maxSeq,
		stderr:                stderr,
		maxSize:               0,
		rotationCheckRecords:  defaultRotationCheckRecords,
		rotationCheckInterval: defaultRotationCheckInterval,
		lastSizeCheck:         time.Now(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Record appends an event to the log. It auto-fills Seq and Ts (if zero).
// Errors are written to stderr — never returned.
//
// Records are gated on size: when the recorder is configured with a
// non-zero MaxSize, Record may rotate the active log before writing
// if the file has crossed the threshold since the last check. Auto
// rotation is amortized — see WithRotationCheckRecords / Interval.
func (r *FileRecorder) Record(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}

	r.maybeAutoRotateLocked()

	// Cross-process flock contention only — r.mu already serializes
	// in-process callers, so this loop never spins for an in-process peer.
	// The bounded wait drops the recorder if a dead writer is holding the
	// lock instead of blocking forever and piling up processes.
	fd := int(r.file.Fd())
	deadline := time.Now().Add(recordFlockTimeout)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			fmt.Fprintf(r.stderr, "events: lock: %v\n", err) //nolint:errcheck // best-effort stderr
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(r.stderr, "events: lock: timed out after %dms waiting on flock at %s\n", recordFlockTimeout.Milliseconds(), r.path) //nolint:errcheck // best-effort stderr
			return
		}
		time.Sleep(recordFlockRetryInterval)
	}
	defer func() {
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			fmt.Fprintf(r.stderr, "events: unlock: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}()

	if err := r.writeRecordLocked(&e); err != nil {
		fmt.Fprintf(r.stderr, "events: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// writeRecordLocked appends e to the active log under the recorder
// mutex. Auto-fills Seq and Ts (if zero). The caller must already
// hold both r.mu and (if cross-process safety matters) the file's
// flock. Returns an error on marshal or write failure; the caller
// decides whether to log to stderr or surface it.
func (r *FileRecorder) writeRecordLocked(e *Event) error {
	if latest, err := readLatestActiveSeq(r.path); err == nil && latest > r.seq {
		r.seq = latest
	} else if err != nil {
		return fmt.Errorf("latest seq: %w", err)
	}
	r.seq++
	e.Seq = r.seq
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := r.file.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	r.recordCount++
	return nil
}

// maybeAutoRotateLocked is the size-gated rotation hook on the
// Record() hot path. It returns immediately if size-triggered
// rotation is disabled (MaxSize <= 0) or if neither the
// records-since-check nor the time-since-check threshold has been
// crossed. On a check, it stats the active file and triggers
// rotateLocked if size has exceeded MaxSize.
//
// Rotation failures are logged to stderr — Record's contract is
// best-effort and a failed rotation must not block subsequent
// writes. The next Record call will retry.
func (r *FileRecorder) maybeAutoRotateLocked() {
	if r.maxSize <= 0 {
		return
	}
	checkRecords := r.rotationCheckRecords
	if checkRecords <= 0 {
		checkRecords = defaultRotationCheckRecords
	}
	checkInterval := r.rotationCheckInterval
	if checkInterval <= 0 {
		checkInterval = defaultRotationCheckInterval
	}
	if r.recordCount%uint64(checkRecords) != 0 && time.Since(r.lastSizeCheck) < checkInterval {
		return
	}
	r.lastSizeCheck = time.Now()

	info, err := r.file.Stat()
	if err != nil {
		fmt.Fprintf(r.stderr, "events: rotation: size check: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	if info.Size() < r.maxSize {
		return
	}
	if _, err := r.rotateLocked(); err != nil {
		fmt.Fprintf(r.stderr, "events: rotation: auto-rotate failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
}

// ForceRotate rotates the active log immediately, ignoring the size
// threshold. Safe to call concurrently with Record. Returns a
// no-op result with Rotated=false if the active log is empty (an
// empty file is never archived).
func (r *FileRecorder) ForceRotate() (RotationResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RotationResult{}, fmt.Errorf("recorder is closed")
	}
	return r.rotateLocked()
}

// rotateLocked performs the close+rename+open+anchor sequence. It
// must be called with r.mu held. The caller is responsible for
// checking r.closed.
//
// On success, the prior active log is renamed to
// events.jsonl.rotating-<ts> and a background goroutine compresses
// it to its canonical archive basename. The result's Done channel
// closes when that goroutine finishes.
func (r *FileRecorder) rotateLocked() (RotationResult, error) {
	info, err := r.file.Stat()
	if err != nil {
		return RotationResult{}, fmt.Errorf("stat active log: %w", err)
	}
	if info.Size() == 0 {
		return RotationResult{Rotated: false, Reason: "active log is empty"}, nil
	}

	first, last, err := readSeqWindow(r.path)
	if err != nil {
		return RotationResult{}, fmt.Errorf("reading seq window: %w", err)
	}

	ts := time.Now().UTC()
	dir := filepath.Dir(r.path)
	archiveBase := formatArchiveBasename(ts, first, last)
	archivePath := filepath.Join(dir, archiveBase)
	rotatingPath := filepath.Join(dir, formatRotatingBasename(ts, first, last))

	if err := r.file.Close(); err != nil {
		return RotationResult{}, fmt.Errorf("closing active log: %w", err)
	}
	r.file = nil

	if err := os.Rename(r.path, rotatingPath); err != nil {
		// Try to recover: re-open the original path. If that also
		// fails, mark the recorder closed so subsequent Record calls
		// drop cleanly instead of dereferencing a nil file under
		// maybeAutoRotateLocked.
		if newF, openErr := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); openErr == nil {
			r.file = newF
		} else {
			r.closed = true
		}
		return RotationResult{}, fmt.Errorf("renaming active log: %w", err)
	}

	newFile, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return RotationResult{}, fmt.Errorf("opening new active log: %w", err)
	}
	r.file = newFile
	r.recordCount = 0
	r.lastSizeCheck = time.Now()

	payload := RotatedPayload{
		PriorArchive:  archiveBase,
		PriorFirstSeq: first,
		PriorLastSeq:  last,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return RotationResult{}, fmt.Errorf("marshaling anchor payload: %w", err)
	}
	anchor := Event{
		Type:    EventsRotated,
		Actor:   "events",
		Message: fmt.Sprintf("rotated to %s", archiveBase),
		Payload: payloadBytes,
	}
	if err := r.writeRecordLocked(&anchor); err != nil {
		return RotationResult{}, fmt.Errorf("writing anchor event: %w", err)
	}
	if err := r.file.Sync(); err != nil {
		fmt.Fprintf(r.stderr, "events: rotation: sync new active log: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	done := make(chan struct{})
	retainAge := r.archiveRetainAge
	r.rotations.Add(1)
	go func() {
		defer r.rotations.Done()
		defer close(done)
		if err := gzipAndArchive(rotatingPath, archivePath, r.stderr); err != nil {
			// gzipAndArchive already wrote to stderr.
			_ = err
			return
		}
		if err := reapExpiredArchives(dir, retainAge, r.stderr); err != nil {
			fmt.Fprintf(r.stderr, "events: rotation: archive retention: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}()

	return RotationResult{
		Rotated:            true,
		ArchivePath:        archivePath,
		FirstSeq:           first,
		LastSeq:            last,
		AnchorSeq:          anchor.Seq,
		AnchorTimestamp:    anchor.Ts,
		CompressionPending: true,
		Done:               done,
	}, nil
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
	seq, err := ReadLatestSeq(r.path)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	if seq > r.seq {
		r.seq = seq
	}
	seq = r.seq
	r.mu.Unlock()
	return seq, nil
}

// Watch returns a Watcher that delivers every retained event with Seq > afterSeq
// exactly once per watcher, in sequence order. When afterSeq is below the active
// file's history it first backfills across the sibling .gz archives and in-flight
// rotating-* files (lazily, on the first Next call) before tailing the active
// file. It also detects rotation mid-watch (inode change) and catches up across
// the just-rotated archive before re-tailing the fresh active file, so no
// pre-rotation tail is lost. Backfill/catch-up reads are deduped by a strictly
// monotonic seq cursor; across separate watcher instances the stream is
// at-least-once (callers already de-dupe by seq).
//
// Watch itself stays O(1): a single stat/open of the active file plus one
// ReadLatestSeq. The (potentially expensive) archive walk is deferred to Next so
// a never-polled watcher — e.g. the multiplexer's attach probe — costs nothing.
func (r *FileRecorder) Watch(ctx context.Context, afterSeq uint64) (Watcher, error) {
	var offset int64
	var inode uint64
	var size int64
	var haveStat bool
	// Open the active file once and keep the fd: the resume backfill reads its
	// tail leg from this fd (capped at the captured size), so a rotation landing
	// mid-backfill still reads the correct bytes instead of the fresh file the
	// path now names.
	activeFile, openErr := os.Open(r.path)
	if openErr == nil {
		if info, serr := activeFile.Stat(); serr == nil {
			size = info.Size()
			inode = inodeOf(info)
			haveStat = true
		}
	}
	latestSeq, err := ReadLatestSeq(r.path)
	if err != nil {
		if activeFile != nil {
			_ = activeFile.Close()
		}
		return nil, err
	}
	r.mu.Lock()
	if latestSeq > r.seq {
		r.seq = latestSeq
	}
	if haveStat && afterSeq >= latestSeq {
		offset = size
	}
	r.mu.Unlock()

	w := &fileWatcher{
		path:       r.path,
		afterSeq:   afterSeq,
		maxSeq:     afterSeq,
		ctx:        ctx,
		poll:       250 * time.Millisecond,
		offset:     offset,
		inode:      inode,
		done:       make(chan struct{}),
		activeFile: activeFile,
		activeSize: size,
	}
	// Backfill is needed only when the cursor predates the active file's head.
	if haveStat && afterSeq < latestSeq {
		w.needsBackfill = true
	} else if activeFile != nil {
		_ = activeFile.Close()
		w.activeFile = nil
	}
	return w, nil
}

// Close closes the underlying file. It is safe to call multiple times;
// subsequent calls after the first return nil.
//
// Close drains in-flight rotation goroutines before returning so any
// rotating-* sibling files have been promoted to canonical archives
// by the time the caller starts reading. This trade-off — a brief
// block for clean shutdown semantics — matches the architect's
// crash-safe NFR-06 goal: a clean exit must not strand events in a
// rotating-* file that ReadAll wouldn't pick up until the next
// process opens a recorder and runs the orphan reaper.
func (r *FileRecorder) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	file := r.file
	r.file = nil
	r.mu.Unlock()

	r.rotations.Wait()

	if file == nil {
		return nil
	}
	return file.Close()
}

// WaitForRotations blocks until every in-flight rotation goroutine
// has completed. Useful for tests that read archives immediately
// after triggering rotations and for callers that want to confirm
// disk state is fully settled before snapshotting.
func (r *FileRecorder) WaitForRotations() {
	r.rotations.Wait()
}

// fileWatcher polls a JSONL file for new events. It tracks the file's
// inode in addition to the byte offset so a rotation (rename + fresh
// re-open) is detected and the offset is reset to 0 against the new
// active file. The afterSeq cursor dedupes against already-yielded
// events.
type fileWatcher struct {
	path     string
	afterSeq uint64
	maxSeq   uint64 // strictly monotonic guard: highest seq delivered so far
	ctx      context.Context
	poll     time.Duration
	offset   int64
	inode    uint64
	buf      []Event // buffered events from last poll
	done     chan struct{}

	// Resume-backfill state (W1): the archive/rotating segments to replay before
	// tailing, and the active file's fd + size captured at Watch time.
	needsBackfill bool
	activeFile    *os.File
	activeSize    int64

	closeOnce sync.Once
}

// errWatcherClosed is returned by Next after Close.
var errWatcherClosed = fmt.Errorf("watcher closed")

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
			return Event{}, errWatcherClosed
		default:
		}

		// Resume backfill (W1): replay archived/rotating events before tailing.
		// Runs lazily here, one source per batch, so a never-polled watcher pays
		// nothing and memory stays bounded to one segment.
		if w.needsBackfill {
			if err := w.runResumeBackfill(); err != nil {
				return Event{}, err
			}
			if len(w.buf) > 0 {
				continue
			}
		}

		// Detect rotation by inode change. Before resetting the offset, catch up
		// across the just-rotated archive (W2): events appended to the OLD active
		// file after the last poll now live only in the rotating/.gz file, so a
		// bare offset reset would drop them. Commit the fresh identity only after
		// a successful catch-up; on error keep the old identity and retry next
		// poll (an early commit would make the next poll see no rotation).
		if info, err := os.Stat(w.path); err == nil {
			if curr := inodeOf(info); curr != 0 {
				if w.inode != 0 && curr != w.inode {
					if cerr := w.catchUpRotation(); cerr != nil {
						if isCancelErr(cerr) {
							return Event{}, cerr
						}
						// Transient read error: leave identity/offset, retry.
					} else {
						w.inode = curr
						w.offset = 0
						if len(w.buf) > 0 {
							continue
						}
					}
				} else {
					w.inode = curr
				}
			}
		}

		// Poll for new events.
		evts, newOffset, err := ReadFrom(w.path, w.offset)
		if err != nil {
			return Event{}, err
		}
		w.offset = newOffset

		// Filter to events after our cursor.
		for _, e := range evts {
			if e.Seq > w.maxSeq {
				w.maxSeq = e.Seq
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
			return Event{}, errWatcherClosed
		case <-time.After(w.poll):
		}
	}
}

// runResumeBackfill streams the next resume segment (archive, rotating file, or
// the captured active-file tail) into w.buf, advancing w.maxSeq. It processes
// one segment per call so memory stays bounded and the consumer paces it; when
// every segment is exhausted it clears needsBackfill and positions the tail at
// the captured active size. The archive walk holds a concurrency slot so N cold
// resumes queue instead of multiplying gunzip cost.
func (w *fileWatcher) runResumeBackfill() error {
	if err := acquireBackfillSlot(w.ctx); err != nil {
		return err
	}
	defer releaseBackfillSlot()

	srcs, err := listBackfillSources(filepath.Dir(w.path), w.maxSeq)
	if err != nil {
		// Directory listing failed: skip straight to the active-file leg + tail.
		srcs = nil
	}
	filter := Filter{}
	for i := range srcs {
		if err := streamBackfillSource(w.ctx, w.done, srcs[i], filter, &w.maxSeq, &w.buf); err != nil {
			return err
		}
		if len(w.buf) > 0 {
			return nil // one segment per batch; resume on the next Next
		}
	}
	// Active-file leg: read from the captured fd (not the path) up to the size
	// captured at Watch time, so a rotation mid-backfill cannot swap the bytes.
	if w.activeFile != nil {
		if err := w.streamActiveLeg(filter); err != nil {
			return err
		}
	}
	w.finishBackfill()
	return nil
}

// streamActiveLeg replays the captured active-file fd (0..activeSize) into
// w.buf, honoring the monotonic guard and cancellation.
func (w *fileWatcher) streamActiveLeg(filter Filter) error {
	if _, err := w.activeFile.Seek(0, 0); err != nil {
		return err
	}
	lr := &io.LimitedReader{R: w.activeFile, N: w.activeSize}
	guard := func(e Event) bool {
		select {
		case <-w.ctx.Done():
			return false
		case <-w.done:
			return false
		default:
		}
		if e.Seq > w.maxSeq {
			w.maxSeq = e.Seq
			w.buf = append(w.buf, e)
		}
		return true
	}
	if err := scanJSONL(lr, filter, guard); err != nil {
		return err
	}
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	case <-w.done:
		return errWatcherClosed
	default:
		return nil
	}
}

// finishBackfill releases the captured fd and positions the incremental tail at
// the captured active size so it re-reads only bytes appended after Watch.
func (w *fileWatcher) finishBackfill() {
	w.needsBackfill = false
	if w.activeFile != nil {
		_ = w.activeFile.Close()
		w.activeFile = nil
	}
	if w.offset < w.activeSize {
		w.offset = w.activeSize
	}
}

// catchUpRotation streams events after w.maxSeq from the archives/rotating files
// (where the just-rotated tail now lives) into w.buf before the caller resets
// the offset. AfterSeq = maxSeq bounds it to the rotation window.
func (w *fileWatcher) catchUpRotation() error {
	if err := acquireBackfillSlot(w.ctx); err != nil {
		return err
	}
	defer releaseBackfillSlot()

	srcs, err := listBackfillSources(filepath.Dir(w.path), w.maxSeq)
	if err != nil {
		return err
	}
	filter := Filter{AfterSeq: w.maxSeq}
	for i := range srcs {
		if err := streamBackfillSource(w.ctx, w.done, srcs[i], filter, &w.maxSeq, &w.buf); err != nil {
			return err
		}
	}
	return nil
}

func isCancelErr(err error) bool {
	return errors.Is(err, errWatcherClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Close unblocks any pending Next call and releases the captured active fd.
func (w *fileWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		if w.activeFile != nil {
			_ = w.activeFile.Close()
			w.activeFile = nil
		}
	})
	return nil
}
