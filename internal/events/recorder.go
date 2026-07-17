package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// ListInFlight returns events matching the filter, including any still stranded
// in an in-flight events.jsonl.rotating-* file during the asynchronous
// compression window that plain List cannot see. Results are seq-ordered and
// de-duplicated by seq. It implements [InFlightProvider] so the event-list
// keyset walk cannot skip a just-rotated segment mid-rotation.
func (r *FileRecorder) ListInFlight(filter Filter) ([]Event, error) {
	return ReadFilteredWithInFlight(r.path, filter)
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
		w.bfActive = true
	} else if activeFile != nil {
		// No backfill: close the captured fd immediately (once).
		w.closeFd()
	}
	return w, nil
}

// closeFd closes the captured active fd exactly once. The pointer is never
// niled (writing it would race an unsynchronized read in the Next goroutine);
// os.File tolerates a concurrent Close vs Read, so a mid-read Close just makes
// the in-flight scan return an error, which the caller treats as cancellation.
func (w *fileWatcher) closeFd() {
	w.closeFdOnce.Do(func() {
		if w.activeFile != nil {
			_ = w.activeFile.Close()
		}
	})
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
	// tailing, and the active file's fd + size captured at Watch time. The fd is
	// assigned once in Watch and never reassigned; closeFdOnce closes it exactly
	// once from whichever of Close / finishBackfill runs first, so the pointer is
	// never written after Watch and cannot race a read.
	needsBackfill bool
	activeFile    *os.File
	activeSize    int64
	closeFdOnce   sync.Once

	// Incremental backfill iterator state (bounded memory): the remaining
	// segments, the index into them, whether the active leg is pending, and the
	// currently-open segment reader (persisted across Next batches so each
	// segment is read exactly once). bfSources/bfIdx/bfListed are owned solely by
	// the single Next consumer. bfReader is the one exception: a consumer that
	// abandons the watcher mid-backfill (e.g. an SSE client that disconnects
	// after a partial archive batch) reaches Close on a possibly different
	// goroutine than Next, so bfMu serializes every bfReader open/read/close with
	// that Close and lets it release the archive fd + gzip.Reader instead of
	// leaking them until GC.
	bfMu       sync.Mutex
	bfSources  []backfillSource
	bfIdx      int
	bfListed   bool
	bfActive   bool
	bfReader   *segmentReader
	bfCatchUp  bool // true while draining a mid-watch rotation catch-up
	catchUpErr int  // consecutive catch-up failures, for backoff

	closeOnce sync.Once
}

// errWatcherClosed is returned by Next after Close.
var errWatcherClosed = fmt.Errorf("watcher closed")

// Next blocks until the next event is available or the context is canceled. It
// runs a uniform per-poll pipeline: drain any buffered batch, then resume
// backfill (W1) and rotation catch-up (W2) — each reporting whether Next should
// re-loop — before tailing the active file. Each step is a small method so the
// loop stays readable and the individual state machines are independently
// testable.
func (w *fileWatcher) Next() (Event, error) {
	for {
		if len(w.buf) > 0 {
			return w.pop(), nil
		}
		if err := w.ctxErr(); err != nil {
			return Event{}, err
		}
		cont, err := w.stepResume()
		if err != nil {
			return Event{}, err
		}
		if cont {
			continue
		}
		cont, err = w.stepRotation()
		if err != nil {
			return Event{}, err
		}
		if cont {
			continue
		}
		// Tail the active file from the current offset.
		if err := w.stepTail(); err != nil {
			return Event{}, err
		}
		if len(w.buf) > 0 {
			continue
		}
		// No new events — wait and retry.
		if err := w.sleep(); err != nil {
			return Event{}, err
		}
	}
}

// stepResume advances the resume backfill (W1) by one batch, replaying archived
// and rotating events before the tail starts. It runs lazily and buffers at most
// backfillBatch events per call, so a never-polled watcher pays nothing and
// resident memory stays bounded to one batch. It returns cont=true when a batch
// was buffered (Next should yield it) and false once resume is done or was never
// needed.
func (w *fileWatcher) stepResume() (cont bool, err error) {
	if !w.needsBackfill {
		return false, nil
	}
	if err := w.stepBackfill(Filter{}, false); err != nil {
		return false, err
	}
	return len(w.buf) > 0, nil
}

// ctxErr reports a terminal error when the watcher's context is canceled or it
// has been closed, so Next can bail before doing more work. It prefers the
// context cause so callers that classify context.Canceled / DeadlineExceeded
// keep that signal.
func (w *fileWatcher) ctxErr() error {
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	case <-w.done:
		return errWatcherClosed
	default:
		return nil
	}
}

// stepRotation detects a mid-watch rotation (active-file inode change) and
// drains the just-rotated archive(s) before the tail resumes on the fresh
// active file. It stays correct across repeated rotations that land while an
// earlier catch-up is still draining: the same-inode fast path is suppressed
// while a catch-up is active (so a later rotation that reused our anchor inode
// cannot abandon the drain and poll the fresh file at a stale offset), and
// stepCatchUp re-lists by seq so an extra rotation window is not skipped.
//
// It returns cont=true when Next should re-loop (a catch-up batch was buffered,
// more batches remain, or a transient error backed off) and cont=false when no
// rotation is in progress and Next should tail the active file.
func (w *fileWatcher) stepRotation() (cont bool, err error) {
	info, statErr := os.Stat(w.path)
	if statErr != nil {
		return false, nil // let stepTail surface any real read error
	}
	curr := inodeOf(info)
	if curr == 0 {
		return false, nil
	}
	if !w.bfCatchUp && (w.inode == 0 || curr == w.inode) {
		// Steady state: same active file (or first poll). Adopt the inode and
		// rewind a cursor stranded past EOF (an inode reuse the stat window
		// missed, or a truncation) so ReadFrom does not seek past the tail.
		w.inode = curr
		if w.offset > info.Size() {
			w.offset = 0
		}
		return false, nil
	}

	// A rotation was detected, or a catch-up is still draining. Drive it.
	done, cerr := w.stepCatchUp()
	if cerr != nil {
		if isCancelErr(cerr) {
			return false, cerr
		}
		// Transient read error: keep the old identity/offset and back off —
		// never fall through to a stale-offset poll (which would advance maxSeq
		// past the unread window and lose it forever).
		if berr := w.backoffCatchUp(cerr); berr != nil {
			return false, berr
		}
		return true, nil
	}
	if len(w.buf) > 0 || !done {
		return true, nil // deliver this batch, or keep draining
	}
	// Catch-up drained and the active file is contiguous with the cursor:
	// commit the fresh identity and re-tail from its start.
	w.catchUpErr = 0
	w.inode = curr
	w.offset = 0
	return false, nil
}

// backoffCatchUp records a transient rotation catch-up failure, logs it at a
// throttled cadence, and waits with escalating backoff. It returns a terminal
// error when the wait was canceled/closed and nil to retry.
func (w *fileWatcher) backoffCatchUp(cerr error) error {
	w.catchUpErr++
	if w.catchUpErr == 1 || w.catchUpErr%40 == 0 {
		log.Printf("events: watcher rotation catch-up failed (attempt %d) for %q: %v", w.catchUpErr, w.path, cerr)
	}
	return w.sleepBackoff()
}

// stepTail polls the active file from the current offset and buffers any events
// beyond the cursor. It reads through an identity-checked fd (readActiveTail) so
// a rotation landing in the catch-up conclusion window — after stepCatchUp's
// final empty re-list but before this read — cannot redirect the tail to a fresh
// active file and advance maxSeq past a just-archived window. On such a mismatch
// it defers to rotation catch-up without advancing. The strictly-monotonic
// maxSeq guard drops any overlap re-read after a rewind or catch-up.
func (w *fileWatcher) stepTail() error {
	evts, newOffset, matched, err := readActiveTail(w.path, w.offset, w.inode)
	if err != nil {
		return err
	}
	if !matched {
		// The active path rotated between the rotation check and this read. Do
		// not advance maxSeq/offset from the unverified fresh file; the next
		// poll's rotation catch-up re-lists by seq and drains the just-archived
		// window before this fresh active file is tailed.
		return nil
	}
	w.offset = newOffset
	for _, e := range evts {
		if e.Seq > w.maxSeq {
			w.maxSeq = e.Seq
			w.buf = append(w.buf, e)
		}
	}
	return nil
}

// readActiveTail reads events appended to the active log after offset, reading
// THROUGH a freshly-opened fd whose identity is checked against wantInode. A
// rotation renames the active file and creates a fresh one at the same path, so
// reading by path alone can observe a different (post-rotation) file than the
// one the caller committed and skip a just-archived window. Reading through the
// fd — and refusing to advance when the path now resolves to a different inode
// than wantInode — keeps the tail identity-stable: a rotation that lands mid-read
// still reads the committed inode's bytes, and a rotation that lands before the
// read is deferred to rotation catch-up (matched=false) rather than tailing an
// unverified file.
//
// wantInode==0 (first poll, before any inode was committed) or a filesystem that
// reports inode 0 disables the gate and falls back to plain by-path tailing.
func readActiveTail(path string, offset int64, wantInode uint64) (evts []Event, newOffset int64, matched bool, err error) {
	f, oerr := os.Open(path)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			// The active file is briefly absent between a rotation's rename and
			// re-create; treat it as "no new bytes" and let the next poll retry.
			return nil, offset, true, nil
		}
		return nil, offset, false, fmt.Errorf("reading events: %w", oerr)
	}
	defer f.Close() //nolint:errcheck // read-only file

	if wantInode != 0 {
		info, serr := f.Stat()
		if serr != nil {
			return nil, offset, false, fmt.Errorf("stat active log: %w", serr)
		}
		if got := inodeOf(info); got != 0 && got != wantInode {
			// The active path now names a different inode than the committed one:
			// a rotation raced this tail. Do not advance.
			return nil, offset, false, nil
		}
	}

	evts, newOffset, err = readEventsFrom(f, offset)
	return evts, newOffset, true, err
}

func (w *fileWatcher) pop() Event {
	e := w.buf[0]
	w.buf = w.buf[1:]
	return e
}

// waitPoll blocks for d, returning nil when the interval elapsed (keep polling)
// or a terminal error when the watcher was canceled or closed meanwhile. The
// context cause is preferred over errWatcherClosed so a Next blocked here still
// surfaces context.Canceled / context.DeadlineExceeded to callers that classify
// it — the watcher contract is "blocks until an event arrives or ctx is
// canceled", not "until Close".
func (w *fileWatcher) waitPoll(d time.Duration) error {
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	case <-w.done:
		if err := w.ctx.Err(); err != nil {
			return err
		}
		return errWatcherClosed
	case <-time.After(d):
		return nil
	}
}

// sleep waits one poll interval. See waitPoll for the return contract.
func (w *fileWatcher) sleep() error { return w.waitPoll(w.poll) }

// sleepBackoff waits poll * min(catchUpErr, 8) so a persistently failing
// catch-up (e.g. a poisoned archive) does not re-gunzip at 4/sec and starve
// other watchers' backfill slots. See waitPoll for the return contract.
func (w *fileWatcher) sleepBackoff() error {
	mult := time.Duration(w.catchUpErr)
	if mult > 8 {
		mult = 8
	}
	return w.waitPoll(w.poll * mult)
}

// stepBackfill advances the resume backfill by up to backfillBatch events. It
// lists the segments once, drains them one batch per call (keeping each segment
// reader open across calls so each segment is read exactly once), then reads the
// captured active leg. When every segment and the active leg are exhausted it
// clears needsBackfill and positions the tail at the captured active size.
func (w *fileWatcher) stepBackfill(filter Filter, catchUp bool) error {
	if err := acquireBackfillSlot(w.ctx, w.done); err != nil {
		return err
	}
	defer releaseBackfillSlot()

	if err := w.ensureBackfillListed(); err != nil {
		return err
	}
	produced, err := w.drainSegments(filter)
	if err != nil || produced {
		return err
	}
	if w.bfActive && w.activeFile != nil {
		return w.drainActiveLeg(filter)
	}
	if !catchUp {
		w.finishResume()
	}
	return nil
}

// ensureBackfillListed lists the archive/rotating segments to replay, once per
// backfill/catch-up round. A listing error is surfaced (not swallowed) so a
// resuming caller reconnects/retries instead of silently receiving active-only
// history that omits archived events the cursor asked for.
func (w *fileWatcher) ensureBackfillListed() error {
	if w.bfListed {
		return nil
	}
	srcs, err := listBackfillSources(filepath.Dir(w.path), w.maxSeq)
	if err != nil {
		return err
	}
	w.bfSources = srcs
	w.bfIdx = 0
	w.bfListed = true
	return nil
}

// drainSegments reads up to one batch from the pending archive/rotating
// segments, opening each segment reader lazily and skipping vanished or
// exhausted segments. It returns produced=true once a batch is buffered so the
// caller yields it before doing more work.
func (w *fileWatcher) drainSegments(filter Filter) (produced bool, err error) {
	// Hold bfMu for the whole drain so a Close arriving from another goroutine
	// (an SSE client disconnecting mid-replay) cannot close the archive fd/gzip
	// stream while readInto is reading through it. The lock is released between
	// Next calls, so Close can still promptly reclaim a segment left open across
	// batches.
	w.bfMu.Lock()
	defer w.bfMu.Unlock()
	for w.bfIdx < len(w.bfSources) {
		if w.bfReader == nil {
			sr, oerr := openSegmentReader(w.bfSources[w.bfIdx])
			if oerr != nil {
				return false, oerr
			}
			if sr == nil { // vanished (promoted rotating file); its .gz covers it
				w.bfIdx++
				continue
			}
			w.bfReader = sr
		}
		eof, rerr := w.bfReader.readInto(withAfterSeq(filter, w.maxSeq), &w.maxSeq, &w.buf, backfillBatch)
		if rerr != nil {
			w.bfReader.close()
			w.bfReader = nil
			return false, rerr
		}
		if eof {
			w.bfReader.close()
			w.bfReader = nil
			w.bfIdx++
		}
		if len(w.buf) > 0 {
			return true, nil // yield this batch; resume on the next call
		}
	}
	return false, nil
}

// drainActiveLeg reads up to one batch from the captured active-file leg (the
// resume tail, bounded to the size captured at Watch). On EOF it finishes the
// resume and positions the incremental tail at that captured size.
func (w *fileWatcher) drainActiveLeg(filter Filter) error {
	eof, err := w.readActiveLegLocked(filter)
	if err != nil {
		return err
	}
	if eof {
		// finishResume re-enters resetBackfillIter (which locks bfMu), so run it
		// once the read lock is released to avoid a self-deadlock.
		w.finishResume()
	}
	return nil
}

// readActiveLegLocked reads up to one batch from the captured active leg under
// bfMu (see the bfReader field comment) and reports eof when the leg is
// exhausted. The active-leg reader wraps the caller-owned active fd (its own
// close is a no-op for that fd), but it is still guarded so a concurrent Close
// cannot race the bfReader pointer.
func (w *fileWatcher) readActiveLegLocked(filter Filter) (eof bool, err error) {
	w.bfMu.Lock()
	defer w.bfMu.Unlock()
	if w.bfReader == nil {
		sr, serr := activeSegmentReader(w.activeFile, w.activeSize)
		if serr != nil {
			return false, serr
		}
		w.bfReader = sr
	}
	done, rerr := w.bfReader.readInto(withAfterSeq(filter, w.maxSeq), &w.maxSeq, &w.buf, backfillBatch)
	if rerr != nil {
		w.bfReader.close()
		w.bfReader = nil
		return false, rerr
	}
	if done {
		w.bfReader.close() // closes the wrapper, not the caller-owned fd
		w.bfReader = nil
	}
	return done, nil
}

// stepCatchUp advances a mid-watch rotation catch-up by up to backfillBatch
// events, reusing the incremental segment machinery bounded by AfterSeq=maxSeq
// (the rotation window). When the frozen source list drains it re-lists against
// the advanced cursor: a rotation that landed mid-drain appended a new segment
// whose window sits above the original list, and re-listing by seq (rather than
// by inode) picks it up even when the fresh active file reused a prior inode.
// Returns done=true only once no archived segment holds events beyond the
// cursor, i.e. the active file is the next contiguous source.
func (w *fileWatcher) stepCatchUp() (done bool, err error) {
	if !w.bfCatchUp {
		// Starting a new catch-up: reset the segment iterator to re-list.
		w.resetBackfillIter()
		w.bfCatchUp = true
	}
	before := len(w.buf)
	if berr := w.stepBackfill(Filter{AfterSeq: w.maxSeq}, true); berr != nil {
		return false, berr
	}
	if len(w.buf) > before {
		return false, nil // produced a batch; keep draining
	}
	if w.backfillReaderOpen() || w.bfIdx < len(w.bfSources) {
		return false, nil // segments still pending
	}

	// The frozen list drained. Re-list against the advanced cursor to pick up a
	// rotation that landed mid-drain (seq-bounded, so it is robust to an inode
	// the later rotation reused). Only when nothing archived remains beyond the
	// cursor is the active file the next contiguous source.
	more, lerr := listBackfillSources(filepath.Dir(w.path), w.maxSeq)
	if lerr != nil {
		return false, lerr
	}
	if len(more) > 0 {
		w.bfSources = more
		w.bfIdx = 0
		w.bfListed = true
		return false, nil // another catch-up round for the mid-drain rotation
	}
	w.bfCatchUp = false
	w.resetBackfillIter()
	return true, nil
}

// withAfterSeq returns filter with AfterSeq raised to at least floor.
func withAfterSeq(filter Filter, floor uint64) Filter {
	if filter.AfterSeq < floor {
		filter.AfterSeq = floor
	}
	return filter
}

// finishResume ends the W1 resume: closes the captured fd (once) and positions
// the incremental tail at the captured active size so it re-reads only bytes
// appended after Watch.
func (w *fileWatcher) finishResume() {
	w.needsBackfill = false
	w.bfActive = false
	w.closeFd()
	if w.offset < w.activeSize {
		w.offset = w.activeSize
	}
	w.resetBackfillIter()
}

// resetBackfillIter clears the segment-iterator state, closing any open reader.
// It runs on the Next consumer and must not hold bfMu (releaseBackfillReader
// takes it); bfSources/bfIdx/bfListed are Next-owned and need no lock.
func (w *fileWatcher) resetBackfillIter() {
	w.releaseBackfillReader()
	w.bfSources = nil
	w.bfIdx = 0
	w.bfListed = false
}

// releaseBackfillReader closes the open backfill segment reader (an archive fd +
// gzip.Reader, or a no-op wrapper over the caller-owned active fd) under bfMu and
// clears the pointer. It is the single close site shared by the Next consumer and
// Close, so whichever runs first releases the descriptor exactly once.
func (w *fileWatcher) releaseBackfillReader() {
	w.bfMu.Lock()
	if w.bfReader != nil {
		w.bfReader.close()
		w.bfReader = nil
	}
	w.bfMu.Unlock()
}

// backfillReaderOpen reports whether a segment reader is currently open, under
// bfMu so the Next consumer's read cannot race a concurrent Close's clear.
func (w *fileWatcher) backfillReaderOpen() bool {
	w.bfMu.Lock()
	defer w.bfMu.Unlock()
	return w.bfReader != nil
}

func isCancelErr(err error) bool {
	return errors.Is(err, errWatcherClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Close unblocks any pending Next call and releases the watcher's descriptors:
// the captured active fd (closeFd, closed exactly once whether Close or
// finishResume runs first) and any open backfill segment reader. The active fd
// pointer is never niled, so it cannot race the Next goroutine's reads;
// releaseBackfillReader takes bfMu so a consumer that abandons the watcher
// mid-backfill (e.g. an SSE client disconnecting after a partial archive batch,
// which reaches Close on a different goroutine than Next) does not orphan the
// archive fd + gzip.Reader until GC.
func (w *fileWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		w.closeFd()
		w.releaseBackfillReader()
	})
	return nil
}
