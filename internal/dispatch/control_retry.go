package dispatch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// DefaultSemanticRetryBudget bounds how long the control dispatcher keeps
// re-asking a store that has already answered "no". At the measured ~5s per
// attempt it allows roughly 180 retries — ample for the sibling-close race the
// classification was originally written for, and 2.7% of the 6,712 futile
// retries a single stuck bead actually burned before this bound existed.
const DefaultSemanticRetryBudget = 15 * time.Minute

// maxControllerRetryErrorMetadata caps the recorded refusal text. Repeat
// detection survives truncation because truncateControllerRetryReason re-trims
// after cutting and RecordSemanticControlRetry reads the stored reason back
// through strings.TrimSpace, so a cut that lands on trailing whitespace is
// normalized on both the write and the read side. It does not depend on this
// value matching controlQuarantineReason's cap: that helper does not re-trim,
// and its quarantine record is terminal (never re-read for repeat detection).
const maxControllerRetryErrorMetadata = 512

// SemanticRetryState is the outcome of recording one Tier-B control failure.
type SemanticRetryState struct {
	// FirstSeen is the persisted deadline anchor: the instant of the FIRST
	// semantic refusal recorded for this bead, read back from the store rather
	// than remembered in the dispatcher.
	FirstSeen time.Time
	// Attempts counts the semantic refusals recorded for this bead, including
	// the one just recorded. Diagnostics only.
	Attempts int
	// Expired reports that the budget elapsed and the caller must escalate.
	Expired bool
	// Repeat reports that this refusal is textually identical to the one
	// already on the bead — the caller uses it to keep a permanently-stuck
	// bead from resetting the dispatcher's idle backoff on every sweep.
	Repeat bool
}

// RecordSemanticControlRetry records one Tier-B (semantic-refusal) control
// failure on the bead and reports whether its persisted budget is exhausted.
//
// The deadline anchor (gc.controller_retry_first_seen) is written on the FIRST
// refusal and never re-stamped. That is the entire reason it lives on the bead:
// the control dispatcher restarted five times during the outage this bounds,
// and an in-process counter would have reset on every one of them. Nothing
// extends the deadline — not a restart, not a change in the refusal text (the
// blocker list inside the message shifts as unrelated siblings close, and
// re-anchoring on that would hand back the unbounded retry through the back
// door).
//
// A negative budget disables the bound (unbounded retry, pre-tier behavior); a
// zero budget escalates on the first refusal.
func RecordSemanticControlRetry(store beads.Store, beadID string, cause error, now time.Time, budget time.Duration) (SemanticRetryState, error) {
	return recordControlRetry(store, beadID, cause, now, budget, semanticRetryKeys, "semantic retry")
}

// RecordPendingControlRetry records one ErrControlPending sweep against a
// SEPARATE bead-persisted budget and reports whether it is exhausted.
//
// Pending retry stays unbounded by design: the refusal names a config/state
// drift a human can heal (`gc rig add`, restoring a city name, returning a
// store), and closing the bead would destroy the very handle the heal completes
// through. What the budget bounds is the SILENCE. An unbounded answering-no with
// no escalation reads as healthy on every metric — the exact shape whose
// three-day, six-city stall is recorded on ControllerErrorTier — so the caller
// escalates once at expiry and keeps retrying.
//
// The pending keys deliberately do not alias the gc.controller_* ones. Sharing
// the anchor would let a long pending wait hand a later Tier-B refusal an
// already-expired deadline, quarantining on its first refusal the bead that
// pending kept open.
func RecordPendingControlRetry(store beads.Store, beadID string, cause error, now time.Time, budget time.Duration) (SemanticRetryState, error) {
	return recordControlRetry(store, beadID, cause, now, budget, pendingRetryKeys, "pending retry")
}

// controlRetryKeys names one disposition's bead-persisted retry budget.
type controlRetryKeys struct {
	reason    string
	count     string
	firstSeen string
	// extra is stamped verbatim alongside the budget on every record. The
	// semantic disposition uses it to keep the controller-error class/retryable
	// pair in step with the reason it records; pending has no such pair.
	extra map[string]string
}

var semanticRetryKeys = controlRetryKeys{
	reason:    beadmeta.ControllerErrorMetadataKey,
	count:     beadmeta.ControllerRetryCountMetadataKey,
	firstSeen: beadmeta.ControllerRetryFirstSeenMetadataKey,
	extra: map[string]string{
		beadmeta.ControllerErrorClassMetadataKey: beadmeta.FailureClassTransient,
		beadmeta.ControllerRetryableMetadataKey:  "true",
	},
}

var pendingRetryKeys = controlRetryKeys{
	reason:    beadmeta.ControlPendingReasonMetadataKey,
	count:     beadmeta.ControlPendingCountMetadataKey,
	firstSeen: beadmeta.ControlPendingFirstSeenMetadataKey,
}

func recordControlRetry(store beads.Store, beadID string, cause error, now time.Time, budget time.Duration, keys controlRetryKeys, label string) (SemanticRetryState, error) {
	bead, err := store.Get(beadID)
	if err != nil {
		return SemanticRetryState{}, fmt.Errorf("reading control bead %s for its retry budget: %w", beadID, err)
	}

	reason := truncateControllerRetryReason(cause)
	state := SemanticRetryState{
		Attempts: parseRetryCount(bead.Metadata[keys.count]) + 1,
	}

	metadata := map[string]string{
		keys.reason: reason,
		keys.count:  strconv.Itoa(state.Attempts),
	}
	for key, value := range keys.extra {
		metadata[key] = value
	}

	firstSeen, anchored := parseRetryFirstSeen(bead.Metadata[keys.firstSeen])
	if anchored {
		state.Repeat = strings.TrimSpace(bead.Metadata[keys.reason]) == reason
	} else {
		firstSeen = now
		metadata[keys.firstSeen] = firstSeen.UTC().Format(time.RFC3339)
	}
	state.FirstSeen = firstSeen

	if err := store.SetMetadataBatch(beadID, metadata); err != nil {
		return SemanticRetryState{}, fmt.Errorf("recording the %s budget on %s: %w", label, beadID, err)
	}

	state.Expired = budget >= 0 && !now.Before(firstSeen.Add(budget))
	return state, nil
}

func parseRetryFirstSeen(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, trimmed)
	if err != nil {
		// An unparseable anchor is re-anchored rather than trusted: a bead that
		// cannot say when it started failing gets a fresh, well-formed budget
		// instead of an immortal or an instantly-expired one.
		return time.Time{}, false
	}
	return parsed, true
}

func parseRetryCount(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func truncateControllerRetryReason(cause error) string {
	reason := ""
	if cause != nil {
		reason = strings.TrimSpace(cause.Error())
	}
	if len(reason) <= maxControllerRetryErrorMetadata {
		return reason
	}
	limit := maxControllerRetryErrorMetadata
	for limit > 0 && !utf8.ValidString(reason[:limit]) {
		limit--
	}
	// Re-trim after truncation so the stored reason and a freshly-recomputed one
	// stay byte-identical: the repeat check reads the stored value back through
	// strings.TrimSpace, so a cut that lands on trailing whitespace would
	// otherwise defeat repeat detection for a >maxControllerRetryErrorMetadata
	// refusal.
	return strings.TrimSpace(reason[:limit])
}

// quietControllerRetryError marks a transient failure that repeats the previous
// attempt's failure verbatim. It preserves both the message and the errors.Is
// chain so every existing classifier keeps seeing the original error.
type quietControllerRetryError struct{ err error }

func (e *quietControllerRetryError) Error() string { return e.err.Error() }

func (e *quietControllerRetryError) Unwrap() error { return e.err }

// MarkQuietControllerRetry marks err as a verbatim repeat of the failure this
// control bead already reported.
//
// The control dispatcher treats any pending or transient outcome as activity
// and resets its idle backoff, so two permanently-stuck beads pinned the whole
// loop at its 1s floor and consumed 95% of one city's control dispatches.
// A quiet retry is deliberately NOT activity: the bead is still retried on
// every sweep, but it no longer holds the dispatcher at full spin.
func MarkQuietControllerRetry(err error) error {
	if err == nil || IsQuietControllerRetry(err) {
		return err
	}
	return &quietControllerRetryError{err: err}
}

// IsQuietControllerRetry reports whether err was marked as a verbatim repeat.
func IsQuietControllerRetry(err error) bool {
	var quiet *quietControllerRetryError
	return errors.As(err, &quiet)
}
