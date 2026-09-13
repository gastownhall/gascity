package api

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// logFileWatcher wraps fsnotify for watching a session log file.
// On creation it tries to set up inotify; if that fails, or if the
// watched file is renamed/removed (log rotation), it falls back to
// polling at outputStreamPollInterval. Active fsnotify watches also keep
// a low-frequency poll as a safety net for missed write events.
type logFileWatcher struct {
	watcher      *fsnotify.Watcher
	fallbackPoll *time.Ticker
	logPath      string
	// onReset is called when the watcher switches to polling due to
	// file rename/remove. Callers should reset their cached file state
	// (size, cursor) so the next read doesn't skip the new file.
	onReset func()
}

// newLogFileWatcher creates a watcher for logPath. If fsnotify is
// unavailable or the file cannot be watched, it falls back to polling.
func newLogFileWatcher(logPath string) *logFileWatcher {
	lw := &logFileWatcher{logPath: logPath}
	lw.watchPath(logPath, false)
	return lw
}

// Close releases watcher or ticker resources.
func (lw *logFileWatcher) Close() {
	if lw.watcher != nil {
		lw.watcher.Close() //nolint:errcheck
	}
	if lw.fallbackPoll != nil {
		lw.fallbackPoll.Stop()
	}
}

// switchToPolling closes the fsnotify watcher and starts polling instead.
// Calls onReset if set so callers can invalidate cached file state.
func (lw *logFileWatcher) switchToPolling(reason string) {
	if lw.watcher != nil {
		lw.watcher.Close() //nolint:errcheck
		lw.watcher = nil
	}
	if lw.fallbackPoll == nil {
		lw.fallbackPoll = time.NewTicker(outputStreamPollInterval)
		log.Printf("session stream: %s for %s, switching to polling", reason, lw.logPath)
	}
	if lw.onReset != nil {
		lw.onReset()
	}
}

func (lw *logFileWatcher) watchPath(path string, reset bool) {
	path = strings.TrimSpace(path)
	lw.logPath = path
	if lw.watcher != nil {
		lw.watcher.Close() //nolint:errcheck
		lw.watcher = nil
	}
	if lw.fallbackPoll != nil {
		lw.fallbackPoll.Stop()
		lw.fallbackPoll = nil
	}
	if path == "" {
		if reset && lw.onReset != nil {
			lw.onReset()
		}
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		lw.fallbackPoll = time.NewTicker(outputStreamPollInterval)
		log.Printf("session stream: fsnotify unavailable for %s, falling back to polling", path)
		if reset && lw.onReset != nil {
			lw.onReset()
		}
		return
	}
	if addErr := watcher.Add(path); addErr != nil {
		_ = watcher.Close()
		lw.fallbackPoll = time.NewTicker(outputStreamPollInterval)
		log.Printf("session stream: fsnotify watch failed for %s, falling back to polling", path)
		if reset && lw.onReset != nil {
			lw.onReset()
		}
		return
	}
	lw.watcher = watcher
	if reset && lw.onReset != nil {
		lw.onReset()
	}
}

// UpdatePath retargets the watcher to a new transcript path when providers
// rotate logs across restarts but keep the old file on disk.
func (lw *logFileWatcher) UpdatePath(path string) {
	path = strings.TrimSpace(path)
	if path == "" || path == lw.logPath {
		return
	}
	lw.watchPath(path, true)
}

// RunOpts configures optional callbacks for the Run loop.
type RunOpts struct {
	// OnStall is called when the log file hasn't grown for StallTimeout.
	// After the first stall fires, it re-fires every StallTimeout until
	// readAndEmit produces new data (which resets the timer).
	// Used to detect stuck sessions (e.g., waiting for tool approval).
	OnStall      func()
	StallTimeout time.Duration // defaults to 5s
	// Wake triggers an immediate readAndEmit outside file-write or poll ticks.
	// Used to fold external signals like worker operation events into the same
	// stream loop without adding another ticker.
	Wake <-chan struct{}
}

// Run executes the main event loop. It calls readAndEmit on file changes
// and writeKeepalive on keepalive ticks. Blocks until ctx is canceled.
func (lw *logFileWatcher) Run(ctx context.Context, readAndEmit func() bool, writeKeepalive func(), opts ...RunOpts) {
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()
	watcherPoll := time.NewTicker(outputStreamPollInterval)
	defer watcherPoll.Stop()

	// Stall detection: fires when no data arrives for stallTimeout,
	// then repeats every stallTimeout until data resumes.
	var stallC <-chan time.Time
	var onStall func()
	stallTimeout := 5 * time.Second
	var wake <-chan struct{}
	if len(opts) > 0 && opts[0].OnStall != nil {
		onStall = opts[0].OnStall
		if opts[0].StallTimeout > 0 {
			stallTimeout = opts[0].StallTimeout
		}
	}
	if len(opts) > 0 {
		wake = opts[0].Wake
	}
	stallTicker := time.NewTicker(stallTimeout)
	stallTicker.Stop() // start stopped — armed after first data
	defer stallTicker.Stop()
	if onStall != nil {
		// Arm after initial emit (below) by letting the first tick start
		// the stall countdown.
		stallC = stallTicker.C
	}

	dataArrived := func() {
		// Reset the stall ticker so next fire is stallTimeout from now.
		stallTicker.Reset(stallTimeout)
	}

	// Emit initial state immediately.
	_ = readAndEmit()
	if onStall != nil {
		stallTicker.Reset(stallTimeout)
		stallC = stallTicker.C
	}

	for {
		if lw.watcher != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-lw.watcher.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Write) {
					if readAndEmit() {
						dataArrived()
					}
				}
				if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
					lw.switchToPolling("file removed/renamed")
					if readAndEmit() {
						dataArrived()
					}
				}
			case err, ok := <-lw.watcher.Errors:
				if !ok {
					return
				}
				lw.switchToPolling("watcher error: " + err.Error())
			case _, ok := <-wake:
				if !ok {
					wake = nil
					continue
				}
				if readAndEmit() {
					dataArrived()
				}
			case <-watcherPoll.C:
				if readAndEmit() {
					dataArrived()
				}
			case <-keepalive.C:
				writeKeepalive()
			case <-stallC:
				onStall()
			}
		} else {
			select {
			case <-ctx.Done():
				return
			case <-lw.fallbackPoll.C:
				if readAndEmit() {
					dataArrived()
				}
			case _, ok := <-wake:
				if !ok {
					wake = nil
					continue
				}
				if readAndEmit() {
					dataArrived()
				}
			case <-keepalive.C:
				writeKeepalive()
			case <-stallC:
				onStall()
			}
		}
	}
}
