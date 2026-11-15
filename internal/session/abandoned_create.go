package session

import "time"

// StaleCreatingTimeout bounds how long a creating attempt may sit before it is
// stale, measured from the attempt's anchor (see CreateAttemptAnchor).
const StaleCreatingTimeout = time.Minute

// NeverStartedCreateLease is how long a pending-create claim that never
// recorded a provider start (no last_woke_at) stays leased, measured from the
// attempt's anchor.
const NeverStartedCreateLease = 10 * time.Minute

// StaleKeyDetectDelay is the immutable production window between a keyed
// session start and the liveness probe that detects a stale resume key. The
// in-flight lease window is derived from it.
const StaleKeyDetectDelay = 2 * time.Second

// AbandonedCreate describes a session whose in-flight create has been
// abandoned.
type AbandonedCreate struct {
	ID       string
	Template string
	State    State
	Since    time.Time
}

// CreateAttemptAnchor returns the start of info's current create attempt.
func CreateAttemptAnchor(_ Info) time.Time {
	return time.Time{}
}

// CreateAttemptStale reports whether info's create attempt has aged past
// StaleCreatingTimeout at now.
func CreateAttemptStale(_ Info, _ time.Time) bool {
	return false
}

// DetectAbandonedCreate reports whether info is an abandoned in-flight create
// at now.
func DetectAbandonedCreate(_ Info, _ time.Time, _ time.Duration) (AbandonedCreate, bool) {
	return AbandonedCreate{}, false
}

// DetectAbandonedCreates returns every abandoned create in infos, oldest first.
func DetectAbandonedCreates(_ []Info, _ time.Time, _ time.Duration) []AbandonedCreate {
	return nil
}
