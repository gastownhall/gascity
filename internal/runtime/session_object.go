package runtime

import (
	"errors"
	"strings"
	"time"
)

// ErrSessionObjectKillUnsupported reports that no backend can kill a session
// object by id.
var ErrSessionObjectKillUnsupported = errors.New("runtime does not implement exact session-object kills")

// ErrInvalidSessionObject reports a kill request whose name, object id,
// creation time or pane pid is malformed or empty. Nothing is sent to the
// runtime.
var ErrInvalidSessionObject = errors.New("invalid session object")

// SessionObjectKillResult is how an exact session-object kill ended.
type SessionObjectKillResult int

const (
	// SessionObjectNotKilled means the call failed before a verdict; see its
	// error.
	SessionObjectNotKilled SessionObjectKillResult = iota
	// SessionObjectKilled means the object passed the re-check and was killed.
	SessionObjectKilled
	// SessionObjectGone means the observed object no longer exists: the id
	// names no session, or names one created at another time (a restarted
	// server reused the id).
	SessionObjectGone
	// SessionObjectRenamed means the id names a session under another name.
	SessionObjectRenamed
	// SessionObjectLive means a corpse kill was refused, because a pane is
	// live or the session has more than one window or pane.
	SessionObjectLive
	// SessionObjectChanged means a zombie kill was refused, because the pane
	// died, its pid changed, or the session has more than one window or pane.
	SessionObjectChanged
)

// SessionObjectKiller kills the exact session object a fresh read observed
// (Liveness.ObjectID and Liveness.ObjectCreated), for v5 F2's two
// identity-waived exceptions. The re-check and the kill run as one
// runtime-side command, so a session re-created under the name between the
// read and the kill is refused.
type SessionObjectKiller interface {
	// KillCorpseObject kills objectID only while it was created at created,
	// is named name and has one window with one pane, which is dead.
	KillCorpseObject(name, objectID, created string) (SessionObjectKillResult, error)
	// KillZombieObject kills objectID only while it was created at created,
	// is named name and has one window with one live pane whose pid is
	// panePID (Liveness.PanePID).
	KillZombieObject(name, objectID, created, panePID string) (SessionObjectKillResult, error)
}

// FreshByConstruction is implemented by providers whose
// ObserveLivenessWithError reads the runtime on every call (acp and
// subprocess probe their control socket), so that read is already fresh.
type FreshByConstruction interface {
	LivenessReadsFresh() bool
}

// FreshLivenessObserver is the optional capability for a liveness read that
// reflects the runtime at or after since (v5 O1 "fresh for the effect").
type FreshLivenessObserver interface {
	ObserveLivenessSince(name string, processNames []string, since time.Time) (Liveness, error)
}

// ObserveLivenessSince returns a liveness read taken at or after since. A
// provider without [FreshLivenessObserver] answers from
// [ObserveLivenessWithError]: acp and subprocess probe their control socket on
// every call, so that read is already fresh. A provider that caches must
// implement the capability before v2 reads it.
func ObserveLivenessSince(sp Provider, name string, processNames []string, since time.Time) (Liveness, error) {
	if sp == nil || strings.TrimSpace(name) == "" {
		return Liveness{}, nil
	}
	if observer, ok := sp.(FreshLivenessObserver); ok {
		obs, err := observer.ObserveLivenessSince(name, processNames, since)
		return normalizeLiveness(obs), err
	}
	return ObserveLivenessWithError(sp, name, processNames)
}
