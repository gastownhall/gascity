package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Event-log mirror defaults. See MirrorOptions.
const (
	// DefaultMirrorMode is the mode a mirror file is given when
	// MirrorOptions.Mode is zero: owner read-write, group read, nothing for
	// other users, so a log shipper in the directory's group can read it.
	DefaultMirrorMode os.FileMode = 0o640

	defaultMirrorRetryInterval = 10 * time.Second
	defaultMirrorLogInterval   = time.Minute
	defaultMirrorStopTimeout   = 2 * time.Second

	// mirrorTailWindow is the first tail window lastSeqInHandle reads when it
	// looks for the last complete line of a mirror file; it doubles from
	// there.
	mirrorTailWindow = 64 * 1024
	// mirrorTailMax caps how far back lastSeqInHandle reads. A mirror whose
	// last complete line is further back than this is treated as holding no
	// seq, and the copier resumes at the canonical log's tail.
	mirrorTailMax = 16 * 1024 * 1024

	// mirrorOpenFlags open the mirror without following a symbolic link at
	// its path and without blocking on a FIFO or device there (openFile then
	// refuses anything but a regular file), and for reading too, so the
	// copier reads the mirror's tail through the handle it appends with.
	mirrorOpenFlags = os.O_RDWR | os.O_APPEND | os.O_CREATE | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
)

// MirrorOptions configures a mirror of the event log: a second JSONL file,
// outside the city's .gc directory, that receives a copy of every event line
// the canonical log receives. The canonical log stays authoritative and
// unchanged; every gc reader keeps reading it. The mirror exists so a log
// shipper that may not enter .gc can still read the events.
type MirrorOptions struct {
	// Path is the absolute path of the mirror file, outside the canonical
	// log's directory. Its parent directory is provisioned by the operator:
	// the mirror never creates or chmods it. The copier writes only a regular
	// file with no other name there: it does not follow a symbolic link at
	// Path, and refuses a FIFO, a device, and a file with another hard link,
	// the canonical log among them.
	Path string
	// Mode is the permission the mirror file is created with and set to on
	// every open. Zero means DefaultMirrorMode. Group ownership follows the
	// usual file-creation rules, so a setgid parent directory hands its group
	// to the file.
	Mode os.FileMode
	// MaxSize is the size in bytes at which the mirror is renamed to
	// <Path>.1 (replacing any previous <Path>.1) and a fresh file is started,
	// so the mirror never holds more than about 2*MaxSize on disk. Zero or
	// negative disables the mirror's own rotation for an operator that rotates
	// it externally: renaming the file away (logrotate create) and truncating
	// it in place (copytruncate) both work, since the copier appends with
	// O_APPEND and reopens the path when it names a different file.
	MaxSize int64
	// RetryInterval is how long the copier waits after a failure (the
	// directory is missing, permission is denied, the disk is full) before it
	// tries again. Zero means 10s.
	RetryInterval time.Duration
	// LogInterval bounds how often a persisting failure is reported to stderr.
	// Zero means one minute.
	LogInterval time.Duration
	// StopTimeout bounds each of Close's two waits on the copier: first for
	// it to copy the events the canonical log held when Close began, then,
	// once it is canceled, for it to release the mirror file and lock. Zero
	// means 2s.
	StopTimeout time.Duration
	// Observe, when set, is called from the copier goroutine at each step it
	// completes (see MirrorProgress), so a caller can wait for the copier
	// instead of guessing how long it takes. It must not block.
	Observe func(MirrorProgress)
}

// MirrorProgressKind names a step of the mirror copier.
type MirrorProgressKind int

const (
	// MirrorWatching means the copier opened the mirror and is tailing the
	// canonical log after Seq.
	MirrorWatching MirrorProgressKind = iota + 1
	// MirrorWrote means the event with Seq was appended to the mirror.
	MirrorWrote
	// MirrorFailed means a step failed with Err, and the copier retries after
	// RetryInterval. Reported for every failure, including those stderr
	// does not show.
	MirrorFailed
	// MirrorStopped means the copier exited and released the mirror file and
	// lock. It is the copier's last step.
	MirrorStopped
)

// MirrorProgress is one step of the mirror copier, passed to
// MirrorOptions.Observe.
type MirrorProgress struct {
	Kind MirrorProgressKind
	Seq  uint64
	Err  error
}

// WithMirror makes the recorder copy every event in its log to a mirror file
// (see MirrorOptions). Only the log's rotation owner, the one long-lived
// recorder per log, should take it: the copier tails the canonical log, so it
// mirrors every writer's events, including other processes'.
//
// The copier runs in its own goroutine from NewFileRecorder until Close, which
// first lets it copy every event the canonical log holds (waiting at most
// StopTimeout), so a clean stop leaves the mirror complete. It never blocks or
// fails a Record: it reads the canonical log after the fact, and its own
// failures are reported to stderr at a bounded rate and retried. An empty
// Path leaves the recorder exactly as it is without the option.
func WithMirror(opts MirrorOptions) FileRecorderOption {
	return func(r *FileRecorder) { r.mirrorOpts = opts }
}

// eventMirror is the copier goroutine behind WithMirror. It tails the
// canonical log through a read-only provider's Watch, so it inherits the
// watcher's handling of canonical rotation (it catches up across a
// just-rotated archive rather than skipping it), and appends each event, encoded
// exactly as the recorder encodes it, to the mirror file.
//
// It resumes after the last seq the mirror file (or its .1) already holds, so
// a restart or a failure neither duplicates nor drops events, as long as the
// canonical log still retains them and the mirror's last event is still in
// the file or its .1 (an external rotation that compresses or date-stamps the
// previous file breaks that). A mirror holding no event starts after the last
// event the canonical log held when the recorder opened it: enabling the
// mirror does not replay older history, and nothing recorded since the open
// is missed, even while the mirror directory is not provisioned yet. On a
// clean stop the copier first catches up with the canonical log (see stop).
//
// Only one copier per canonical log writes the mirror: it holds an exclusive
// flock on <canonical>.mirror.lock for its lifetime, and a second copier (a
// supervisor and a standalone controller briefly both opening the city) waits
// for it instead of duplicating lines.
type eventMirror struct {
	canonical string
	opts      MirrorOptions
	// floor is the canonical log's latest seq when the recorder opened it:
	// where a mirror that holds no event starts.
	floor  uint64
	stderr io.Writer
	cancel context.CancelFunc
	done   chan struct{}

	// mu guards the copier's progress as stop reads it: its state, the last
	// seq the mirror holds while it tails, and the drain stop waits on.
	mu      sync.Mutex
	state   mirrorState
	written uint64
	drainTo uint64
	drained chan struct{} // closed when the pending drain ends; nil when none

	// Owned by the run goroutine.
	file          *os.File
	failing       bool
	lastLog       time.Time
	suppressed    int
	lastChmodWarn time.Time
}

// mirrorState is the copier's state as stop sees it.
type mirrorState int

const (
	// mirrorStarting: the copier is taking the lock or opening the mirror,
	// and has not failed yet.
	mirrorStarting mirrorState = iota
	// mirrorTailing: the copier is tailing the canonical log and is caught
	// up through written: the mirror holds every event after where it
	// started and up to written.
	mirrorTailing
	// mirrorFailed: the copier's last step failed and it waits to retry, so
	// it cannot catch up before stop gives up on it.
	mirrorFailed
)

// validateMirrorOptions reports why opts cannot mirror the log at canonical,
// or nil when it can.
func validateMirrorOptions(canonical string, opts MirrorOptions) error {
	if !filepath.IsAbs(opts.Path) {
		return fmt.Errorf("path %q is not absolute", opts.Path)
	}
	canonDir := filepath.Clean(filepath.Dir(canonical))
	if insideDir(canonDir, filepath.Clean(opts.Path)) {
		return fmt.Errorf("path %q is inside the canonical log's directory %s", opts.Path, canonDir)
	}
	if opts.Mode&^os.ModePerm != 0 {
		return fmt.Errorf("mode %#o has bits outside 0777", uint32(opts.Mode))
	}
	return nil
}

// insideDir reports whether path is dir itself or a path under it. Both are
// clean absolute paths; symbolic links are not resolved.
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func startMirror(canonical string, opts MirrorOptions, floor uint64, stderr io.Writer) *eventMirror {
	if opts.Mode == 0 {
		opts.Mode = DefaultMirrorMode
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = defaultMirrorRetryInterval
	}
	if opts.LogInterval <= 0 {
		opts.LogInterval = defaultMirrorLogInterval
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = defaultMirrorStopTimeout
	}
	opts.Path = filepath.Clean(opts.Path)
	ctx, cancel := context.WithCancel(context.Background())
	m := &eventMirror{
		canonical: canonical,
		opts:      opts,
		floor:     floor,
		stderr:    stderr,
		cancel:    cancel,
		done:      make(chan struct{}),
		state:     mirrorStarting,
	}
	go m.run(ctx)
	return m
}

// stop ends the copier. It first waits, up to StopTimeout, for the copier to
// copy every event the canonical log holds now, so a clean stop leaves the
// mirror complete; it does not wait for a copier that is failing. Then it
// cancels the copier and waits up to StopTimeout more for it to release the
// mirror file and lock. A copier still blocked after that (a write to a hung
// network filesystem) is left to exit when its system call returns.
func (m *eventMirror) stop() {
	if target, err := ReadLatestSeq(m.canonical); err != nil {
		fmt.Fprintf(m.stderr, "events: mirror %s: not catching up before the stop: %v\n", m.opts.Path, err) //nolint:errcheck // best-effort stderr
	} else {
		m.awaitDrain(target)
	}
	m.cancel()
	t := time.NewTimer(m.opts.StopTimeout)
	defer t.Stop()
	select {
	case <-m.done:
	case <-t.C:
		fmt.Fprintf(m.stderr, "events: mirror %s: the copier is still blocked %s after the stop; not waiting for it\n", m.opts.Path, m.opts.StopTimeout) //nolint:errcheck // best-effort stderr
	}
}

// awaitDrain waits, up to StopTimeout, until the mirror holds target or the
// copier can no longer get there before the stop (it failed, or it exited).
func (m *eventMirror) awaitDrain(target uint64) {
	m.mu.Lock()
	if m.drainOverLocked(target) {
		m.mu.Unlock()
		return
	}
	drained := make(chan struct{})
	m.drainTo, m.drained = target, drained
	m.mu.Unlock()

	t := time.NewTimer(m.opts.StopTimeout)
	defer t.Stop()
	select {
	case <-drained:
	case <-m.done:
	case <-t.C:
		fmt.Fprintf(m.stderr, "events: mirror %s: stopping before it copied seq %d; the next start copies the rest\n", m.opts.Path, target) //nolint:errcheck // best-effort stderr
	}
}

// drainOverLocked reports whether a drain to target is over: the mirror holds
// target, or the copier failed. The caller holds mu.
func (m *eventMirror) drainOverLocked(target uint64) bool {
	return m.state == mirrorFailed || (m.state == mirrorTailing && m.written >= target)
}

// step records one copier step: it updates the progress stop reads, ends a
// pending drain that the step completes, and reports the step to Observe.
func (m *eventMirror) step(p MirrorProgress) {
	m.mu.Lock()
	switch p.Kind {
	case MirrorWatching, MirrorWrote:
		// A copier watching after Seq is caught up through it: the mirror
		// holds the events up to its resume point, or the copier was never
		// to copy them. A drain still waits for a rotated window past that
		// point, since its target counts in-flight rotating files.
		m.state, m.written = mirrorTailing, p.Seq
	case MirrorFailed:
		m.state = mirrorFailed
	}
	if m.drained != nil && m.drainOverLocked(m.drainTo) {
		close(m.drained)
		m.drained = nil
	}
	m.mu.Unlock()
	if m.opts.Observe != nil {
		m.opts.Observe(p)
	}
}

func (m *eventMirror) run(ctx context.Context) {
	defer close(m.done)
	defer m.step(MirrorProgress{Kind: MirrorStopped})
	unlock, ok := m.acquireLock(ctx)
	if !ok {
		return
	}
	defer unlock()
	defer m.closeFile()
	for {
		err := m.copyOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		m.reportFailure(err)
		m.closeFile()
		if !sleepCtx(ctx, m.opts.RetryInterval) {
			return
		}
	}
}

// acquireLock takes the exclusive flock that makes this the canonical log's
// only copier, waiting for another copier to release it. It reports false when
// ctx ends first.
func (m *eventMirror) acquireLock(ctx context.Context) (func(), bool) {
	for {
		lock, err := tryLockFile(m.canonical + ".mirror.lock")
		if err == nil {
			if m.failing {
				// The wait was reported; say that it ended.
				fmt.Fprintf(m.stderr, "events: mirror %s: took the mirror lock; this process mirrors the event log now\n", m.opts.Path) //nolint:errcheck // best-effort stderr
				m.failing = false
				m.suppressed = 0
			}
			return func() {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				_ = lock.Close()
			}, true
		}
		m.reportFailure(fmt.Errorf("waiting for the mirror lock: %w", err))
		if !sleepCtx(ctx, m.opts.RetryInterval) {
			return nil, false
		}
	}
}

func tryLockFile(path string) (*os.File, error) {
	lock, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("another process is mirroring this event log")
		}
		return nil, err
	}
	return lock, nil
}

// copyOnce opens the mirror, resumes after the last event it holds, and
// copies events from the canonical log until a write fails or ctx ends.
func (m *eventMirror) copyOnce(ctx context.Context) error {
	if err := m.openFile(); err != nil {
		return err
	}
	after, err := m.resumeSeq()
	if err != nil {
		return err
	}
	w, err := NewReadOnlyFileProvider(m.canonical, m.stderr).Watch(ctx, after)
	if err != nil {
		return fmt.Errorf("watching %s: %w", m.canonical, err)
	}
	defer w.Close() //nolint:errcheck // fileWatcher.Close never fails
	m.step(MirrorProgress{Kind: MirrorWatching, Seq: after})
	for {
		e, err := w.Next()
		if err != nil {
			return fmt.Errorf("reading %s: %w", m.canonical, err)
		}
		if err := m.write(e); err != nil {
			return err
		}
		// Only a write proves the mirror works again: a full disk or a
		// failing rotation lets the open and the watch succeed.
		m.reportRecovered()
		m.step(MirrorProgress{Kind: MirrorWrote, Seq: e.Seq})
	}
}

// resumeSeq is the seq the copier continues after: the last one the mirror
// (or, right after a rotation, its .1) holds, else the floor captured when the
// recorder opened the log. Either is capped at the canonical log's latest seq,
// so a canonical log that was reset (its seqs restarted) is followed from its
// tail.
func (m *eventMirror) resumeSeq() (uint64, error) {
	canonLatest, err := ReadLatestSeq(m.canonical)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", m.canonical, err)
	}
	last, err := lastSeqInHandle(m.file)
	if err != nil {
		return 0, fmt.Errorf("reading the last seq of %s: %w", m.opts.Path, err)
	}
	if last == 0 {
		if last, err = lastSeqInFile(m.opts.Path + ".1"); err != nil {
			return 0, err
		}
	}
	if last == 0 {
		last = m.floor
	}
	if last > canonLatest {
		return canonLatest, nil
	}
	return last, nil
}

// openFile opens the mirror for appending, creating it with the configured
// mode, and sets that mode (creation is subject to the umask). It never
// creates the parent directory, and never follows a symbolic link at the
// path: whoever can write the mirror's directory must not be able to point gc
// at another file to chmod and append to. It refuses anything but a regular
// file with no other name (see checkOpened). A torn last line (a write cut
// short) is closed with a newline so the next event starts on its own line.
func (m *eventMirror) openFile() error {
	f, err := os.OpenFile(m.opts.Path, mirrorOpenFlags, m.opts.Mode)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("opening: %w (gc writes the mirror only as a regular file, never through a symbolic link)", err)
		}
		return fmt.Errorf("opening: %w", err)
	}
	info, err := m.checkOpened(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	if info.Mode().Perm() != m.opts.Mode {
		// Best effort: a file another user owns (pre-created by the operator)
		// keeps its mode, and the events still flow.
		if err := f.Chmod(m.opts.Mode); err != nil {
			m.warnChmod(err)
		}
	}
	if info.Size() > 0 {
		torn, err := endsWithoutNewline(f, info.Size())
		if err != nil {
			_ = f.Close()
			return err
		}
		if torn {
			if err := writeBatch(f, []byte{'\n'}); err != nil {
				_ = f.Close()
				return fmt.Errorf("terminating a torn line: %w", err)
			}
		}
	}
	m.file = f
	return nil
}

// checkOpened returns the info of f, the just-opened mirror, when gc may
// write it: a regular file with no other name. O_NOFOLLOW does not refuse a
// hard link, so whoever can write the mirror's directory could otherwise link
// the canonical log, or any other file of the city's user, at the mirror path
// for gc to chmod and append to. It never removes what it refuses.
func (m *eventMirror) checkOpened(f *os.File) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (mode %v)", info.Mode())
	}
	canon, err := os.Stat(m.canonical)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", m.canonical, err)
	}
	if err == nil && os.SameFile(info, canon) {
		return nil, fmt.Errorf("the path names the canonical event log %s itself", m.canonical)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return nil, fmt.Errorf("the file has %d hard links; gc writes the mirror only as a file with no other name", st.Nlink)
	}
	return info, nil
}

// warnChmod reports a failed chmod of the mirror at most once per
// LogInterval: the copier reopens the mirror at every retry and rotation, and
// a file another user owns fails the chmod each time.
func (m *eventMirror) warnChmod(err error) {
	now := time.Now()
	if !m.lastChmodWarn.IsZero() && now.Sub(m.lastChmodWarn) < m.opts.LogInterval {
		return
	}
	m.lastChmodWarn = now
	fmt.Fprintf(m.stderr, "events: mirror %s: chmod %#o: %v\n", m.opts.Path, uint32(m.opts.Mode), err) //nolint:errcheck // best-effort stderr
}

// endsWithoutNewline reports whether the size bytes of f end without a
// newline: the last line was cut short.
func endsWithoutNewline(f *os.File, size int64) (bool, error) {
	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return false, fmt.Errorf("reading the last byte: %w", err)
	}
	return b[0] != '\n', nil
}

func (m *eventMirror) closeFile() {
	if m.file != nil {
		_ = m.file.Close()
		m.file = nil
	}
}

// write appends one event line to the mirror. Before it writes it reopens the
// path if it no longer names the open file (an external rotation renamed or
// removed it) and rotates the mirror if this line would take it past MaxSize.
func (m *eventMirror) write(e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event %d: %w", e.Seq, err)
	}
	data = append(data, '\n')
	if err := m.reopenIfReplaced(); err != nil {
		return err
	}
	if err := m.maybeRotate(int64(len(data))); err != nil {
		return err
	}
	if err := writeBatch(m.file, data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func (m *eventMirror) reopenIfReplaced() error {
	held, err := m.file.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	current, err := os.Lstat(m.opts.Path)
	if err == nil && os.SameFile(held, current) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat: %w", err)
	}
	m.closeFile()
	return m.openFile()
}

func (m *eventMirror) maybeRotate(next int64) error {
	if m.opts.MaxSize <= 0 {
		return nil
	}
	info, err := m.file.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() == 0 || info.Size()+next <= m.opts.MaxSize {
		return nil
	}
	m.closeFile()
	if err := os.Rename(m.opts.Path, m.opts.Path+".1"); err != nil {
		return fmt.Errorf("rotating: %w", err)
	}
	return m.openFile()
}

// reportFailure writes err to stderr, at most once per LogInterval while
// failures persist; the next report counts the ones it held back.
func (m *eventMirror) reportFailure(err error) {
	if err == nil {
		return
	}
	m.step(MirrorProgress{Kind: MirrorFailed, Err: err})
	now := time.Now()
	if m.failing && now.Sub(m.lastLog) < m.opts.LogInterval {
		m.suppressed++
		return
	}
	msg := fmt.Sprintf("events: mirror %s: %v; retrying every %s", m.opts.Path, err, m.opts.RetryInterval)
	if m.suppressed > 0 {
		msg += fmt.Sprintf(" (%d similar failures not shown)", m.suppressed)
	}
	fmt.Fprintln(m.stderr, msg) //nolint:errcheck // best-effort stderr
	m.failing = true
	m.lastLog = now
	m.suppressed = 0
}

// reportRecovered notes, once, that the mirror is working again after a
// reported failure.
func (m *eventMirror) reportRecovered() {
	if !m.failing {
		return
	}
	fmt.Fprintf(m.stderr, "events: mirror %s: recovered\n", m.opts.Path) //nolint:errcheck // best-effort stderr
	m.failing = false
	m.suppressed = 0
}

// lastSeqInFile returns the seq of the last complete, parseable line in the
// regular file at path (see lastSeqInHandle), or 0 when it is absent. Like the
// mirror itself, it does not follow a symbolic link at path.
func lastSeqInFile(path string) (uint64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading the last seq of %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("reading the last seq of %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("reading the last seq of %s: not a regular file (mode %v)", path, info.Mode())
	}
	seq, err := lastSeqInHandle(f)
	if err != nil {
		return 0, fmt.Errorf("reading the last seq of %s: %w", path, err)
	}
	return seq, nil
}

// lastSeqInHandle returns the seq of the last complete, parseable line in f,
// or 0 when f is empty or holds none within mirrorTailMax of its end.
func lastSeqInHandle(f *os.File) (uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	for window := int64(mirrorTailWindow); ; window *= 2 {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, size-window); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		lines := bytes.Split(buf, []byte{'\n'})
		// The final element follows the last newline: empty, or a torn line.
		// The first is a partial line unless the window reaches the start.
		first := 1
		if window == size {
			first = 0
		}
		for i := len(lines) - 2; i >= first; i-- {
			var probe struct {
				Seq uint64 `json:"seq"`
			}
			if json.Unmarshal(lines[i], &probe) == nil && probe.Seq > 0 {
				return probe.Seq, nil
			}
		}
		if window == size || window >= mirrorTailMax {
			return 0, nil
		}
	}
}

// sleepCtx waits d and reports true, or false as soon as ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
