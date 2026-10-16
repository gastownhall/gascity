package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Turn identity at the worker boundary (engdocs/design/x11-turn-identity-coverage.md
// §5.2, slice TI-2b). A session's turns come from one source: the runtime's
// protocol when it observes turns itself, else the provider family's transcript
// when it records turn ids and lives on this host, else nothing. The worker
// composes that choice per session and reads turns through it. No turn source
// reads an id out of delivered text (X11 D13).

// TurnHandle exposes a worker's turn identity.
type TurnHandle interface {
	// TurnCapabilities reports what the worker can tell about its session's
	// turns, composed from the routed runtime and the transcript family.
	TurnCapabilities(context.Context) (TurnCapabilities, error)
	// CurrentTurn reads the session's current and recent turns now. A source
	// that cannot be read is an error wrapping runtime.ErrRuntimeUnavailable,
	// never an idle history; a session with no source is
	// runtime.ErrTurnIdentityUnsupported.
	CurrentTurn(context.Context) (runtime.TurnHistory, error)
	// WatchTurns sends a history first, then a history whenever the current or
	// newest ended turn changes (coalesced: latest wins), and an update with
	// Err set while its source cannot be read (never an idle history instead).
	// It closes when ctx is done. A session with no source is
	// runtime.ErrTurnIdentityUnsupported, returned at once.
	WatchTurns(context.Context) (<-chan TurnUpdate, error)
}

// TurnSource is where a session's turns are read from.
type TurnSource string

// The turn sources.
const (
	// TurnSourceProtocol reads turns from the runtime itself
	// (runtime.TurnObservationProvider).
	TurnSourceProtocol TurnSource = "protocol"
	// TurnSourceTranscript reads turns from the provider's transcript on
	// this host.
	TurnSourceTranscript TurnSource = "transcript"
	// TurnSourceNone means the session's turns cannot be identified here.
	TurnSourceNone TurnSource = "none"
)

// TurnReplace is the sequence a session's family needs to replace its running
// turn with a new message (X11 D11), read by deliver-now.
type TurnReplace string

// The turn replacement sequences.
const (
	// TurnReplaceNone means the running turn cannot be replaced.
	TurnReplaceNone TurnReplace = "none"
	// TurnReplaceInterrupt interrupts the turn, then delivers.
	TurnReplaceInterrupt TurnReplace = "interrupt"
	// TurnReplaceInterruptReset interrupts, resets the interrupted turn, then
	// delivers.
	TurnReplaceInterruptReset TurnReplace = "interrupt_reset"
	// TurnReplaceRestart restarts the agent, discarding the pending turn.
	TurnReplaceRestart TurnReplace = "restart"
)

// TurnCapabilities is what a worker can tell about its session's turns.
type TurnCapabilities struct {
	runtime.TurnCapabilities
	// Source is where the turns are read from.
	Source TurnSource
	// Interrupt is how the runtime interrupts the session's turn. Unsupported
	// forces Fence none.
	Interrupt runtime.TurnInterruptMode
	// Replace is the family's replacement sequence.
	Replace TurnReplace
	// HookTurnID names the verified hook payload field carrying the turn id;
	// empty until the X11 live arm verifies one.
	HookTurnID string
}

// TurnUpdate is one WatchTurns update: a history, or the error that kept the
// source from being read.
type TurnUpdate struct {
	History runtime.TurnHistory
	Err     error
}

const (
	// turnQuietWindow is how long an open turn's transcript must not grow
	// before the idle overlay may infer its end.
	turnQuietWindow = 2 * time.Second
	// turnOverlayEvery bounds how often the watcher probes idle while a turn
	// looks quiet.
	turnOverlayEvery = 2 * time.Second
	// turnIdleGap separates the overlay's two idle readings, so they are two
	// looks rather than one.
	turnIdleGap = 250 * time.Millisecond
	// turnPollOpen and turnPollIdle are the watcher's backstop polls while a
	// turn is open and while the session is idle.
	turnPollOpen = 5 * time.Second
	turnPollIdle = 60 * time.Second
	// turnWatchReadBound bounds each protocol read the watcher makes.
	turnWatchReadBound = 10 * time.Second
)

// noTurnCapabilities is the canonical "nothing" on every axis.
var noTurnCapabilities = runtime.TurnCapabilities{}.Normalized()

// turnTiming is the clock the turn observer runs on. Tests replace it.
type turnTiming struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	// idleGap separates the overlay's two idle readings.
	idleGap time.Duration
	// noNotify disables fsnotify, leaving the backstop poll alone.
	noNotify bool
}

var defaultTurnTiming = turnTiming{now: time.Now, after: time.After, idleGap: turnIdleGap}

// turnObservation is a SessionHandle's turn-observation state.
type turnObservation struct {
	mu sync.Mutex
	// timing overrides defaultTurnTiming in tests.
	timing *turnTiming
	// idleSnapshots memoizes, by runtime session name, whether the routed
	// runtime takes idle snapshots: a trait of the backend, read once.
	idleSnapshots map[string]bool
	// inferred is the last end the idle overlay inferred. It holds while the
	// transcript is unchanged, so repeated reads report one end.
	inferred inferredTurnEnd
}

// inferredTurnEnd is an end the idle overlay inferred for turnID while the
// transcript at path was in state info.
type inferredTurnEnd struct {
	path    string
	turnID  string
	info    os.FileInfo
	endedAt time.Time
}

func (o *turnObservation) clock() turnTiming {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timing != nil {
		return *o.timing
	}
	return defaultTurnTiming
}

// composeTurnCapabilities composes a session's turn capabilities (X11 §5.2):
// protocol when the runtime reports any capability; else the transcript when
// it resolves on this host (transcript), the family records turn starts and the
// runtime interrupts by keystroke; else none. An inferred end needs idle
// snapshots, or nobody can see it, so without them End is none. Interrupt mode
// unsupported forces Fence none.
func composeTurnCapabilities(rcaps runtime.TurnCapabilities, mode runtime.TurnInterruptMode, support sessionlog.TurnSupport, transcript, idleSnapshots bool) TurnCapabilities {
	caps := TurnCapabilities{
		TurnCapabilities: noTurnCapabilities,
		Source:           TurnSourceNone,
		Interrupt:        mode,
		Replace:          TurnReplace(support.Replace),
		HookTurnID:       support.HookTurnID,
	}
	rcaps = rcaps.Normalized()
	switch {
	case rcaps != noTurnCapabilities:
		caps.TurnCapabilities, caps.Source = rcaps, TurnSourceProtocol
	case transcript && support.Start == sessionlog.TurnStartRecorded && mode == runtime.TurnInterruptKeystroke:
		end := runtime.TurnEnd(support.End)
		if end == runtime.TurnEndInferred && !idleSnapshots {
			end = runtime.TurnEndNone
		}
		caps.TurnCapabilities = runtime.TurnCapabilities{
			Start:    runtime.TurnStartRecorded,
			End:      end,
			Fence:    runtime.TurnFenceChecked,
			Link:     runtime.TurnLink(support.Link),
			Lossless: true,
		}.Normalized()
		caps.Source = TurnSourceTranscript
	}
	if mode == runtime.TurnInterruptUnsupported {
		caps.Fence = runtime.TurnFenceNone
	}
	return caps
}

// turnPlan is one composition of a session's turn source.
type turnPlan struct {
	caps        TurnCapabilities
	sessionID   string
	sessionName string // the runtime session name
	provider    string // the transcript family's provider string
	path        string // the transcript, for a transcript source
}

// planTurns composes the session's turn source now.
func (h *SessionHandle) planTurns(ctx context.Context) (turnPlan, error) {
	id := h.currentSessionID()
	if id == "" {
		support := h.adapter.TurnSupportFor(h.session.Provider)
		return turnPlan{caps: composeTurnCapabilities(noTurnCapabilities, runtime.TurnInterruptKeystroke, support, false, false)}, nil
	}
	rcaps, mode, err := h.manager.RuntimeTurnCapabilities(id)
	if err != nil {
		return turnPlan{}, fmt.Errorf("turn capabilities of session %s: %w", id, err)
	}
	info, err := h.manager.Get(id)
	if err != nil {
		return turnPlan{}, fmt.Errorf("turn capabilities of session %s: %w", id, err)
	}
	plan := turnPlan{sessionID: id, sessionName: info.SessionName, provider: h.historyProvider(info)}
	support := h.adapter.TurnSupportFor(plan.provider)
	transcriptEligible := rcaps.Normalized() == noTurnCapabilities &&
		support.Start == sessionlog.TurnStartRecorded && mode == runtime.TurnInterruptKeystroke
	idleSnapshots := false
	if transcriptEligible {
		path, err := h.TranscriptPath(ctx)
		switch {
		case errors.Is(err, ErrHistoryUnavailable):
		case err != nil:
			return turnPlan{}, fmt.Errorf("turn capabilities of session %s: %w: %w", id, runtime.ErrRuntimeUnavailable, err)
		default:
			plan.path = path
		}
		if plan.path != "" && support.End == sessionlog.TurnEndInferred {
			if idleSnapshots, err = h.takesIdleSnapshots(id, info.SessionName); err != nil {
				return turnPlan{}, fmt.Errorf("turn capabilities of session %s: %w", id, err)
			}
		}
	}
	plan.caps = composeTurnCapabilities(rcaps, mode, support, plan.path != "", idleSnapshots)
	return plan, nil
}

// takesIdleSnapshots reports whether the runtime serving the session takes idle
// snapshots, reading it once per runtime session name.
func (h *SessionHandle) takesIdleSnapshots(id, sessionName string) (bool, error) {
	h.turns.mu.Lock()
	known, ok := h.turns.idleSnapshots[sessionName]
	h.turns.mu.Unlock()
	if ok {
		return known, nil
	}
	_, supported, err := h.manager.RuntimeIdleSnapshot(id)
	if err != nil && !supported {
		return false, err
	}
	h.turns.mu.Lock()
	if h.turns.idleSnapshots == nil {
		h.turns.idleSnapshots = map[string]bool{}
	}
	h.turns.idleSnapshots[sessionName] = supported
	h.turns.mu.Unlock()
	return supported, nil
}

// TurnCapabilities reports what the worker can tell about its session's turns.
func (h *SessionHandle) TurnCapabilities(ctx context.Context) (TurnCapabilities, error) {
	plan, err := h.planTurns(ctx)
	if err != nil {
		return TurnCapabilities{}, err
	}
	return plan.caps, nil
}

// CurrentTurn reads the session's current and recent turns from its source.
func (h *SessionHandle) CurrentTurn(ctx context.Context) (runtime.TurnHistory, error) {
	plan, err := h.planTurns(ctx)
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	switch plan.caps.Source {
	case TurnSourceProtocol:
		return h.protocolTurnHistory(ctx, plan)
	case TurnSourceTranscript:
		return h.transcriptTurnHistory(ctx, plan, func() (sessionlog.TurnScan, error) {
			return h.adapter.ScanTurns(plan.provider, plan.path)
		}, true)
	default:
		return runtime.TurnHistory{}, h.noTurnSource(plan)
	}
}

func (h *SessionHandle) noTurnSource(plan turnPlan) error {
	return fmt.Errorf("turns of session %s: %w", plan.sessionID, runtime.ErrTurnIdentityUnsupported)
}

// turnObservationScope names the city in a turn observation key, so two
// cities' sessions of one name never share an in-flight read.
func (h *SessionHandle) turnObservationScope() string {
	if city, err := h.manager.CityDir(); err == nil {
		return city.String()
	}
	return fmt.Sprintf("manager:%p", h.manager)
}

// turnObservationKey is the ObserveBounded key of a turn read of one session.
// It is a namespace apart from liveness observation (postStartObservationKey
// in cmd/gc), so a wedged turn read never blocks a liveness read, nor the
// other way round. kind separates the history read from the idle snapshot.
func turnObservationKey(kind, scope, sessionName string) string {
	return "worker.turns." + kind + "\x00" + scope + "\x00" + sessionName
}

func (h *SessionHandle) protocolTurnHistory(ctx context.Context, plan turnPlan) (runtime.TurnHistory, error) {
	key := turnObservationKey("history", h.turnObservationScope(), plan.sessionName)
	return ObserveBounded(ctx, key, func(ctx context.Context) (runtime.TurnHistory, error) {
		return h.manager.RuntimeTurnHistory(ctx, plan.sessionID)
	})
}

// transcriptTurnHistory reads the transcript's turns through read, converts
// them and, for a family whose ends are inferred, applies the idle overlay.
// probe lets the overlay take idle readings; without it the overlay only
// reuses an end it already inferred for the unchanged transcript.
func (h *SessionHandle) transcriptTurnHistory(ctx context.Context, plan turnPlan, read func() (sessionlog.TurnScan, error), probe bool) (runtime.TurnHistory, error) {
	timing := h.turns.clock()
	before, err := os.Stat(plan.path)
	if err != nil {
		return runtime.TurnHistory{}, transcriptTurnsUnavailable(plan, err)
	}
	scan, err := read()
	if err != nil {
		return runtime.TurnHistory{}, transcriptTurnsUnavailable(plan, err)
	}
	hist, err := turnHistoryFromScan(scan, timing.now())
	if err != nil {
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w", plan.sessionID, err)
	}
	if plan.caps.End == runtime.TurnEndInferred && hist.Current.Active() {
		h.applyIdleOverlay(ctx, plan, timing, &hist, before, probe)
	}
	return hist, nil
}

// transcriptTurnsUnavailable is the error of a transcript that could not be
// read for turns: unknown, never idle (X11 I8).
func transcriptTurnsUnavailable(plan turnPlan, err error) error {
	if errors.Is(err, sessionlog.ErrTurnsUnsupported) {
		return fmt.Errorf("turns of session %s: %w: %w", plan.sessionID, runtime.ErrTurnIdentityUnsupported, err)
	}
	return fmt.Errorf("turns of session %s: %w: %w", plan.sessionID, runtime.ErrRuntimeUnavailable, err)
}

// applyIdleOverlay infers the end of hist's open turn (X11 §5.2): once the
// transcript has not grown for turnQuietWindow, two idle snapshots that both
// read idle, with no transcript growth between them, end the turn with
// Inferred set at the observer's clock. A busy or failed reading, or growth,
// leaves the turn open. before is the transcript's state before the scan.
func (h *SessionHandle) applyIdleOverlay(ctx context.Context, plan turnPlan, timing turnTiming, hist *runtime.TurnHistory, before os.FileInfo, probe bool) {
	turnID := hist.Current.TurnID
	h.turns.mu.Lock()
	memo := h.turns.inferred
	h.turns.mu.Unlock()
	if memo.path == plan.path && memo.turnID == turnID && sameTranscriptState(memo.info, before) {
		endCurrentTurn(hist, memo.endedAt)
		return
	}
	if !probe || timing.now().Sub(before.ModTime()) < turnQuietWindow {
		return
	}
	if !h.readsIdle(ctx, plan) {
		return
	}
	if timing.idleGap > 0 {
		select {
		case <-ctx.Done():
			return
		case <-timing.after(timing.idleGap):
		}
	}
	if !h.readsIdle(ctx, plan) {
		return
	}
	after, err := os.Stat(plan.path)
	if err != nil || !sameTranscriptState(before, after) {
		return
	}
	endedAt := timing.now()
	h.turns.mu.Lock()
	h.turns.inferred = inferredTurnEnd{path: plan.path, turnID: turnID, info: after, endedAt: endedAt}
	h.turns.mu.Unlock()
	endCurrentTurn(hist, endedAt)
}

// readsIdle takes one idle snapshot of the session, bounded by ctx. Only a
// successful idle reading counts; busy, unsupported or failed is not idle.
func (h *SessionHandle) readsIdle(ctx context.Context, plan turnPlan) bool {
	type snapshot struct{ idle, supported bool }
	key := turnObservationKey("idle", h.turnObservationScope(), plan.sessionName)
	got, err := ObserveBounded(ctx, key, func(context.Context) (snapshot, error) {
		idle, supported, err := h.manager.RuntimeIdleSnapshot(plan.sessionID)
		return snapshot{idle, supported}, err
	})
	return err == nil && got.supported && got.idle
}

// sameTranscriptState reports whether two reads of a transcript saw the same
// file at the same size and modification time: nothing was written between.
func sameTranscriptState(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// endCurrentTurn moves hist's open turn to Recent as an inferred end.
func endCurrentTurn(hist *runtime.TurnHistory, endedAt time.Time) {
	ended := runtime.EndedTurn{
		TurnID:       hist.Current.TurnID,
		StartedAt:    hist.CurrentStart,
		EndedAt:      endedAt,
		Inferred:     true,
		DeliveryKeys: hist.CurrentKeys,
	}
	hist.Recent = slices.Insert(hist.Recent, 0, ended)
	if len(hist.Recent) > sessionlog.MaxRecentTurns {
		hist.Recent = hist.Recent[:sessionlog.MaxRecentTurns]
	}
	hist.Current, hist.CurrentStart, hist.CurrentKeys = runtime.TurnState{}, time.Time{}, nil
}

// turnHistoryFromScan converts a transcript scan to a runtime turn history:
// ids verbatim, times at the source's millisecond precision, statuses mapped
// and ObservedAt the observer's clock. An ended turn recorded without a usable
// id is left out: it cannot be told from any other. An open turn without one
// is an error wrapping runtime.ErrTurnIdentityUnsupported, never idle.
func turnHistoryFromScan(scan sessionlog.TurnScan, observedAt time.Time) (runtime.TurnHistory, error) {
	hist := runtime.TurnHistory{ObservedAt: observedAt}
	if scan.Open {
		if !runtime.IsTurnIdentity(scan.Current.ID) {
			return runtime.TurnHistory{}, fmt.Errorf("transcript records the open turn without an id: %w", runtime.ErrTurnIdentityUnsupported)
		}
		hist.Current = runtime.TurnState{TurnID: scan.Current.ID}
		hist.CurrentStart = turnMillis(scan.Current.StartedAt)
	}
	for _, t := range scan.Recent {
		if !runtime.IsTurnIdentity(t.ID) {
			continue
		}
		hist.Recent = append(hist.Recent, runtime.EndedTurn{
			TurnID:    t.ID,
			StartedAt: turnMillis(t.StartedAt),
			EndedAt:   turnMillis(t.EndedAt),
			Status:    runtimeTurnStatus(t.Status),
		})
	}
	return hist, nil
}

// turnMillis keeps a source time at millisecond precision, the precision the
// transcript families record and the delivery record compares against.
func turnMillis(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Truncate(time.Millisecond)
}

func runtimeTurnStatus(s sessionlog.TurnStatus) runtime.TurnStatus {
	switch s {
	case sessionlog.TurnStatusCompleted:
		return runtime.TurnStatusCompleted
	case sessionlog.TurnStatusCancelled:
		return runtime.TurnStatusCancelled
	case sessionlog.TurnStatusFailed:
		return runtime.TurnStatusFailed
	default:
		return ""
	}
}
