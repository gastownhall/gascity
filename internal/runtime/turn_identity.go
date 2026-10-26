package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// TurnStart is how a runtime knows a session's turn starts. A value this build
// does not know, or the empty string, reads as [TurnStartNone].
type TurnStart string

// The TurnStart values, lowest first.
const (
	// TurnStartNone means no turn start is observed.
	TurnStartNone TurnStart = "none"
	// TurnStartBoundary means busy/idle boundaries are observed, without ids.
	TurnStartBoundary TurnStart = "boundary"
	// TurnStartRecorded means every start the source shows carries its id.
	TurnStartRecorded TurnStart = "recorded"
)

// TurnEnd is how a runtime knows a session's turn ends. A value this build
// does not know, or the empty string, reads as [TurnEndNone].
type TurnEnd string

// The TurnEnd values, lowest first.
const (
	// TurnEndNone means no turn end is observed.
	TurnEndNone TurnEnd = "none"
	// TurnEndBoundary means idle boundaries are observed, without ids.
	TurnEndBoundary TurnEnd = "boundary"
	// TurnEndInferred means some ends are inferred from idle boundaries.
	TurnEndInferred TurnEnd = "inferred"
	// TurnEndRecorded means every end carries its id and how it ended.
	TurnEndRecorded TurnEnd = "recorded"
)

// TurnFence is how a runtime confines an interrupt to a named turn. A value
// this build does not know, or the empty string, reads as [TurnFenceNone].
type TurnFence string

// The TurnFence values, lowest first.
const (
	// TurnFenceNone means an interrupt cannot be aimed at a turn.
	TurnFenceNone TurnFence = "none"
	// TurnFenceChecked means the turn is re-read just before an untargeted
	// interrupt, so a turn starting in between can still be hit.
	TurnFenceChecked TurnFence = "checked"
	// TurnFenceAtomic means the runtime refuses, in one critical section, to
	// interrupt a turn that is not current.
	TurnFenceAtomic TurnFence = "atomic"
)

// TurnLink is how a delivery is tied to the turn it started. A value this
// build does not know, or the empty string, reads as [TurnLinkNone].
type TurnLink string

// The TurnLink values, lowest first.
const (
	// TurnLinkNone means no delivery can be tied to a turn.
	TurnLinkNone TurnLink = "none"
	// TurnLinkCorrelated means the runtime confirms only that it took the
	// input, so the turn is matched by observation.
	TurnLinkCorrelated TurnLink = "correlated"
	// TurnLinkExact means the runtime names the turn its delivery started.
	TurnLinkExact TurnLink = "exact"
)

// axisRank is the position of v in values (lowest first), or 0 when v is
// not one of them.
func axisRank[T ~string](v T, values ...T) int {
	for i, known := range values {
		if v == known {
			return i
		}
	}
	return 0
}

func (s TurnStart) rank() int {
	return axisRank(s, TurnStartNone, TurnStartBoundary, TurnStartRecorded)
}

func (e TurnEnd) rank() int {
	return axisRank(e, TurnEndNone, TurnEndBoundary, TurnEndInferred, TurnEndRecorded)
}

func (f TurnFence) rank() int {
	return axisRank(f, TurnFenceNone, TurnFenceChecked, TurnFenceAtomic)
}

func (l TurnLink) rank() int {
	return axisRank(l, TurnLinkNone, TurnLinkCorrelated, TurnLinkExact)
}

// AtLeast reports whether s is floor or higher; an unknown value is none.
func (s TurnStart) AtLeast(floor TurnStart) bool { return s.rank() >= floor.rank() }

// AtLeast reports whether e is floor or higher; an unknown value is none.
func (e TurnEnd) AtLeast(floor TurnEnd) bool { return e.rank() >= floor.rank() }

// AtLeast reports whether f is floor or higher; an unknown value is none.
func (f TurnFence) AtLeast(floor TurnFence) bool { return f.rank() >= floor.rank() }

// AtLeast reports whether l is floor or higher; an unknown value is none.
func (l TurnLink) AtLeast(floor TurnLink) bool { return l.rank() >= floor.rank() }

// TurnCapabilities is what a runtime can tell about one session's turns
// (engdocs/design/x11-turn-identity-coverage.md §3.1, a wave-2 design not
// yet in tree). The zero value is nothing on every axis. These are transport
// facts about the runtime, never about the agent role running in it.
type TurnCapabilities struct {
	Start TurnStart
	End   TurnEnd
	Fence TurnFence
	Link  TurnLink
	// Lossless means every turn since the last TurnHistory read can be
	// recovered from the next one.
	Lossless bool
}

// Normalized returns c with every axis a value this build knows: an unknown
// or empty value becomes that axis's none.
func (c TurnCapabilities) Normalized() TurnCapabilities {
	return TurnCapabilities{
		Start:    [...]TurnStart{TurnStartNone, TurnStartBoundary, TurnStartRecorded}[c.Start.rank()],
		End:      [...]TurnEnd{TurnEndNone, TurnEndBoundary, TurnEndInferred, TurnEndRecorded}[c.End.rank()],
		Fence:    [...]TurnFence{TurnFenceNone, TurnFenceChecked, TurnFenceAtomic}[c.Fence.rank()],
		Link:     [...]TurnLink{TurnLinkNone, TurnLinkCorrelated, TurnLinkExact}[c.Link.rank()],
		Lossless: c.Lossless,
	}
}

// EndedTurn is a turn the source saw end.
type EndedTurn struct {
	// TurnID is the turn's id, as [TurnState.TurnID].
	TurnID string
	// StartedAt is the source's start time at its own precision; zero when
	// unknown.
	StartedAt time.Time
	// EndedAt is the source's end time, or the observer's for an inferred end.
	EndedAt time.Time
	// Status is how the turn ended; empty when the source closed it only
	// because a newer turn started.
	Status TurnStatus
	// Inferred reports that the end was inferred from an idle boundary.
	Inferred bool
	// DeliveryKeys are the delivery keys whose messages the runtime itself
	// reports as this turn's input. They are never parsed from delivered text.
	DeliveryKeys []string
}

// TurnHistory is one read of a session's current and recent turns.
type TurnHistory struct {
	// Current is the turn in progress, as ObserveTurn would report it.
	Current TurnState
	// CurrentStart is the current turn's start, as EndedTurn.StartedAt; zero
	// when unknown or idle.
	CurrentStart time.Time
	// CurrentKeys are the current turn's delivery keys, as
	// EndedTurn.DeliveryKeys.
	CurrentKeys []string
	// Recent are the turns that ended, newest first, at most the source's
	// bound.
	Recent []EndedTurn
	// ObservedAt is when the read was made, on the reader's clock.
	ObservedAt time.Time
}

// MaxTurnIDLen bounds a turn id in bytes. A longer id is not an identity: a
// reader treats the session as unable to identify that turn rather than
// truncate or hash the id, which would break the match between observers.
const MaxTurnIDLen = 128

// IsTurnIdentity reports whether id can identify a turn: non-empty, at most
// [MaxTurnIDLen] bytes, valid UTF-8 and free of control characters. Turn ids
// are compared only for equality.
func IsTurnIdentity(id string) bool {
	if id == "" || len(id) > MaxTurnIDLen || !utf8.ValidString(id) {
		return false
	}
	return !strings.ContainsFunc(id, unicode.IsControl)
}

var (
	// ErrTurnIdentityUnsupported reports that the runtime cannot identify the
	// session's turns.
	ErrTurnIdentityUnsupported = errors.New("runtime cannot identify the session's turns")
	// ErrTurnFenceUnavailable reports that the process owning the session
	// could not be asked about its turns, as with an owner that predates the
	// turn commands. It wraps ErrRuntimeUnavailable, so callers retry rather
	// than act unfenced.
	ErrTurnFenceUnavailable = fmt.Errorf("%w: turn fence unavailable", ErrRuntimeUnavailable)
)

// TurnObservationProvider is an optional extension for runtimes that know
// their sessions' turns natively. Composites answer for the session's routed
// backend: no capabilities, and ErrInteractionUnsupported from TurnHistory,
// for a backend that lacks it.
type TurnObservationProvider interface {
	// TurnCapabilities reports what the runtime can tell about name's turns.
	TurnCapabilities(name string) TurnCapabilities
	// TurnHistory reads name's turns now. A read that cannot be made returns
	// an error wrapping ErrRuntimeUnavailable, ErrSessionNotFound or
	// ErrSessionInitializing; it never reports idle instead.
	TurnHistory(ctx context.Context, name string) (TurnHistory, error)
}

// TurnCapabilitiesFor reports p's capabilities for session name, normalized:
// nothing on every axis when p is not a [TurnObservationProvider], and an
// unknown or empty axis value as that axis's none. Its result is the
// canonical form: "no capabilities" is TurnCapabilities{}.Normalized(), not
// the zero value, so compare capabilities only after normalizing them.
func TurnCapabilitiesFor(p Provider, name string) TurnCapabilities {
	tp, ok := p.(TurnObservationProvider)
	if !ok {
		return TurnCapabilities{}.Normalized()
	}
	return tp.TurnCapabilities(name).Normalized()
}

// LocalTranscriptProvider is an optional extension for runtimes that run the
// agent on this host, so the transcript the agent writes lands on this host's
// filesystem, where gc can read it for turns (X11 §3.2: the transcript tiers
// need the reader on the transcript's host). A runtime that runs the agent
// elsewhere (a pod, a remote host, an exec script's own box) does not
// implement it. Composites do not either: the trait is read on the routed
// leaf ([LocalTranscriptsFor]).
type LocalTranscriptProvider interface {
	LocalTranscripts() bool
}

// LocalTranscriptsFor reports whether the backend serving session name runs
// the agent on this host: the leaf [ResolveBackend] names implements
// [LocalTranscriptProvider] and says so. A backend that does not declare it
// is taken to be remote, so a transcript that happens to resolve here is
// never read as its session's.
func LocalTranscriptsFor(p Provider, name string) bool {
	leaf, _, _ := ResolveBackend(p, name)
	lt, ok := leaf.(LocalTranscriptProvider)
	return ok && lt.LocalTranscripts()
}

// IdleSnapshotsFor reports whether the backend serving session name takes
// idle snapshots: the leaf [ResolveBackend] names is an
// [IdleSnapshotProvider]. It takes none, so it costs no runtime call.
func IdleSnapshotsFor(p Provider, name string) bool {
	leaf, _, _ := ResolveBackend(p, name)
	_, ok := leaf.(IdleSnapshotProvider)
	return ok
}

// InterruptedTurnResetsFor reports whether the backend serving session name
// discards an interrupted turn: the leaf [ResolveBackend] names is an
// [InterruptedTurnResetProvider]. Composites forward the reset to that leaf,
// so it is the one whose answer counts. It resets nothing, so it costs no
// runtime call.
func InterruptedTurnResetsFor(p Provider, name string) bool {
	leaf, _, _ := ResolveBackend(p, name)
	_, ok := leaf.(InterruptedTurnResetProvider)
	return ok
}
