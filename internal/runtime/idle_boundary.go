package runtime

import "errors"

// IdleBoundary is one non-blocking reading of whether a session is at an
// idle input boundary now ([IdleBoundaryNow]).
type IdleBoundary uint8

const (
	// IdleBoundaryUnknown means the reading says neither: the runtime takes
	// no idle snapshot of the session, the snapshot failed, or it was
	// indeterminate. Callers keep the rule they would use without it.
	IdleBoundaryUnknown IdleBoundary = iota
	// IdleBoundaryAt means the session reads idle at its input boundary.
	IdleBoundaryAt
	// IdleBoundaryBusy means the runtime reports the session busy, a reading
	// the runtime trusts for the session's provider family.
	IdleBoundaryBusy
	// IdleBoundaryPending means a pending interaction owns the session's
	// input: text sent now would answer it.
	IdleBoundaryPending
)

// String names the reading.
func (b IdleBoundary) String() string {
	switch b {
	case IdleBoundaryAt:
		return "at"
	case IdleBoundaryBusy:
		return "busy"
	case IdleBoundaryPending:
		return "pending"
	default:
		return "unknown"
	}
}

// OutOfReach reports whether the reading rules out an idle boundary within a
// short wait: the session is busy, or a pending interaction owns its input.
func (b IdleBoundary) OutOfReach() bool {
	return b == IdleBoundaryBusy || b == IdleBoundaryPending
}

// IdleBoundaryNow takes a single [IdleSnapshotProvider] reading of session
// name on p, without waiting, and classifies it. It is the pre-read of a
// wait-idle delivery: a session that reads busy or pending will not reach an
// idle boundary within a short wait, so the delivery is queued at once
// instead. Which provider families' busy readings count is the runtime's
// policy: a runtime that does not trust a family's busy reading reports it
// as [ErrIdleIndeterminate], which reads as [IdleBoundaryUnknown].
func IdleBoundaryNow(p Provider, name string) IdleBoundary {
	snap, ok := p.(IdleSnapshotProvider)
	if !ok {
		return IdleBoundaryUnknown
	}
	idle, err := snap.SnapshotIdle(name)
	switch {
	case errors.Is(err, ErrPendingInteraction):
		return IdleBoundaryPending
	case err != nil:
		return IdleBoundaryUnknown
	case idle:
		return IdleBoundaryAt
	}
	return IdleBoundaryBusy
}
