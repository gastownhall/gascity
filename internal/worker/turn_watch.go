package worker

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// turnWake is why a turn watcher reads again.
type turnWake int

const (
	turnWakeStart turnWake = iota // the first read
	turnWakeHint                  // a file write or a runtime turn event
	turnWakePoll                  // the backstop poll
)

// turnWatchLoop is one watcher: how it reads, what wakes it early and how
// long it waits otherwise.
type turnWatchLoop struct {
	timing turnTiming
	// read reads the source once.
	read func(ctx context.Context, why turnWake) (runtime.TurnHistory, error)
	// hints wakes the watcher early; nil when nothing does.
	hints <-chan struct{}
	// wait is the backstop delay after a read.
	wait func(hist runtime.TurnHistory, err error) time.Duration
	// stop releases the watcher's resources when it ends.
	stop func()
}

// turnMark is what a watcher compares between reads: the current turn and the
// newest ended one. ObservedAt and older turns do not count as a change.
type turnMark struct {
	current      string
	currentStart time.Time
	newest       string
	newestEnd    time.Time
	newestStatus runtime.TurnStatus
	inferred     bool
	ended        int
}

func markOf(h runtime.TurnHistory) turnMark {
	m := turnMark{current: h.Current.TurnID, currentStart: h.CurrentStart, ended: len(h.Recent)}
	if len(h.Recent) > 0 {
		n := h.Recent[0]
		m.newest, m.newestEnd, m.newestStatus, m.inferred = n.TurnID, n.EndedAt, n.Status, n.Inferred
	}
	return m
}

// startTurnWatch runs loop on its own goroutine and returns its updates.
func startTurnWatch(ctx context.Context, loop turnWatchLoop) <-chan TurnUpdate {
	out := make(chan TurnUpdate, 1)
	go runTurnWatch(ctx, out, loop)
	return out
}

// runTurnWatch reads, publishes what changed and waits for a hint or the
// backstop poll, until ctx ends. The first successful read is always sent,
// as is the first after a failure. A failed read is sent as an Err update
// once per failure streak, never as an idle history.
func runTurnWatch(ctx context.Context, out chan TurnUpdate, loop turnWatchLoop) {
	defer close(out)
	if loop.stop != nil {
		defer loop.stop()
	}
	var last turnMark
	sent, failing := false, false
	why := turnWakeStart
	for {
		hist, err := loop.read(ctx, why)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			if !failing {
				publishTurnUpdate(out, TurnUpdate{Err: err})
			}
			failing = true
		case !sent || failing || markOf(hist) != last:
			publishTurnUpdate(out, TurnUpdate{History: hist})
			last, sent, failing = markOf(hist), true, false
		}
		select {
		case <-ctx.Done():
			return
		case <-loop.hints:
			why = turnWakeHint
		case <-loop.timing.after(loop.wait(hist, err)):
			why = turnWakePoll
		}
	}
}

// publishTurnUpdate sends u, replacing an update the reader has not taken yet:
// the latest wins. Only the watcher goroutine sends, so after the drain the
// buffer has room.
func publishTurnUpdate(out chan TurnUpdate, u TurnUpdate) {
	select {
	case out <- u:
		return
	default:
	}
	select {
	case <-out:
	default:
	}
	out <- u
}

// pokeTurnWatch wakes a watcher without blocking; a pending wake already
// covers this one.
func pokeTurnWatch(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// protocolTurnWatch builds the watcher of a protocol source. The runtime's
// turn events for the session are hints, each followed by a history read,
// which also recovers dropped events and turns running at subscribe time. The
// backstop polls while a turn is open, and slowly while idle.
func protocolTurnWatch(timing turnTiming, read func(context.Context) (runtime.TurnHistory, error), events <-chan runtime.TurnEvent, sessionName string) turnWatchLoop {
	loop := turnWatchLoop{
		timing: timing,
		read: func(ctx context.Context, _ turnWake) (runtime.TurnHistory, error) {
			readCtx, cancel := context.WithTimeout(ctx, turnWatchReadBound)
			defer cancel()
			return read(readCtx)
		},
		wait: func(hist runtime.TurnHistory, err error) time.Duration {
			if err != nil || hist.Current.Active() || events == nil {
				return turnPollOpen
			}
			return turnPollIdle
		},
	}
	if events != nil {
		wake := make(chan struct{}, 1)
		loop.hints = wake
		go func() {
			for ev := range events {
				if ev.Session == sessionName {
					pokeTurnWatch(wake)
				}
			}
		}()
	}
	return loop
}

// WatchTurns watches the session's turns from its source.
func (h *SessionHandle) WatchTurns(ctx context.Context) (<-chan TurnUpdate, error) {
	plan, err := h.planTurns(ctx)
	if err != nil {
		return nil, err
	}
	timing := h.turns.clock()
	switch plan.caps.Source {
	case TurnSourceProtocol:
		events, err := h.manager.SubscribeRuntimeTurnEvents(ctx)
		if err != nil {
			events = nil // no hints: the backstop poll alone
		}
		read := func(ctx context.Context) (runtime.TurnHistory, error) { return h.protocolTurnHistory(ctx, plan) }
		return startTurnWatch(ctx, protocolTurnWatch(timing, read, events, plan.sessionName)), nil
	case TurnSourceTranscript:
		return startTurnWatch(ctx, h.transcriptTurnWatch(plan, timing)), nil
	default:
		return nil, h.noTurnSource(plan)
	}
}

// transcriptTurnWatch builds the watcher of a transcript source: fsnotify on
// the transcript plus the backstop poll, every 5 s while a turn is open and
// every 60 s while idle, with the idle overlay probing at most every 2 s while
// an open turn looks quiet. A TurnFollower folds only appended bytes and
// rescans after rotation or truncation. The poll also re-resolves the
// transcript, so a session that moved to a new file is followed there.
func (h *SessionHandle) transcriptTurnWatch(plan turnPlan, timing turnTiming) turnWatchLoop {
	wake := make(chan struct{}, 1)
	var (
		follower  *sessionlog.TurnFollower
		notify    *transcriptNotifier
		lastProbe time.Time
	)
	stopNotify := func() {
		if notify != nil {
			notify.close()
			notify = nil
		}
	}
	follow := func(path string) error {
		f, err := h.adapter.NewTurnFollower(plan.provider, path)
		if err != nil {
			return err
		}
		stopNotify()
		plan.path, follower = path, f
		if !timing.noNotify {
			notify = newTranscriptNotifier(path, wake)
		}
		return nil
	}
	loop := turnWatchLoop{
		timing: timing,
		hints:  wake,
		stop:   stopNotify,
	}
	loop.read = func(ctx context.Context, why turnWake) (runtime.TurnHistory, error) {
		if why == turnWakePoll || follower == nil {
			path, err := h.TranscriptPath(ctx)
			if err != nil && follower == nil {
				return runtime.TurnHistory{}, transcriptTurnsUnavailable(plan, err)
			}
			if err == nil && (follower == nil || path != plan.path) {
				if err := follow(path); err != nil {
					return runtime.TurnHistory{}, transcriptTurnsUnavailable(plan, err)
				}
			}
		}
		probe := timing.now().Sub(lastProbe) >= turnOverlayEvery
		hist, err := h.transcriptTurnHistory(ctx, plan, follower.Read, probe)
		if probe && err == nil && plan.caps.End == runtime.TurnEndInferred {
			lastProbe = timing.now()
		}
		return hist, err
	}
	loop.wait = func(hist runtime.TurnHistory, err error) time.Duration {
		switch {
		case err == nil && hist.Current.Active() && plan.caps.End == runtime.TurnEndInferred:
			return turnOverlayEvery
		case err != nil || hist.Current.Active():
			return turnPollOpen
		default:
			return turnPollIdle
		}
	}
	return loop
}

// transcriptNotifier wakes a watcher when its transcript is written, created,
// renamed or removed. It watches the transcript's directory, so a file
// replaced by rotation keeps waking it.
type transcriptNotifier struct {
	watcher *fsnotify.Watcher
}

// newTranscriptNotifier watches path, or returns nil when fsnotify is
// unavailable; the backstop poll then carries the watcher alone.
func newTranscriptNotifier(path string, wake chan<- struct{}) *transcriptNotifier {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Debug("worker: transcript turn watch falls back to polling", slog.String("path", path), slog.Any("err", err))
		return nil
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		w.Close() //nolint:errcheck // nothing was watched
		slog.Debug("worker: transcript turn watch falls back to polling", slog.String("path", path), slog.Any("err", err))
		return nil
	}
	target := filepath.Clean(path)
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) == target {
					pokeTurnWatch(wake)
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				if !errors.Is(err, fsnotify.ErrEventOverflow) {
					slog.Debug("worker: transcript turn watch error", slog.String("path", path), slog.Any("err", err))
				}
				pokeTurnWatch(wake) // an overflow may have dropped this file's event
			}
		}
	}()
	return &transcriptNotifier{watcher: w}
}

func (n *transcriptNotifier) close() {
	n.watcher.Close() //nolint:errcheck // releasing the watch
}
