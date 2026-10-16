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

// turnErrClass is the class of a failed turn read, which is what a consumer
// acts on: wait while unavailable or initializing, downgrade on unsupported,
// drop the session when it is not found.
type turnErrClass int

const (
	turnErrNone turnErrClass = iota
	turnErrOther
	turnErrUnavailable
	turnErrInitializing
	turnErrUnsupported
	turnErrNotFound
)

// turnErrorClass classifies err. An error that wraps several sentinels takes
// the most final: not found, then unsupported, then initializing, then
// unavailable.
func turnErrorClass(err error) turnErrClass {
	switch {
	case err == nil:
		return turnErrNone
	case errors.Is(err, runtime.ErrSessionNotFound):
		return turnErrNotFound
	case errors.Is(err, runtime.ErrTurnIdentityUnsupported):
		return turnErrUnsupported
	case errors.Is(err, runtime.ErrSessionInitializing):
		return turnErrInitializing
	case errors.Is(err, runtime.ErrRuntimeUnavailable):
		return turnErrUnavailable
	default:
		return turnErrOther
	}
}

// runTurnWatch reads, publishes what changed and waits for a hint or the
// backstop poll, until ctx ends. The first successful read is always sent,
// as is the first after a failure. A failed read is sent as an Err update
// when the failure streak begins and whenever its error changes class, never
// as an idle history.
func runTurnWatch(ctx context.Context, out chan TurnUpdate, loop turnWatchLoop) {
	defer close(out)
	if loop.stop != nil {
		defer loop.stop()
	}
	var last turnMark
	sent, failing := false, turnErrNone
	why := turnWakeStart
	for {
		hist, err := loop.read(ctx, why)
		if ctx.Err() != nil {
			return
		}
		switch class := turnErrorClass(err); {
		case err != nil:
			if class != failing {
				publishTurnUpdate(out, TurnUpdate{Err: err})
			}
			failing = class
		case !sent || failing != turnErrNone || markOf(hist) != last:
			publishTurnUpdate(out, TurnUpdate{History: hist})
			last, sent, failing = markOf(hist), true, turnErrNone
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
	plan, err := h.planTurns()
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
		return startTurnWatch(ctx, protocolTurnWatch(timing, read, events, plan.info.SessionName)), nil
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
// rescans after rotation or truncation. Until the conversation's transcript
// resolves (no key yet, not written yet) every read is an Err update and the
// watcher retries on the 5 s poll. The backstop poll, and only it, re-resolves
// the transcript from the session as it stands, so a session that moved to a
// new conversation's file is followed there; a resolution that fails drops
// the file it was following, which may be the previous conversation's.
func (h *SessionHandle) transcriptTurnWatch(plan turnPlan, timing turnTiming) turnWatchLoop {
	wake := make(chan struct{}, 1)
	var (
		follower    *sessionlog.TurnFollower
		notify      *transcriptNotifier
		lastProbe   time.Time
		lastResolve time.Time
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
	resolve := func() error {
		lastResolve = timing.now()
		info, err := h.turnSession(plan.info.ID)
		if err != nil {
			return err
		}
		path, err := h.conversationTranscript(info, plan.provider)
		if err != nil {
			return err
		}
		plan.info = info
		if follower != nil && path == plan.path {
			return nil
		}
		if err := follow(path); err != nil {
			return transcriptTurnsUnavailable(plan, err)
		}
		return nil
	}
	loop := turnWatchLoop{
		timing: timing,
		hints:  wake,
		stop:   stopNotify,
	}
	loop.read = func(ctx context.Context, why turnWake) (runtime.TurnHistory, error) {
		backstop := why == turnWakePoll && timing.now().Sub(lastResolve) >= turnPollOpen
		if follower == nil || backstop {
			if err := resolve(); err != nil {
				stopNotify()
				follower = nil
				return runtime.TurnHistory{}, err
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
		// Each watch holds its own inotify instance; a host out of them
		// (fs.inotify.max_user_instances) leaves every new watch on the poll.
		slog.Warn("worker: transcript turn watch falls back to polling", slog.String("path", path), slog.Any("err", err))
		return nil
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		w.Close() //nolint:errcheck // nothing was watched
		slog.Warn("worker: transcript turn watch falls back to polling", slog.String("path", path), slog.Any("err", err))
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
