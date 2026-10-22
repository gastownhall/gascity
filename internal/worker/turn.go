package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// Turn identity at the worker boundary (engdocs/design/x11-turn-identity-coverage.md
// §5.2, slice TI-2b). A session's turns come from one source: the runtime's
// protocol when it observes turns itself, else the transcript of its current
// conversation when the family records turn ids, finds that transcript by the
// session's key and the agent runs on this host, else nothing. The worker
// composes that choice per session from fixed traits, so it holds for the
// session's generation, and reads turns through it. No turn source reads an
// id out of delivered text (X11 D13).

// TurnHandle exposes a worker's turn identity.
type TurnHandle interface {
	// TurnCapabilities reports what the worker can tell about its session's
	// turns, composed from the routed runtime and the transcript family.
	TurnCapabilities(context.Context) (TurnCapabilities, error)
	// CurrentTurn reads the session's current and recent turns now. A source
	// that cannot be read (a transcript not written yet included) is an error
	// wrapping runtime.ErrRuntimeUnavailable, never an idle history; a session
	// with no source, or a transcript that records turns without ids, is
	// runtime.ErrTurnIdentityUnsupported; a session that is gone or closed is
	// runtime.ErrSessionNotFound. Concurrent reads of one session share one
	// runtime read.
	CurrentTurn(context.Context) (runtime.TurnHistory, error)
	// FreshTurn is CurrentTurn from reads of its own, started within the
	// call: it never joins a read of the session already in flight, which
	// may have begun before the turn now running did and so name the turn
	// before it. A read that decides which turn to act on (an interrupt's
	// target) is this one. Its session load and runtime probes are bounded
	// by ctx, a store that stalls included, so it returns when ctx ends
	// (runtime.ErrRuntimeUnavailable); a transcript's file reads are not
	// bounded. Fresh reads of one session do not share: while one is in
	// flight, healthy or left wedged by a caller that gave up on it, another
	// is refused at once with ErrObservationOutstanding (and
	// runtime.ErrRuntimeUnavailable), on either handle of the session, so a
	// caller serializes its fresh reads of a session.
	FreshTurn(context.Context) (runtime.TurnHistory, error)
	// WatchTurns sends a history first, then a history whenever the current or
	// newest ended turn changes (coalesced: latest wins), and an update with
	// Err set when its source stops being readable and whenever the error
	// changes class (unavailable, initializing, unsupported, not found),
	// never an idle history instead. Every read reloads the session first,
	// so a session closed while it is watched is not found from the next
	// read on, as for CurrentTurn. A session reload or runtime probe that
	// does not answer within 10 s fails its read as unavailable; a
	// transcript's file reads are not bounded.
	// From a transcript, CurrentTurn scans its last 16 MiB
	// (sessionlog.ScanTurns) while WatchTurns folds what was appended since
	// its last rescan (sessionlog.TurnFollower), so the two can disagree about
	// a turn that began before that window. A transcript write wakes the watch
	// at once, but a move to a new conversation's transcript, and every write
	// while fsnotify is unavailable, is seen only on the backstop poll: within
	// 60 s while the session is idle.
	// It closes when ctx is done; a transcript watch closes once a file read
	// in progress returns. A session with no source is
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

// TurnReplace is what deliver-now does to a session's running turn before it
// delivers the new message (X11 D11), read by the lane's deliver-now. It is
// internal/session's decision (session.RuntimeTurnTraits.Replace), the one
// interrupt_now itself runs on, never a family table's.
type TurnReplace = sessionpkg.TurnReplacement

// The turn replacements.
const (
	// TurnReplaceNone means the running turn cannot be replaced.
	TurnReplaceNone = sessionpkg.TurnReplacementNone
	// TurnReplaceInterrupt interrupts the turn, then delivers.
	TurnReplaceInterrupt = sessionpkg.TurnReplacementInterrupt
	// TurnReplaceInterruptReset interrupts, resets the interrupted turn, then
	// delivers.
	TurnReplaceInterruptReset = sessionpkg.TurnReplacementInterruptReset
	// TurnReplaceRestart restarts the agent, discarding the pending turn.
	TurnReplaceRestart = sessionpkg.TurnReplacementRestart
)

// TurnCapabilities is what a worker can tell about its session's turns.
type TurnCapabilities struct {
	runtime.TurnCapabilities
	// Source is where the turns are read from.
	Source TurnSource
	// Interrupt is how the runtime interrupts the session's turn. Unsupported
	// forces Fence none.
	Interrupt runtime.TurnInterruptMode
	// Replace is what deliver-now does to the session's running turn, as
	// internal/session decides it; none where the handle has no deliver-now.
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
	// turnWatchReadBound bounds a watcher read's session reload and runtime
	// probes. A transcript's file reads take no context and are not bounded.
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
// one can be read for the session here (transcript: the runtime runs the
// agent on this host and the family finds the conversation's own transcript
// by its key), the family records turn starts and the runtime interrupts by
// keystroke; else none. An inferred end needs idle snapshots, or nobody can
// see it, so without them End is none. The transcript is never Lossless: a
// read reports at most sessionlog.MaxRecentTurns ended turns from a bounded
// window, so turns that end between two reads can fall out of it. Interrupt
// mode unsupported forces Fence none. Every input is a fixed trait, never
// whether a file exists yet, so the capabilities do not move while a session
// waits for its transcript. replace is internal/session's answer for the
// session, passed through.
func composeTurnCapabilities(rcaps runtime.TurnCapabilities, mode runtime.TurnInterruptMode, support sessionlog.TurnSupport, transcript, idleSnapshots bool, replace TurnReplace) TurnCapabilities {
	caps := TurnCapabilities{
		TurnCapabilities: noTurnCapabilities,
		Source:           TurnSourceNone,
		Interrupt:        mode,
		Replace:          replace,
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
			Start: runtime.TurnStartRecorded,
			End:   end,
			Fence: runtime.TurnFenceChecked,
			Link:  runtime.TurnLink(support.Link),
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
	caps     TurnCapabilities
	info     sessionpkg.Info // the session as the plan read it
	provider string          // the transcript family's provider string
	path     string          // the transcript being read, for a transcript source
	// reads is how the plan's runtime probes are made (observeTurnRead).
	reads turnReads
}

// turnReads is how a turn read makes its runtime probes, and a locked read
// its session load too (observeTurnRead).
type turnReads int

const (
	// sharedTurnReads join every concurrent read of their key
	// (ObserveShared): CurrentTurn's and a watcher's.
	sharedTurnReads turnReads = iota
	// freshTurnReads are the read's own, started within the call, so a read
	// already in flight, which may have begun before the turn now running
	// did, never answers them (FreshTurn).
	freshTurnReads
	// lockedTurnReads are a read's own, as fresh ones are, for a read made
	// under the session mutation lock (lockedCurrentTurn), under keys apart
	// from a fresh read's; an idle state such a read cannot read fails it
	// (idleUnknown).
	lockedTurnReads
)

// planTurns composes the session's turn source now, from the session and the
// fixed traits of its routed runtime.
func (h *SessionHandle) planTurns() (turnPlan, error) {
	return h.planTurnsFrom(h.turnSession)
}

// planTurnsWithin is planTurns with the session reload bounded by ctx, as a
// watcher's is (reloadTurnSession): a store that stalls leaves the plan
// unanswered, runtime.ErrRuntimeUnavailable, when ctx ends.
func (h *SessionHandle) planTurnsWithin(ctx context.Context) (turnPlan, error) {
	return h.planTurnsFrom(func(id string) (sessionpkg.Info, error) {
		return h.reloadTurnSession(ctx, id)
	})
}

// planTurnsFrom is planTurns with the session loaded by load.
func (h *SessionHandle) planTurnsFrom(load func(id string) (sessionpkg.Info, error)) (turnPlan, error) {
	id := h.currentSessionID()
	if id == "" {
		// No session yet: nothing to read turns from, nor a turn to replace.
		support := h.adapter.TurnSupportFor(h.session.Provider)
		return turnPlan{caps: composeTurnCapabilities(noTurnCapabilities, runtime.TurnInterruptKeystroke, support, false, false, TurnReplaceNone)}, nil
	}
	info, err := load(id)
	if err != nil {
		return turnPlan{}, err
	}
	traits := h.manager.RuntimeTurnTraits(info)
	plan := turnPlan{info: info, provider: h.historyProvider(info)}
	support := h.adapter.TurnSupportFor(plan.provider)
	transcript := traits.LocalTranscripts &&
		strings.TrimSpace(info.WorkDir) != "" &&
		sessionpkg.HasKeyedTranscriptLookup(info, plan.provider)
	plan.caps = composeTurnCapabilities(traits.Capabilities, traits.Interrupt, support, transcript, traits.IdleSnapshots, traits.Replace)
	return plan, nil
}

// turnSession loads session id for a turn read. A session that is gone,
// closed or not a session is runtime.ErrSessionNotFound; any other failed
// load is runtime.ErrRuntimeUnavailable, the read's answer unknown.
func (h *SessionHandle) turnSession(id string) (sessionpkg.Info, error) {
	info, err := h.manager.Get(id)
	switch {
	case err != nil:
		return sessionpkg.Info{}, turnRowError(id, err)
	case info.Closed:
		return sessionpkg.Info{}, turnRowError(id, sessionpkg.ErrSessionClosed)
	}
	return info, nil
}

// turnRowError is the error of a turn read whose load of session id's row
// failed with err: a row that is gone, closed or not a session is
// runtime.ErrSessionNotFound; any other failure is
// runtime.ErrRuntimeUnavailable, the read's answer unknown.
func turnRowError(id string, err error) error {
	if TurnSessionGone(err) {
		return fmt.Errorf("turns of session %s: %w: %w", id, runtime.ErrSessionNotFound, err)
	}
	return fmt.Errorf("turns of session %s: %w: %w", id, runtime.ErrRuntimeUnavailable, err)
}

// TurnSessionGone reports whether a turn read failed because its session's
// row is gone, closed or not a session (turnRowError). A runtime that reports
// runtime.ErrSessionNotFound on its own, as a protocol source does when no
// runtime is bound to the session, says nothing about the row: the session
// can be open.
func TurnSessionGone(err error) bool {
	return errors.Is(err, beads.ErrNotFound) || errors.Is(err, sessionpkg.ErrSessionNotFound) ||
		errors.Is(err, sessionpkg.ErrNotSession) || errors.Is(err, sessionpkg.ErrSessionClosed)
}

// conversationTranscript resolves the transcript of info's current
// conversation: only a file its provider session key finds, and that records
// that conversation, never a work-directory or newest-file fallback, which
// can return another conversation's file (KeyedTranscriptPath's rule). A
// session without a key yet, or whose transcript is not written yet, is
// runtime.ErrRuntimeUnavailable: its source exists, its turns are unknown.
func (h *SessionHandle) conversationTranscript(info sessionpkg.Info, provider string) (string, error) {
	key := strings.TrimSpace(info.SessionKey)
	if key == "" {
		return "", fmt.Errorf("turns of session %s: its conversation has no session key yet: %w", info.ID, runtime.ErrRuntimeUnavailable)
	}
	path := sessionpkg.ResolveKeyedTranscriptPaths([]sessionpkg.Info{info}, h.adapter.SearchPaths, provider)[info.ID]
	if path == "" {
		return "", fmt.Errorf("turns of session %s: no single transcript of conversation %s found: %w", info.ID, key, runtime.ErrRuntimeUnavailable)
	}
	owned, err := h.adapter.TurnTranscriptOwnedBy(provider, path, key)
	if err != nil {
		return "", fmt.Errorf("turns of session %s: %w: %w", info.ID, runtime.ErrRuntimeUnavailable, err)
	}
	if !owned {
		return "", fmt.Errorf("turns of session %s: transcript %s does not record conversation %s: %w", info.ID, path, key, runtime.ErrRuntimeUnavailable)
	}
	return path, nil
}

// TurnCapabilities reports what the worker can tell about its session's turns.
func (h *SessionHandle) TurnCapabilities(context.Context) (TurnCapabilities, error) {
	plan, err := h.planTurns()
	if err != nil {
		return TurnCapabilities{}, err
	}
	return plan.caps, nil
}

// CurrentTurn reads the session's current and recent turns from its source.
func (h *SessionHandle) CurrentTurn(ctx context.Context) (runtime.TurnHistory, error) {
	plan, err := h.planTurns()
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	return h.currentTurnOf(ctx, plan)
}

// FreshTurn is CurrentTurn from reads of its own (TurnHandle.FreshTurn):
// its session load, the protocol history and the idle overlay's snapshots go
// through freshTurnReads, the session load bounded by ctx like the rest.
func (h *SessionHandle) FreshTurn(ctx context.Context) (runtime.TurnHistory, error) {
	return h.ownCurrentTurn(ctx, freshTurnReads)
}

// lockedCurrentTurn is CurrentTurn for a read made under the session
// mutation lock. Its session load and runtime probes are its own, started
// within the call (lockedTurnReads), so a shared read already in flight,
// which may have begun before the lock, holding the session as it was then,
// never answers it.
func (h *SessionHandle) lockedCurrentTurn(ctx context.Context) (runtime.TurnHistory, error) {
	return h.ownCurrentTurn(ctx, lockedTurnReads)
}

// ownCurrentTurn is CurrentTurn from reads of its own, made as reads says
// (fresh or locked; observeTurnRead). Its session load is one of them, so a
// store that stalls leaves the read unanswered when ctx ends
// (runtime.ErrRuntimeUnavailable) rather than holding it until the store
// answers.
func (h *SessionHandle) ownCurrentTurn(ctx context.Context, reads turnReads) (runtime.TurnHistory, error) {
	plan, err := h.planTurnsFrom(func(id string) (sessionpkg.Info, error) {
		return observeTurnRead(ctx, reads, "session", h.turnObservationScope(), id, func(context.Context) (sessionpkg.Info, error) {
			return h.turnSession(id)
		})
	})
	if err != nil {
		return runtime.TurnHistory{}, err
	}
	plan.reads = reads
	return h.currentTurnOf(ctx, plan)
}

// currentTurnOf reads plan's turns from its source.
func (h *SessionHandle) currentTurnOf(ctx context.Context, plan turnPlan) (runtime.TurnHistory, error) {
	var err error
	switch plan.caps.Source {
	case TurnSourceProtocol:
		return h.protocolTurnHistory(ctx, plan)
	case TurnSourceTranscript:
		if plan.path, err = h.conversationTranscript(plan.info, plan.provider); err != nil {
			return runtime.TurnHistory{}, err
		}
		return h.transcriptTurnHistory(ctx, plan, func() (sessionlog.TurnScan, error) {
			return h.adapter.ScanTurns(plan.provider, plan.path)
		}, true)
	default:
		return runtime.TurnHistory{}, h.noTurnSource(plan)
	}
}

func (h *SessionHandle) noTurnSource(plan turnPlan) error {
	return fmt.Errorf("turns of session %s: %w", plan.info.ID, runtime.ErrTurnIdentityUnsupported)
}

// turnObservationScope names the city in a turn observation key, so two
// cities' sessions of one name never share an in-flight read.
func (h *SessionHandle) turnObservationScope() string {
	city, err := h.manager.CityDir()
	return turnCityScope(city, err, h.manager)
}

// turnCityScope is the scope of a city's turn observation keys: the city's
// directory, so every handle of one city's session derives one key; or, with
// no city, a name unique to owner, the object the reads go through.
func turnCityScope(city sessionpkg.CityDir, err error, owner any) string {
	if err == nil && !city.IsZero() {
		return city.String()
	}
	return fmt.Sprintf("unscoped:%p", owner)
}

// turnObservationKey is the ObserveBounded key of a turn read of one session.
// It is a namespace apart from liveness observation (postStartObservationKey
// in cmd/gc), so a wedged turn read never blocks a liveness read, nor the
// other way round. kind separates the history read and the idle snapshot,
// which name the session by its runtime name, from a watcher's session
// reload, which names it by its id. Turn reads go through ObserveShared:
// concurrent readers of one session share one read; a fresh read and a read
// under the session mutation lock go through ObserveBounded under a fresh-
// or locked- kind of their own (observeTurnRead).
func turnObservationKey(kind, scope, session string) string {
	return "worker.turns." + kind + "\x00" + scope + "\x00" + session
}

// observeTurnRead runs one turn read of kind for session as reads says:
// shared, through ObserveShared, with every concurrent one of its key; or a
// read of its own that starts within the call, through ObserveBounded under
// a key of the fresh or locked kind, which no shared read uses. A read of
// its own is refused at once (ErrObservationOutstanding) while another of
// its key is in flight, healthy or left wedged by a caller that gave up on
// it, so its callers serialize their reads of a session, and a wedged
// session still holds one goroutine per key.
func observeTurnRead[T any](ctx context.Context, reads turnReads, kind, scope, session string, read func(context.Context) (T, error)) (T, error) {
	switch reads {
	case freshTurnReads:
		return ObserveBounded(ctx, turnObservationKey("fresh-"+kind, scope, session), read)
	case lockedTurnReads:
		return ObserveBounded(ctx, turnObservationKey("locked-"+kind, scope, session), read)
	}
	return ObserveShared(ctx, turnObservationKey(kind, scope, session), read)
}

func (h *SessionHandle) protocolTurnHistory(ctx context.Context, plan turnPlan) (runtime.TurnHistory, error) {
	return observeTurnRead(ctx, plan.reads, "history", h.turnObservationScope(), plan.info.SessionName, func(ctx context.Context) (runtime.TurnHistory, error) {
		return h.manager.RuntimeTurnHistory(ctx, plan.info)
	})
}

// transcriptTurnHistory reads the transcript's turns through read, converts
// them and, for a family whose ends are inferred, applies the idle overlay.
// probe lets the overlay take idle readings; without it the overlay only
// reuses an end it already inferred for the unchanged transcript. Under a
// locked plan, an overlay that cannot read the session's idle state fails
// the read (applyIdleOverlay).
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
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w", plan.info.ID, err)
	}
	if plan.caps.End == runtime.TurnEndInferred && hist.Current.Active() {
		if err := h.applyIdleOverlay(ctx, plan, timing, &hist, before, probe); err != nil {
			return runtime.TurnHistory{}, err
		}
	}
	return hist, nil
}

// transcriptTurnsUnavailable is the error of a transcript that could not be
// read for turns: unknown, never idle (X11 I8).
func transcriptTurnsUnavailable(plan turnPlan, err error) error {
	if errors.Is(err, sessionlog.ErrTurnsUnsupported) {
		return fmt.Errorf("turns of session %s: %w: %w", plan.info.ID, runtime.ErrTurnIdentityUnsupported, err)
	}
	return fmt.Errorf("turns of session %s: %w: %w", plan.info.ID, runtime.ErrRuntimeUnavailable, err)
}

// applyIdleOverlay infers the end of hist's open turn (X11 §5.2): once the
// transcript has not grown for turnQuietWindow, two idle snapshots that both
// read idle, with no transcript growth between them, end the turn with
// Inferred set at the observer's clock. A busy reading, or growth, leaves the
// turn open. So does a reading that is neither idle nor busy, unless the plan
// is locked: the check under the session mutation lock takes only a busy
// reading as the turn running, so for it such a reading, a ctx that ends
// between the two, or a transcript it cannot stat again is the overlay's
// error (idleUnknown). before is the transcript's state before the scan.
func (h *SessionHandle) applyIdleOverlay(ctx context.Context, plan turnPlan, timing turnTiming, hist *runtime.TurnHistory, before os.FileInfo, probe bool) error {
	turnID := hist.Current.TurnID
	h.turns.mu.Lock()
	memo := h.turns.inferred
	h.turns.mu.Unlock()
	if memo.path == plan.path && memo.turnID == turnID && sameTranscriptState(memo.info, before) {
		endCurrentTurn(hist, memo.endedAt)
		return nil
	}
	if !probe || timing.now().Sub(before.ModTime()) < turnQuietWindow {
		return nil
	}
	if idle, err := h.readsIdle(ctx, plan); !idle {
		return err
	}
	if timing.idleGap > 0 {
		select {
		case <-ctx.Done():
			return plan.idleUnknown(ctx.Err())
		case <-timing.after(timing.idleGap):
		}
	}
	if idle, err := h.readsIdle(ctx, plan); !idle {
		return err
	}
	after, err := os.Stat(plan.path)
	switch {
	case err != nil:
		return plan.idleUnknown(err)
	case !sameTranscriptState(before, after):
		return nil
	}
	endedAt := timing.now()
	h.turns.mu.Lock()
	h.turns.inferred = inferredTurnEnd{path: plan.path, turnID: turnID, info: after, endedAt: endedAt}
	h.turns.mu.Unlock()
	endCurrentTurn(hist, endedAt)
	if hist.ObservedAt.Before(endedAt) {
		hist.ObservedAt = endedAt
	}
	return nil
}

// idleReading is one idle snapshot of a session.
type idleReading struct{ idle, supported bool }

// errNoIdleSnapshot is the reading of a runtime that took no idle snapshot of
// the session (RuntimeIdleSnapshot's unsupported).
var errNoIdleSnapshot = errors.New("its runtime takes no idle snapshot of it")

// readsIdle takes one idle snapshot of the session, bounded by ctx, and
// reports whether it read idle. A busy reading is not idle, and no error. A
// reading that is neither is not idle either, with plan.idleUnknown's error:
// one that failed, read indeterminate or unsupported, or went unanswered,
// ErrObservationOutstanding included, which a locked read gets at once while
// one an earlier check left wedged is still out.
func (h *SessionHandle) readsIdle(ctx context.Context, plan turnPlan) (bool, error) {
	got, err := observeTurnRead(ctx, plan.reads, "idle", h.turnObservationScope(), plan.info.SessionName, func(context.Context) (idleReading, error) {
		idle, supported, err := h.manager.RuntimeIdleSnapshot(plan.info)
		return idleReading{idle, supported}, err
	})
	switch {
	case err != nil:
		return false, plan.idleUnknown(err)
	case !got.supported:
		return false, plan.idleUnknown(errNoIdleSnapshot)
	}
	return got.idle, nil
}

// idleUnknown is the overlay's error when the session's idle state could not
// be read (cause): none for an unlocked plan, whose turn stays open, as on a
// busy reading; for a locked plan an error wrapping
// runtime.ErrRuntimeUnavailable, so the check under the lock refuses rather
// than take a reading it did not get as proof the turn runs.
func (p turnPlan) idleUnknown(cause error) error {
	switch {
	case p.reads != lockedTurnReads:
		return nil
	case errors.Is(cause, runtime.ErrRuntimeUnavailable):
		return fmt.Errorf("turns of session %s: its idle state is unknown: %w", p.info.ID, cause)
	}
	return fmt.Errorf("turns of session %s: its idle state is unknown: %w: %w", p.info.ID, runtime.ErrRuntimeUnavailable, cause)
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
// and ObservedAt the observer's clock.
//
// A turn the transcript records without a usable id, from a start inside the
// scan (an older CLI, an id over sessionlog.MaxTurnIDLen), gives boundaries,
// not identity, so the read is an error wrapping
// runtime.ErrTurnIdentityUnsupported, whether or not a turn is open: never an
// idle history that hides the turns it could not name (X11 §8, D1: the
// observer downgrades the generation to boundary).
//
// A scan that begins inside a turn reads that turn without its start or id:
// a scan window cut in the turn's tail, or a Claude conversation that opens
// with a turn run from queued input, which has no user entry. That is the
// window, not the family, so it never reads as unsupported. Ended, such a
// turn is the oldest in the scan with no start, and is dropped, as a turn
// before the window is. Open, the session's turn is unknown until it ends:
// an error wrapping runtime.ErrRuntimeUnavailable, never idle.
//
// sessionlog's MissingIDs is not consulted: it is also set by such a head
// end, and by an id-less entry that continues a turn already named, neither
// of which loses an identity this history reports.
func turnHistoryFromScan(scan sessionlog.TurnScan, observedAt time.Time) (runtime.TurnHistory, error) {
	hist := runtime.TurnHistory{ObservedAt: observedAt}
	for i, t := range scan.Recent {
		if runtime.IsTurnIdentity(t.ID) {
			continue
		}
		if i != len(scan.Recent)-1 || !t.StartedAt.IsZero() {
			return runtime.TurnHistory{}, fmt.Errorf("transcript records turns without an id: %w", runtime.ErrTurnIdentityUnsupported)
		}
	}
	if scan.Open {
		switch {
		case runtime.IsTurnIdentity(scan.Current.ID):
		case scan.Current.StartedAt.IsZero():
			return runtime.TurnHistory{}, fmt.Errorf("transcript scan begins inside the open turn, before its id: %w", runtime.ErrRuntimeUnavailable)
		default:
			return runtime.TurnHistory{}, fmt.Errorf("transcript records the open turn without an id: %w", runtime.ErrTurnIdentityUnsupported)
		}
		hist.Current = runtime.TurnState{TurnID: scan.Current.ID}
		hist.CurrentStart = turnMillis(scan.Current.StartedAt)
	}
	for _, t := range scan.Recent {
		if !runtime.IsTurnIdentity(t.ID) {
			continue // the head end
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

// runtimeTurnStatus maps a transcript's turn status to the runtime's. The
// empty status, no recorded end, maps to the empty one: ended by a newer
// turn. The switch must name every other sessionlog.TurnStatus, or a status
// it misses reads as ended by a newer turn.
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

// The turn-scoped interrupt (X11 §5.2 "Interrupt with Turn set", slice
// TI-2b2, SESSION-RUNTIME-023). A session-scoped interrupt keeps StopTurn's
// path. One aimed at a turn is refused, with nothing sent, where the
// session's turns cannot confirm it (turnInterruptRefusal). A protocol source
// asks the runtime to interrupt the turn only while it is current and waits
// for its end (session.InterruptTurnOn). A transcript source checks the turn
// under the session mutation lock just before the keys
// (session.Manager.StopTurnIf) and then reads the transcript until the turn
// is no longer current: a family's own wait returning is no proof, as
// herdr's idle wait returns whether or not the agent went idle.

const (
	// turnInterruptSettle bounds the wait for an interrupted turn to end,
	// from the moment the check passed.
	turnInterruptSettle = sessionpkg.TurnInterruptSettle
	// turnSettlePoll separates the settle reads of a transcript source.
	turnSettlePoll = 500 * time.Millisecond
)

// turnInterruptRefusal is why caps cannot carry an interrupt of a named turn,
// or nil: a runtime that cannot interrupt at all, else a source that does not
// record starts, cannot fence at least by a check, or cannot see ends at
// least by inference, so that no read could confirm the turn settled (X11
// §3.3; subprocess and herdr Claude have End none).
func turnInterruptRefusal(caps TurnCapabilities, session, turn string) error {
	switch {
	case caps.Interrupt == runtime.TurnInterruptUnsupported:
		return fmt.Errorf("interrupting turn %q of %s: %w", turn, session, sessionpkg.ErrTurnInterruptUnsupported)
	case !caps.InterruptsTurns():
		return fmt.Errorf("interrupting turn %q of %s (start %s, end %s, fence %s): %w",
			turn, session, caps.Start, caps.End, caps.Fence, runtime.ErrTurnIdentityUnsupported)
	}
	return nil
}

// InterruptsTurns reports whether c can carry an interrupt of a named turn
// (InterruptRequest.Turn): the runtime can interrupt, and the source records
// starts, fences at least by a check and sees ends at least by inference, so
// a read can confirm the turn settled (X11 §3.3). Otherwise a turn-scoped
// interrupt is refused, nothing sent.
func (c TurnCapabilities) InterruptsTurns() bool {
	return c.Interrupt != runtime.TurnInterruptUnsupported && c.Start.AtLeast(runtime.TurnStartRecorded) &&
		c.Fence.AtLeast(runtime.TurnFenceChecked) && c.End.AtLeast(runtime.TurnEndInferred)
}

// protocolTurnInterruptFenceRefusal refuses an input fence on a protocol
// source's turn interrupt: the runtime's InterruptTurn reads no input fence,
// so it could not confine the interrupt to the fence's incarnation, and a
// fenced request is never sent unfenced (runtime.ErrInputFenceUnsupported).
func protocolTurnInterruptFenceRefusal(fence runtime.InputFence, session, turn string) error {
	if fence.IsZero() {
		return nil
	}
	return fmt.Errorf("interrupting turn %q of %s: its runtime interrupts turns by protocol, without an input fence: %w",
		turn, session, runtime.ErrInputFenceUnsupported)
}

// interruptTurn interrupts the session's turn req.Turn for by, as
// InterruptRequest.Turn documents. req has been validated.
func (h *SessionHandle) interruptTurn(ctx context.Context, by *sessionpkg.Actor, req InterruptRequest) (InterruptResult, error) {
	turn := req.Turn
	plan, err := h.planTurnsWithin(ctx)
	if err != nil {
		return InterruptResult{}, err
	}
	name := plan.info.SessionName
	if name == "" {
		name = "a session not created yet"
	}
	if err := turnInterruptRefusal(plan.caps, name, turn); err != nil {
		return InterruptResult{}, err
	}
	if req.ForReplacement {
		if err := replacementRefusal(plan.caps.Replace, name); err != nil {
			return InterruptResult{}, err
		}
	}
	switch plan.caps.Source {
	case TurnSourceProtocol:
		var result InterruptResult
		if !req.Fence.IsZero() && req.AllowUnfenced {
			req.Fence, result.Unfenced = runtime.InputFence{}, true
		}
		if err := protocolTurnInterruptFenceRefusal(req.Fence, name, turn); err != nil {
			return InterruptResult{}, err
		}
		restart, err := h.restartOnFailure(by, plan.info.ID, req.OnFailure)
		if err != nil {
			return InterruptResult{}, err
		}
		// The runtime's acknowledgement says whether it fenced the
		// interrupt itself; the capabilities' Fence already tells the
		// caller which tier it is on, so the result carries no more. The
		// Manager restarts a keystroke runtime (ACP) whose interrupted turn
		// was not seen to end, never a protocol runtime (its stop may leave
		// it running); a replacement needs nothing more of either.
		_, report, err := h.manager.InterruptTurn(ctx, plan.info.ID, turn, restart)
		result.Restarted = report.Restarted
		return result, turnInterruptResult(plan.info.ID, err)
	case TurnSourceTranscript:
		req, unfenced, err := h.unfenceable(plan.info.ID, req)
		if err != nil {
			return InterruptResult{}, err
		}
		// The replacement was refused above where it cannot run, so the
		// stop takes it as it is.
		restart, err := h.restartOnFailure(by, plan.info.ID, req.OnFailure)
		if err != nil {
			return InterruptResult{}, err
		}
		if unfenced {
			ctx = runtime.WithInputFence(ctx, runtime.InputFence{})
		}
		opts := sessionpkg.StopTurnOptions{ForReplacement: req.ForReplacement, Restart: restart}
		result, err := h.checkedTurnInterrupt(ctx, plan.info, turn, req.Fence, opts)
		result.Unfenced = unfenced
		return result, err
	}
	return InterruptResult{}, h.noTurnSource(plan)
}

// checkedTurnInterrupt is the checked transcript path: the session stop
// (session.Manager.StopTurnWith) re-reads the transcript under the session
// mutation lock and sends the family's sequence only while turn is the open
// turn, each key under fence when one is set, and checks again before the
// sequence's follow-up interrupt (a soft-escape family's Ctrl-C fallback);
// then, still under the lock, the transcript is read until turn is no longer
// current, within turnInterruptSettle of the check first passing
// (awaitTurnEnd), and opts' replacement or restart on failure runs, a
// restart only once something showed the turn still running. ctx bounds the
// wait for the lock and the check, whose session reload and runtime probes
// are further bounded by turnWatchReadBound: the check holds the lock. They
// are the check's own, started under the lock
// (lockedCurrentTurn), as the settle's reads are, so a shared read that
// began before it never decides the check or the settle. A check that
// cannot finish within them, or whose idle reading is neither idle nor busy
// (idleUnknown), refuses, nothing sent, with runtime.ErrRuntimeUnavailable:
// an idle reading it could not take is no proof the turn runs.
func (h *SessionHandle) checkedTurnInterrupt(ctx context.Context, info sessionpkg.Info, turn string, fence runtime.InputFence, opts sessionpkg.StopTurnOptions) (InterruptResult, error) {
	timing := h.turns.clock()
	var deadline time.Time
	checked := false
	unanswered := func(err error) error {
		return fmt.Errorf("checking turn %q of %s before interrupting it: %w: %w", turn, info.SessionName, runtime.ErrRuntimeUnavailable, err)
	}
	stopCtx := ctx
	if !fence.IsZero() {
		stopCtx = runtime.WithInputFence(ctx, fence)
	}
	opts.Still = func() error {
		if err := ctx.Err(); err != nil {
			return unanswered(err)
		}
		readCtx, cancel := context.WithTimeout(ctx, turnWatchReadBound)
		defer cancel()
		hist, err := h.lockedCurrentTurn(readCtx)
		if ctxErr := readCtx.Err(); ctxErr != nil {
			return unanswered(ctxErr)
		}
		if err != nil {
			return err
		}
		if hist.Current.TurnID != turn {
			return &runtime.TurnNotCurrentError{Session: info.SessionName, TurnID: turn, Current: hist.Current}
		}
		if !checked {
			// The settle's bound runs from the first pass; the check run
			// again before a follow-up interrupt does not move it.
			checked = true
			deadline = timing.now().Add(turnInterruptSettle)
		}
		return nil
	}
	opts.Settle = func(waitErr error) error {
		return h.awaitTurnEnd(ctx, info.SessionName, turn, deadline, timing, waitErr)
	}
	report, err := h.manager.StopTurnWith(stopCtx, info.ID, opts)
	result := InterruptResult{Restarted: report.Restarted}
	switch {
	case err == nil && !checked:
		// The stop found no turn to interrupt (a suspended row, a runtime
		// not running) and sent nothing.
		return result, &runtime.TurnNotCurrentError{Session: info.SessionName, TurnID: turn}
	case checked && errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable) &&
		!errors.Is(err, runtime.ErrTurnNotEnded) && !errors.Is(err, sessionpkg.ErrReplacementNotPrepared):
		// The runtime stopped between the check and the keys: nothing was
		// sent, and no turn runs. A replacement whose preparation found the
		// runtime gone sent its interrupt, and its turn settled.
		return result, &runtime.TurnNotCurrentError{Session: info.SessionName, TurnID: turn}
	case err != nil && !errors.Is(err, runtime.ErrTurnNotEnded):
		return result, turnInterruptResult(info.ID, err)
	}
	return result, err
}

// turnInterruptResult is err, from the Manager's turn-scoped stop or
// interrupt of session id, in the classes InterruptRequest.Turn documents.
// The Manager loads the row again under the session mutation lock and
// returns that load's own error, wrapped in session.ErrStopNotSent: a row
// closed or gone since the plan read, or not a session, is
// runtime.ErrSessionNotFound, as a turn read has it (turnRowError), and every
// other error in none of those classes, a row that would not load among
// them, is unknown, runtime.ErrRuntimeUnavailable. Either keeps
// session.ErrStopNotSent, so a caller that tells nothing sent from a runtime
// that could not answer still can.
func turnInterruptResult(id string, err error) error {
	switch {
	case err == nil,
		errors.Is(err, runtime.ErrTurnNotCurrent), errors.Is(err, runtime.ErrTurnNotEnded),
		errors.Is(err, runtime.ErrRuntimeUnavailable), errors.Is(err, runtime.ErrSessionNotFound),
		errors.Is(err, runtime.ErrTurnIdentityUnsupported), errors.Is(err, sessionpkg.ErrTurnInterruptUnsupported),
		errors.Is(err, runtime.ErrFenceMismatch), errors.Is(err, runtime.ErrInputFenceUnsupported),
		errors.Is(err, sessionpkg.ErrReplacementNotPrepared), errors.Is(err, sessionpkg.ErrRestartNotStarted),
		errors.Is(err, sessionpkg.ErrSessionStopping), errors.Is(err, ErrInterruptNotSent),
		errors.Is(err, ErrInterruptRestartUnsupported):
		return err
	}
	return turnRowError(id, err)
}

// awaitTurnEnd reads the session's turns until turn is no longer current,
// and returns nil then; a session that is gone or closed has ended its turn,
// as a protocol runtime's WaitTurnEnded reads one it no longer hosts. It
// reads at least once, even past deadline, since the family's own waits may
// have spent the bound; past deadline, or when ctx ends, the turn was not
// seen to end: an error wrapping runtime.ErrTurnNotEnded and no other
// sentinel, the last failure (cause, the family wait's, or a failed read) in
// its text. That is session.TurnNotEndedError once something showed the turn
// still running, a read naming it current or a family wait that failed for a
// cause other than a runtime that could not answer
// (runtime.ErrRuntimeUnavailable), and session.TurnEndUnreadError otherwise,
// which a restart on failure does not restart: whether the interrupt ended
// the turn is unknown. A failed read is never taken as the end, nor as the
// turn running. It runs under the session mutation lock (the stop's
// Settle), so each read is a locked one (lockedCurrentTurn), as the check's
// are: its session load and runtime probes are its own, started within the
// read, so a shared read that began before the lock, holding the session as
// it was then, never decides the end, nor keeps the settle, which holds the
// lock, waiting on its answer. Each read is bounded by turnWatchReadBound,
// as a watcher read's is.
func (h *SessionHandle) awaitTurnEnd(ctx context.Context, name, turn string, deadline time.Time, timing turnTiming, cause error) error {
	what := fmt.Sprintf("interrupted turn %q of %s", turn, name)
	read := func() (runtime.TurnHistory, error) {
		readCtx, cancel := context.WithTimeout(ctx, turnWatchReadBound)
		defer cancel()
		return h.lockedCurrentTurn(readCtx)
	}
	running := cause != nil && !errors.Is(cause, runtime.ErrRuntimeUnavailable)
	notEnded := func(last error) error {
		if running {
			return sessionpkg.TurnNotEndedError(what, last)
		}
		return sessionpkg.TurnEndUnreadError(what, last)
	}
	last := cause
	for {
		hist, err := read()
		switch {
		case err == nil && hist.Current.TurnID != turn:
			return nil
		case errors.Is(err, runtime.ErrSessionNotFound) && !errors.Is(err, runtime.ErrRuntimeUnavailable):
			return nil
		case err != nil:
			last = err
		default:
			running = true
		}
		left := deadline.Sub(timing.now())
		if left <= 0 {
			return notEnded(last)
		}
		select {
		case <-ctx.Done():
			return notEnded(ctx.Err())
		case <-timing.after(min(turnSettlePoll, left)):
		}
	}
}
