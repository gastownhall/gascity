package api

import (
	"context"
	"log"
	"time"

	"github.com/fsnotify/fsnotify"
)

// logFileWatcher wraps fsnotify for watching a session log file.
// On creation it tries to set up inotify; if that fails, or if the
// watched file is renamed/removed (log rotation), it falls back to
// polling at outputStreamPollInterval.
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

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		lw.fallbackPoll = time.NewTicker(outputStreamPollInterval)
		log.Printf("session stream: fsnotify unavailable for %s, falling back to polling", logPath)
		return lw
	}
	if addErr := watcher.Add(logPath); addErr != nil {
		_ = watcher.Close()
		lw.fallbackPoll = time.NewTicker(outputStreamPollInterval)
		log.Printf("session stream: fsnotify watch failed for %s, falling back to polling", logPath)
		return lw
	}
	lw.watcher = watcher
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

// RunOpts configures optional callbacks for the Run loop.
type RunOpts struct {
	// OnStall is called when the log file hasn't grown for StallTimeout.
	// After the first stall fires, it re-fires every StallTimeout until
	// readAndEmit produces new data (which resets the timer).
	// Used to detect stuck sessions (e.g., waiting for tool approval).
	OnStall      func()
	StallTimeout time.Duration // defaults to 5s
}

// Run executes the main event loop. It calls readAndEmit on file changes
// and writeKeepalive on keepalive ticks. Blocks until ctx is canceled.
func (lw *logFileWatcher) Run(ctx context.Context, readAndEmit func(), writeKeepalive func(), opts ...RunOpts) {
	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	// Stall detection: fires when no data arrives for stallTimeout,
	// then repeats every stallTimeout until data resumes.
	var stallC <-chan time.Time
	var onStall func()
	stallTimeout := 5 * time.Second
	if len(opts) > 0 && opts[0].OnStall != nil {
		onStall = opts[0].OnStall
		if opts[0].StallTimeout > 0 {
			stallTimeout = opts[0].StallTimeout
		}
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
	readAndEmit()
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
					readAndEmit()
					dataArrived()
				}
				if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
					lw.switchToPolling("file removed/renamed")
					readAndEmit()
				}
			case err, ok := <-lw.watcher.Errors:
				if !ok {
					return
				}
				lw.switchToPolling("watcher error: " + err.Error())
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
				readAndEmit()
				dataArrived()
			case <-keepalive.C:
				writeKeepalive()
			case <-stallC:
				onStall()
			}
		}
	}
}
