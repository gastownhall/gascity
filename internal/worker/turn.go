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
}

// planTurns composes the session's turn source now, from the session and the
// fixed traits of its routed runtime.
func (h *SessionHandle) planTurns() (turnPlan, error) {
	id := h.currentSessionID()
	if id == "" {
		// No session yet: nothing to read turns from, nor a turn to replace.
		support := h.adapter.TurnSupportFor(h.session.Provider)
		return turnPlan{caps: composeTurnCapabilities(noTurnCapabilities, runtime.TurnInterruptKeystroke, support, false, false, TurnReplaceNone)}, nil
	}
	info, err := h.turnSession(id)
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
	case errors.Is(err, beads.ErrNotFound), errors.Is(err, sessionpkg.ErrSessionNotFound),
		errors.Is(err, sessionpkg.ErrNotSession), errors.Is(err, sessionpkg.ErrSessionClosed):
		return sessionpkg.Info{}, fmt.Errorf("turns of session %s: %w: %w", id, runtime.ErrSessionNotFound, err)
	case err != nil:
		return sessionpkg.Info{}, fmt.Errorf("turns of session %s: %w: %w", id, runtime.ErrRuntimeUnavailable, err)
	case info.Closed:
		return sessionpkg.Info{}, fmt.Errorf("turns of session %s: %w: %w", id, runtime.ErrSessionNotFound, sessionpkg.ErrSessionClosed)
	}
	return info, nil
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
// concurrent readers of one session share one read.
func turnObservationKey(kind, scope, session string) string {
	return "worker.turns." + kind + "\x00" + scope + "\x00" + session
}

func (h *SessionHandle) protocolTurnHistory(ctx context.Context, plan turnPlan) (runtime.TurnHistory, error) {
	key := turnObservationKey("history", h.turnObservationScope(), plan.info.SessionName)
	return ObserveShared(ctx, key, func(ctx context.Context) (runtime.TurnHistory, error) {
		return h.manager.RuntimeTurnHistory(ctx, plan.info)
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
		return runtime.TurnHistory{}, fmt.Errorf("turns of session %s: %w", plan.info.ID, err)
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
		return fmt.Errorf("turns of session %s: %w: %w", plan.info.ID, runtime.ErrTurnIdentityUnsupported, err)
	}
	return fmt.Errorf("turns of session %s: %w: %w", plan.info.ID, runtime.ErrRuntimeUnavailable, err)
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
	if hist.ObservedAt.Before(endedAt) {
		hist.ObservedAt = endedAt
	}
}

// readsIdle takes one idle snapshot of the session, bounded by ctx. Only a
// successful idle reading counts; busy, unsupported or failed is not idle.
func (h *SessionHandle) readsIdle(ctx context.Context, plan turnPlan) bool {
	type snapshot struct{ idle, supported bool }
	key := turnObservationKey("idle", h.turnObservationScope(), plan.info.SessionName)
	got, err := ObserveShared(ctx, key, func(context.Context) (snapshot, error) {
		idle, supported, err := h.manager.RuntimeIdleSnapshot(plan.info)
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
