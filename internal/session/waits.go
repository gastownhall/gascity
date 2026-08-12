package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// WaitBeadType identifies durable wait beads associated with sessions.
	WaitBeadType = "gate"
	// LegacyWaitBeadType is the historical durable wait bead type kept for
	// backward-compatible reads against older stores.
	LegacyWaitBeadType = "wait"
	// WaitBeadLabel is the common label used to locate session wait beads.
	WaitBeadLabel = "gc:wait"

	waitStateClosed   = "closed"
	waitStateCanceled = "canceled"
	waitStateExpired  = "expired"
	waitStateFailed   = "failed"

	// SessionWaitLookupLimit bounds per-session wait bead lookups.
	SessionWaitLookupLimit = 1000
)

// WakeConflictError reports a lifecycle state that cannot accept an explicit
// wake request.
type WakeConflictError struct {
	SessionID string
	State     string
}

func (e *WakeConflictError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("session %s is %s", e.SessionID, e.State)
}

// WakeConflictState extracts the conflicting lifecycle state from err.
func WakeConflictState(err error) (string, bool) {
	var conflict *WakeConflictError
	if errors.As(err, &conflict) && conflict != nil {
		return conflict.State, true
	}
	return "", false
}

// IsWaitTerminalState reports whether a durable wait has reached a terminal lifecycle state.
func IsWaitTerminalState(state string) bool {
	switch state {
	case waitStateClosed, waitStateCanceled, waitStateExpired, waitStateFailed:
		return true
	default:
		return false
	}
}

// IsWaitBeadType reports whether the bead type is recognized as a durable
// session wait.
func IsWaitBeadType(typ string) bool {
	switch typ {
	case WaitBeadType, LegacyWaitBeadType:
		return true
	default:
		return false
	}
}

// IsWaitBead reports whether a bead is a durable session wait. New waits are
// stored as gate beads, while legacy stores may still contain type "wait".
func IsWaitBead(b beads.Bead) bool {
	if !IsWaitBeadType(b.Type) {
		return false
	}
	if beadHasLabel(b, WaitBeadLabel) {
		return true
	}
	sessionID := b.Metadata["session_id"]
	return sessionID != "" && beadHasLabel(b, "session:"+sessionID)
}

// WaitInfo is the typed projection of a durable session wait bead: the domain
// view of a wait that callers read and decide against without touching
// *beads.Bead. It carries only bead-stored facts (metadata keys, description,
// status, created-at, labels), so a wait bead round-trips to the same WaitInfo
// regardless of which backend stored it.
//
// Bead serialization for waits is confined to this file: WaitInfoFromBead is the
// only place the wait-read paths learn these facts come from a bead. The wait
// write paths (metadata batches, retry clones, create) still speak *beads.Bead —
// that is the deliberate serialization edge, mirroring session.Store.
type WaitInfo struct {
	// ID is the wait bead ID.
	ID string
	// SessionID is the session bead ID the wait is registered against (metadata session_id).
	SessionID string
	// SessionName is the runtime session name recorded at registration (metadata session_name).
	SessionName string
	// Kind is the wait kind, e.g. "deps" or "probe" (metadata kind).
	Kind string
	// State is the wait lifecycle state, e.g. "pending"/"ready" (metadata state).
	State string
	// DepIDs are the dependency bead IDs the wait watches, comma-split and
	// trimmed with empties dropped (metadata dep_ids). It is nil when unset.
	DepIDs []string
	// DepMode is "all" or "any" (metadata dep_mode).
	DepMode string
	// RegisteredEpoch is the session continuation epoch at registration (metadata registered_epoch).
	RegisteredEpoch string
	// DeliveryAttempt is the current delivery attempt counter (metadata delivery_attempt).
	DeliveryAttempt string
	// NudgeID is the shadow wait-nudge ID once dispatched (metadata nudge_id).
	NudgeID string
	// ExpiresAt is the raw RFC3339 expiry string kept verbatim; consumers parse
	// it and tolerate malformed values (metadata expires_at).
	ExpiresAt string
	// Note is the reminder text delivered when the wait is satisfied (bead Description, untrimmed).
	Note string
	// Status is the persisted bead status ("open"/"closed").
	Status string
	// CreatedAt is the bead creation time.
	CreatedAt time.Time
	// Labels are the bead labels.
	Labels []string
}

// WaitInfoFromBead projects a durable wait bead onto WaitInfo. It is pure,
// side-effect-free, and backend-invariant: it reads only stored bead fields and
// applies the same key-for-key decoding (and dep_ids split/trim) the wait render
// and decision paths previously performed inline.
func WaitInfoFromBead(b beads.Bead) WaitInfo {
	return WaitInfo{
		ID:              b.ID,
		SessionID:       b.Metadata["session_id"],
		SessionName:     b.Metadata["session_name"],
		Kind:            b.Metadata["kind"],
		State:           b.Metadata["state"],
		DepIDs:          splitWaitDepIDs(b.Metadata["dep_ids"]),
		DepMode:         b.Metadata["dep_mode"],
		RegisteredEpoch: b.Metadata["registered_epoch"],
		DeliveryAttempt: b.Metadata["delivery_attempt"],
		NudgeID:         b.Metadata["nudge_id"],
		ExpiresAt:       b.Metadata["expires_at"],
		Note:            b.Description,
		Status:          b.Status,
		CreatedAt:       b.CreatedAt,
		Labels:          b.Labels,
	}
}

// splitWaitDepIDs splits a comma-separated dep_ids value into trimmed, non-empty
// IDs, returning nil for a blank value. It is the confined codec for the wait
// dependency-ID list (formerly cmd/gc's splitWaitIDs).
func splitWaitDepIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// StampWaitLookupCapMetadata adds the shared durable wait lookup cap
// diagnostic metadata to batch.
func StampWaitLookupCapMetadata(batch map[string]string, label string, limit int, now time.Time, source string) {
	if batch == nil {
		return
	}
	if source == "" {
		source = "wait-lookup"
	}
	batch["wait_lookup_capped_at"] = now.UTC().Format(time.RFC3339)
	batch["wait_lookup_capped_label"] = label
	batch["wait_lookup_capped_limit"] = strconv.Itoa(limit)
	batch["wait_lookup_capped_source"] = source
}

func beadHasLabel(b beads.Bead, want string) bool {
	return labelsContain(b.Labels, want)
}

func labelsContain(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}
