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

	// mirrorTailWindow is the first tail window lastSeqInFile reads when it
	// looks for the last complete line of a mirror file; it doubles from there.
	mirrorTailWindow = 64 * 1024
	// mirrorTailMax caps how far back lastSeqInFile reads. A mirror whose last
	// complete line is further back than this is treated as holding no seq,
	// and the copier resumes at the canonical log's tail.
	mirrorTailMax = 16 * 1024 * 1024
)

// MirrorOptions configures a mirror of the event log: a second JSONL file,
// outside the city's .gc directory, that receives a copy of every event line
// the canonical log receives. The canonical log stays authoritative and
// unchanged; every gc reader keeps reading it. The mirror exists so a log
// shipper that may not enter .gc can still read the events.
type MirrorOptions struct {
	// Path is the absolute path of the mirror file. Its parent directory is
	// provisioned by the operator: the mirror never creates or chmods it.
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
// The copier runs in its own goroutine from NewFileRecorder until Close. It
// never blocks or fails a Record: it reads the canonical log after the fact,
// and its own failures are reported to stderr at a bounded rate and retried.
// An empty Path leaves the recorder exactly as it is without the option.
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
// canonical log still retains them. A mirror holding no event starts after the
// last event the canonical log held when the recorder opened it: enabling the
// mirror does not replay older history, and nothing recorded since the open
// is missed, even while the mirror directory is not provisioned yet.
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

	// Owned by the run goroutine.
	file       *os.File
	failing    bool
	lastLog    time.Time
	suppressed int
}

// validateMirrorOptions reports why opts cannot mirror the log at canonical,
// or nil when it can.
func validateMirrorOptions(canonical string, opts MirrorOptions) error {
	if !filepath.IsAbs(opts.Path) {
		return fmt.Errorf("path %q is not absolute", opts.Path)
	}
	mirror := filepath.Clean(opts.Path)
	canonDir := filepath.Clean(filepath.Dir(canonical))
	if filepath.Dir(mirror) == canonDir {
		return fmt.Errorf("path %q is in the canonical log's directory %s", opts.Path, canonDir)
	}
	if opts.Mode&^os.ModePerm != 0 {
		return fmt.Errorf("mode %#o has bits outside 0777", uint32(opts.Mode))
	}
	return nil
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
	opts.Path = filepath.Clean(opts.Path)
	ctx, cancel := context.WithCancel(context.Background())
	m := &eventMirror{
		canonical: canonical,
		opts:      opts,
		floor:     floor,
		stderr:    stderr,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	go m.run(ctx)
	return m
}

// stop ends the copier and waits for it to release the mirror file and lock.
func (m *eventMirror) stop() {
	m.cancel()
	<-m.done
}

func (m *eventMirror) run(ctx context.Context) {
	defer close(m.done)
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
	m.reportRecovered()
	m.observe(MirrorProgress{Kind: MirrorWatching, Seq: after})
	for {
		e, err := w.Next()
		if err != nil {
			return fmt.Errorf("reading %s: %w", m.canonical, err)
		}
		if err := m.write(e); err != nil {
			return err
		}
		m.reportRecovered()
		m.observe(MirrorProgress{Kind: MirrorWrote, Seq: e.Seq})
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
	last, err := lastSeqInFile(m.opts.Path)
	if err != nil {
		return 0, err
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
// creates the parent directory. A torn last line (a write cut short) is closed
// with a newline so the next event starts on its own line.
func (m *eventMirror) openFile() error {
	f, err := os.OpenFile(m.opts.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, m.opts.Mode)
	if err != nil {
		return fmt.Errorf("opening: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat: %w", err)
	}
	if info.Mode().Perm() != m.opts.Mode {
		// Best effort: a file another user owns (pre-created by the operator)
		// keeps its mode, and the events still flow.
		if err := f.Chmod(m.opts.Mode); err != nil {
			fmt.Fprintf(m.stderr, "events: mirror %s: chmod %#o: %v\n", m.opts.Path, uint32(m.opts.Mode), err) //nolint:errcheck // best-effort stderr
		}
	}
	if info.Size() > 0 {
		torn, err := endsWithoutNewline(m.opts.Path, info.Size())
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

func endsWithoutNewline(path string, size int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("reading the last byte: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
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
	current, err := os.Stat(m.opts.Path)
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
	m.observe(MirrorProgress{Kind: MirrorFailed, Err: err})
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

func (m *eventMirror) observe(p MirrorProgress) {
	if m.opts.Observe != nil {
		m.opts.Observe(p)
	}
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

// lastSeqInFile returns the seq of the last complete, parseable line in path,
// or 0 when the file is absent, empty, or holds none within mirrorTailMax of
// its end.
func lastSeqInFile(path string) (uint64, error) {
	f, err := os.Open(path)
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
	size := info.Size()
	for window := int64(mirrorTailWindow); ; window *= 2 {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, size-window); err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("reading the last seq of %s: %w", path, err)
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
