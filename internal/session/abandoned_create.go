package session

import (
	"slices"
	"strings"
	"time"
)

// StaleCreatingTimeout bounds how long a creating attempt may sit before it is
// stale, measured from the attempt's anchor (see CreateAttemptAnchor). Pending
// creates that never recorded a provider start use NeverStartedCreateLease
// instead.
const StaleCreatingTimeout = time.Minute

// NeverStartedCreateLease is how long a pending-create claim that never
// recorded a provider start (no last_woke_at) stays leased, measured from the
// attempt's anchor. It is deliberately longer than StaleCreatingTimeout: a
// never-started create may sit behind a busy pool start queue.
const NeverStartedCreateLease = 10 * time.Minute

// StaleKeyDetectDelay is the immutable production window between a keyed
// session start and the liveness probe that detects a stale resume key. The
// in-flight lease window is derived from it, so the lease and the start
// sequence cannot drift apart.
const StaleKeyDetectDelay = 2 * time.Second

// createStartInFlightSlack is the margin added to the startup timeout and
// StaleKeyDetectDelay when judging whether a provider start is still in
// flight.
const createStartInFlightSlack = 5 * time.Second

// AbandonedCreate describes a session whose in-flight create has been
// abandoned: the create is mid-flight by state, no live lease protects it, and
// its current attempt is stale.
type AbandonedCreate struct {
	// ID is the session bead id.
	ID string
	// Template is the trimmed Info.Template; empty on a damaged bead.
	Template string
	// State is the raw trimmed lifecycle state: creating or start-pending.
	State State
	// Since is the start of the current create attempt (see
	// CreateAttemptAnchor); zero when the bead carries no anchor.
	Since time.Time
}

// CreateAttemptAnchor returns the start of info's current create attempt: the
// parsed pending_create_started_at marker, else the bead's creation time. The
// zero time means no anchor.
func CreateAttemptAnchor(info Info) time.Time {
	if started, ok := parseCreateTimestamp(info.PendingCreateStartedAt); ok {
		return started
	}
	return info.CreatedAt
}

// CreateAttemptStale reports whether info's current create attempt has aged
// past StaleCreatingTimeout at now. The attempt is stale AT the boundary, a
// zero anchor is stale, and a now before the anchor (clock skew) is not. It
// reads no clock: now is the caller's, so a pure caller stays pure.
func CreateAttemptStale(info Info, now time.Time) bool {
	return attemptStale(CreateAttemptAnchor(info), now)
}

// DetectAbandonedCreate reports whether info is an abandoned in-flight create
// at now, and describes it when so. It is a pure function of info, now and the
// startup timeout: no store, config or clock. A closed row is never abandoned.
// A zero now is judged against the wall clock; this is a detector and fails
// toward reporting. startupTimeout <= 0 defaults to one minute.
//
// A row is abandoned when it is in flight (creating or start-pending), no
// pending-create lease protects it, and its attempt is stale. The lease holds
// while the provider start is in flight (last_woke_at plus the startup window),
// while a never-started claim is younger than NeverStartedCreateLease, and,
// for a claim that did start but is no longer in flight, only while the attempt
// is not stale.
func DetectAbandonedCreate(info Info, now time.Time, startupTimeout time.Duration) (AbandonedCreate, bool) {
	if info.Closed {
		return AbandonedCreate{}, false
	}
	state := State(strings.TrimSpace(info.MetadataState))
	if state != StateCreating && state != StateStartPending {
		return AbandonedCreate{}, false
	}
	now = resolveNow(now)
	anchor := CreateAttemptAnchor(info)
	if createLeaseActive(info, anchor, now, startupTimeout) || !attemptStale(anchor, now) {
		return AbandonedCreate{}, false
	}
	return AbandonedCreate{
		ID:       info.ID,
		Template: strings.TrimSpace(info.Template),
		State:    state,
		Since:    anchor,
	}, true
}

// DetectAbandonedCreates returns every abandoned create in infos, oldest Since
// first with ties broken by ID. A row with no anchor (zero Since) sorts first.
// infos is not modified.
func DetectAbandonedCreates(infos []Info, now time.Time, startupTimeout time.Duration) []AbandonedCreate {
	now = resolveNow(now)
	var out []AbandonedCreate
	for _, info := range infos {
		if a, ok := DetectAbandonedCreate(info, now, startupTimeout); ok {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b AbandonedCreate) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// createLeaseActive reports whether a pending-create claim still holds a live
// lease at now. Each branch keeps the inclusivity of the reconciler's leaf
// rules: the in-flight window ends before last_woke_at+window, the
// never-started lease is still held AT anchor+NeverStartedCreateLease.
func createLeaseActive(info Info, anchor, now time.Time, startupTimeout time.Duration) bool {
	if !info.PendingCreateClaim {
		return false
	}
	lastWoke := strings.TrimSpace(info.LastWokeAt)
	if lastWoke == "" {
		if anchor.IsZero() {
			return false
		}
		return !now.After(anchor.Add(NeverStartedCreateLease))
	}
	if startedAt, err := time.Parse(time.RFC3339, lastWoke); err == nil {
		if startupTimeout <= 0 {
			startupTimeout = time.Minute
		}
		if now.Before(startedAt.Add(startupTimeout + StaleKeyDetectDelay + createStartInFlightSlack)) {
			return true
		}
	}
	return !attemptStale(anchor, now)
}

// attemptStale reports whether an attempt anchored at anchor is stale at now.
func attemptStale(anchor, now time.Time) bool {
	if anchor.IsZero() {
		return true
	}
	return !now.Before(anchor.Add(StaleCreatingTimeout))
}

// resolveNow substitutes the wall clock for a zero now.
func resolveNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now()
	}
	return now
}

// parseCreateTimestamp parses an RFC3339 metadata value. A missing, zero or
// unparseable value returns ok=false so the caller falls back to the next
// anchor instead of protecting an unanchored create indefinitely.
func parseCreateTimestamp(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}
