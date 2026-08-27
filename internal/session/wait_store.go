package session

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file extends the session-class domain wrapper (Store) with the durable
// wait sub-surface. Reads project a wait bead onto WaitInfo (via
// WaitInfoFromBead, the codec confined to waits.go); writes speak typed intents
// so bead serialization — the metadata batches, the terminal Close, the retry
// clone+Create — is confined here instead of leaking into cmd/gc business logic.
//
// Every method follows the front-door error convention established by
// ApplyPatch: store errors are returned BARE so callers own their diagnostic
// text (several CLI/HTTP tests pin exact stderr and status text). The domain
// guards below add typed sentinels (ErrNotAWait / ErrNotSessionBead) that
// callers match to render their own messages.

const (
	waitStatePending = "pending"
	waitStateReady   = "ready"
)

// ErrNotAWait reports that a bead exists but is not a durable session wait. It
// wraps the offending id so callers (e.g. gc wait inspect / the blocked-nudge
// gate) can render their own "X is not a wait" text.
var ErrNotAWait = errors.New("not a wait")

// ErrNotSessionBead reports that a bead exists but is not a session bead (nor a
// repairable one). WakeSession returns it so callers render their own
// "X is not a session" text / 400 status.
var ErrNotSessionBead = errors.New("not a session bead")

// GetWait returns the WaitInfo projection of a durable wait bead. A missing bead
// passes the bare store error through (errors.Is(err, beads.ErrNotFound) keeps
// working); a bead that is not a durable wait returns an error wrapping
// ErrNotAWait carrying the id.
func (s *Store) GetWait(id string) (WaitInfo, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return WaitInfo{}, err
	}
	if !IsWaitBead(b) {
		return WaitInfo{}, fmt.Errorf("%w: %s", ErrNotAWait, id)
	}
	return WaitInfoFromBead(b), nil
}

// WaitReadyClaim is the result of a pending-to-ready claim. Wait and Persisted
// carry the newest exact observation when another writer canceled, rebound, or
// otherwise advanced the wait; Outcome makes the mutation boundary explicit.
type WaitReadyClaim struct {
	Wait      WaitInfo
	Persisted WaitPersistedResponse
	Outcome   WaitReadyClaimOutcome
}

// WaitReadyClaimOutcome classifies whether ClaimPendingWaitReady reached its
// mutation boundary. Callers may yield to legacy processing only for
// WaitReadyClaimNotApplied; ambiguous outcomes retain keyed ownership.
type WaitReadyClaimOutcome string

const (
	// WaitReadyClaimNotApplied proves the conditional mutation did not apply.
	WaitReadyClaimNotApplied WaitReadyClaimOutcome = "not_applied"
	// WaitReadyClaimCommitted proves this controller's exact ready claim applied.
	WaitReadyClaimCommitted WaitReadyClaimOutcome = "committed"
	// WaitReadyClaimAmbiguous means a conditional attempt may have mutated the row
	// but the authoritative reread cannot prove the operation's final outcome.
	WaitReadyClaimAmbiguous WaitReadyClaimOutcome = "ambiguous"
)

// WaitPersistedResponse carries the opaque whole-row revision from the same
// durable read as a WaitInfo. It is internal and off-wire; zero means the
// backend did not supply a revision.
type WaitPersistedResponse struct {
	Revision int64 `json:"-"`
}

// WaitReadyOwner identifies the controller protocol allowed to claim a
// dependency-ready wait. Keeping this typed prevents another controller's
// ownership marker from being accidentally adopted during crash recovery.
type WaitReadyOwner string

const (
	// WaitReadyOwnerDependency is the sole owner for dependency-ready claims.
	WaitReadyOwnerDependency WaitReadyOwner = "dependency-ready"
)

// GetWaitPersistedResponse loads one durable wait and its revision from one
// same-read observation. The revision is only a precondition token for
// ClaimPendingWaitReady; it never crosses a wire. Missing and non-wait error
// behavior matches GetWait. Callers that require an authoritative read must
// use the store's live handle before entering this domain surface.
func (s *Store) GetWaitPersistedResponse(id string) (WaitInfo, WaitPersistedResponse, error) {
	if s == nil || s.store.Store == nil {
		return WaitInfo{}, WaitPersistedResponse{}, beads.ErrNotFound
	}
	b, err := s.store.Get(id)
	if err != nil {
		return WaitInfo{}, WaitPersistedResponse{}, err
	}
	if !IsWaitBead(b) {
		return WaitInfo{}, WaitPersistedResponse{}, fmt.Errorf("%w: %s", ErrNotAWait, id)
	}
	return WaitInfoFromBead(b), WaitPersistedResponse{Revision: b.Revision}, nil
}

// ClaimPendingWaitReady conditionally moves an exact open pending wait to
// ready. The caller supplies an operation token unique to its controller
// attempt; it is durably recorded with the deterministic ready timestamp so a
// later controller can reconstruct whether that exact operation won.
//
// The conditional write preserves all metadata not owned by this transition.
// Every attempted write is followed by an authoritative exact reread: a stale
// revision returns the new canceled/rebound/newer observation without
// overwriting it, while a transport response lost after a committed write is
// recognized as this caller's successful claim. If the write error cannot be
// classified from the reread, Outcome is WaitReadyClaimAmbiguous and the
// caller must retain keyed ownership.
func (s *Store) ClaimPendingWaitReady(candidate WaitInfo, persisted WaitPersistedResponse, now time.Time, owner WaitReadyOwner, operation string) (WaitReadyClaim, error) {
	result := WaitReadyClaim{Wait: candidate, Persisted: persisted, Outcome: WaitReadyClaimNotApplied}
	if owner != WaitReadyOwnerDependency {
		return result, fmt.Errorf("claiming pending wait: unsupported ready owner %q", owner)
	}
	if operation == "" || strings.TrimSpace(operation) != operation || strings.ContainsAny(operation, " \t\r\n") {
		return result, fmt.Errorf("claiming pending wait: ready operation must be non-empty and canonical")
	}
	if candidate.ID == "" || persisted.Revision == 0 {
		return result, fmt.Errorf("claiming pending wait: exact wait ID and revision are required")
	}
	if candidate.Status != "open" || candidate.State != waitStatePending {
		return result, nil
	}
	if s == nil || s.store.Store == nil {
		return result, fmt.Errorf("claiming pending wait %q: %w", candidate.ID, beads.ErrConditionalWriteUnsupported)
	}

	writer, _, resolveErr := beads.ResolveConditionalWriter(s.store)
	if resolveErr != nil {
		return result, fmt.Errorf("claiming pending wait %q: resolving conditional writer: %w", candidate.ID, resolveErr)
	}
	if writer == nil {
		return result, fmt.Errorf("claiming pending wait %q: %w", candidate.ID, beads.ErrConditionalWriteUnsupported)
	}
	readyAt := now.UTC().Format(time.RFC3339)
	writeErr := writer.UpdateIfMatch(candidate.ID, persisted.Revision, beads.UpdateOpts{Metadata: map[string]string{
		"state":           waitStateReady,
		"ready_at":        readyAt,
		"ready_owner":     string(owner),
		"ready_operation": operation,
	}})

	afterBead, readErr := beads.HandlesFor(s.store.Store).Live.Get(candidate.ID)
	if readErr != nil {
		result.Outcome = classifyWaitReadyClaimReadFailure(writeErr)
		if result.Outcome == WaitReadyClaimNotApplied {
			return result, fmt.Errorf("confirming rejected pending wait claim %q: %w", candidate.ID, readErr)
		}
		return result, fmt.Errorf("claiming pending wait %q: authoritative reread after conditional attempt: %w", candidate.ID, readErr)
	}
	if !IsWaitBead(afterBead) {
		if beads.IsPreconditionFailed(writeErr) {
			return result, nil
		}
		result.Outcome = WaitReadyClaimAmbiguous
		return result, fmt.Errorf("confirming pending wait claim %q: %w", candidate.ID, ErrNotAWait)
	}
	after, afterPersisted := WaitInfoFromBead(afterBead), WaitPersistedResponse{Revision: afterBead.Revision}
	result = WaitReadyClaim{Wait: after, Persisted: afterPersisted}
	result.Outcome = classifyWaitReadyClaim(after, afterPersisted, candidate, persisted, readyAt, owner, operation, writeErr)
	switch result.Outcome {
	case WaitReadyClaimCommitted:
		return result, nil
	case WaitReadyClaimNotApplied:
		if beads.IsPreconditionFailed(writeErr) {
			return result, nil
		}
		return result, fmt.Errorf("claiming pending wait %q: %w", candidate.ID, writeErr)
	default:
		return result, fmt.Errorf("confirming pending wait claim %q: outcome is ambiguous after conditional attempt", candidate.ID)
	}
}

func classifyWaitReadyClaim(after WaitInfo, afterPersisted WaitPersistedResponse, candidate WaitInfo, persisted WaitPersistedResponse, readyAt string, owner WaitReadyOwner, operation string, writeErr error) WaitReadyClaimOutcome {
	if waitReadyClaimMatches(after, afterPersisted, candidate, persisted, readyAt, owner, operation) {
		return WaitReadyClaimCommitted
	}
	if beads.IsPreconditionFailed(writeErr) {
		return WaitReadyClaimNotApplied
	}
	if writeErr != nil && afterPersisted.Revision == persisted.Revision && reflect.DeepEqual(after, candidate) {
		return WaitReadyClaimNotApplied
	}
	return WaitReadyClaimAmbiguous
}

func classifyWaitReadyClaimReadFailure(writeErr error) WaitReadyClaimOutcome {
	if beads.IsPreconditionFailed(writeErr) {
		return WaitReadyClaimNotApplied
	}
	return WaitReadyClaimAmbiguous
}

func waitReadyClaimMatches(after WaitInfo, afterPersisted WaitPersistedResponse, candidate WaitInfo, persisted WaitPersistedResponse, readyAt string, owner WaitReadyOwner, operation string) bool {
	return afterPersisted.Revision != 0 && afterPersisted.Revision != persisted.Revision &&
		waitRegistrationMatches(after, candidate) &&
		after.Status == "open" &&
		after.State == waitStateReady &&
		after.ReadyAt == readyAt &&
		after.ReadyOwner == string(owner) &&
		after.ReadyOperation == operation
}

func waitRegistrationMatches(after, candidate WaitInfo) bool {
	if after.ID != candidate.ID || after.SessionID != candidate.SessionID ||
		after.Kind != candidate.Kind || after.DepMode != candidate.DepMode ||
		after.RegisteredEpoch != candidate.RegisteredEpoch || after.ExpiresAt != candidate.ExpiresAt {
		return false
	}
	return reflect.DeepEqual(after.DepIDs, candidate.DepIDs)
}

// WaitsForSession returns the WaitInfo projection of open durable wait beads for
// one session, located via the "session:<id>" label, created DESC and capped at
// SessionWaitLookupLimit. When the lookup is capped it returns the partial slice
// plus a beads.LookupLimitError.
//
// PartialResultError semantics mirror ListAllSessionBeads: a degraded-but-non-empty
// store read (some rows parsed, some skipped) still projects the returned rows and
// folds the beads.PartialResultError through as the returned error, so callers that
// can render a degraded view (the /waits handler, the CLI fallback) keep the
// reachable waits instead of dropping them. A hard (non-partial) store error still
// short-circuits with nil rows.
func (s *Store) WaitsForSession(sessionID string) ([]WaitInfo, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	waits, err := s.store.List(beads.ListQuery{
		Status: "open",
		Label:  "session:" + sessionID,
		Limit:  SessionWaitLookupLimit + 1,
		Sort:   beads.SortCreatedDesc,
	})
	if err != nil && !beads.IsPartialResult(err) {
		return nil, err
	}
	partialErr := err
	capped := len(waits) > SessionWaitLookupLimit
	if capped {
		waits = waits[:SessionWaitLookupLimit]
	}
	result := make([]WaitInfo, 0, len(waits))
	for _, wait := range waits {
		if !IsWaitBead(wait) {
			continue
		}
		if wait.Metadata["session_id"] != sessionID {
			continue
		}
		result = append(result, WaitInfoFromBead(wait))
	}
	if capped {
		return result, beads.LookupLimitError{Kind: "wait", Label: "session:" + sessionID, Limit: SessionWaitLookupLimit}
	}
	return result, partialErr
}

// ListWaits returns durable waits. When sessionID is set it delegates to
// WaitsForSession (label-scoped); otherwise it scans the global gc:wait label
// (closed excluded, IsWaitBead-filtered, created DESC, capped). A non-empty
// state filters the projected waits in memory. The result is DESC — callers that
// need a stable tie order apply their own sort. A capped lookup returns the
// partial slice plus a beads.LookupLimitError; a degraded store read returns the
// surviving rows plus a beads.PartialResultError (both folded through from the
// delegate).
func (s *Store) ListWaits(state, sessionID string) ([]WaitInfo, error) {
	var (
		waits []WaitInfo
		err   error
	)
	if strings.TrimSpace(sessionID) != "" {
		waits, err = s.WaitsForSession(sessionID)
	} else {
		waits, err = s.listWaitsByLabel()
	}
	if state == "" {
		return waits, err
	}
	filtered := make([]WaitInfo, 0, len(waits))
	for _, wait := range waits {
		if wait.State == state {
			filtered = append(filtered, wait)
		}
	}
	return filtered, err
}

// listWaitsByLabel is the global gc:wait scan behind ListWaits: closed beads are
// excluded, non-wait beads filtered, results are DESC and capped at
// SessionWaitLookupLimit with a LookupLimitError on overflow. A degraded store
// read folds its beads.PartialResultError through alongside the surviving rows
// (same fold-through as WaitsForSession); a hard error returns nil rows.
func (s *Store) listWaitsByLabel() ([]WaitInfo, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	all, err := s.store.List(beads.ListQuery{
		Label: WaitBeadLabel,
		Limit: SessionWaitLookupLimit + 1,
		Sort:  beads.SortCreatedDesc,
	})
	if err != nil && !beads.IsPartialResult(err) {
		return nil, err
	}
	partialErr := err
	capped := len(all) > SessionWaitLookupLimit
	if capped {
		all = all[:SessionWaitLookupLimit]
	}
	result := make([]WaitInfo, 0, len(all))
	for _, item := range all {
		if item.Status == "closed" {
			continue
		}
		if !IsWaitBead(item) {
			continue
		}
		result = append(result, WaitInfoFromBead(item))
	}
	if capped {
		return result, beads.LookupLimitError{Kind: "wait", Label: WaitBeadLabel, Limit: SessionWaitLookupLimit}
	}
	return result, partialErr
}

// WaitNudgeIDs returns the deduplicated queued nudge IDs for the session's
// currently open waits.
func (s *Store) WaitNudgeIDs(sessionID string) ([]string, error) {
	waits, err := s.WaitsForSession(sessionID)
	if err != nil && !beads.IsLookupLimitError(err) {
		return nil, err
	}
	ids := make([]string, 0, len(waits))
	seen := make(map[string]bool, len(waits))
	for _, wait := range waits {
		if wait.NudgeID == "" || seen[wait.NudgeID] {
			continue
		}
		seen[wait.NudgeID] = true
		ids = append(ids, wait.NudgeID)
	}
	return ids, err
}

// setWaitTerminalState is the terminal-write funnel: it stamps the batch then
// closes the wait bead, exactly the SetMetadataBatch+Close pair (in that order)
// the wait terminal writes performed inline. Store errors are returned bare.
func (s *Store) setWaitTerminalState(id string, batch map[string]string) error {
	if err := s.store.SetMetadataBatch(id, batch); err != nil {
		return err
	}
	return s.store.Close(id)
}

// CancelWait marks a wait canceled and closes it. lastError is recorded only
// when non-empty (matching the inline batches).
func (s *Store) CancelWait(id string, now time.Time, lastError string) error {
	batch := map[string]string{
		"state":       waitStateCanceled,
		"canceled_at": now.UTC().Format(time.RFC3339),
	}
	if lastError != "" {
		batch["last_error"] = lastError
	}
	return s.setWaitTerminalState(id, batch)
}

// ExpireWait marks a wait expired and closes it.
func (s *Store) ExpireWait(id string, now time.Time) error {
	return s.setWaitTerminalState(id, map[string]string{
		"state":      waitStateExpired,
		"expired_at": now.UTC().Format(time.RFC3339),
	})
}

// FailWait marks a wait failed with lastError and closes it.
func (s *Store) FailWait(id string, now time.Time, lastError string) error {
	return s.setWaitTerminalState(id, map[string]string{
		"state":      waitStateFailed,
		"failed_at":  now.UTC().Format(time.RFC3339),
		"last_error": lastError,
	})
}

// CloseWaitFromNudge closes a ready wait whose shadow nudge injected, recording
// the nudge id and commit boundary.
func (s *Store) CloseWaitFromNudge(id string, now time.Time, nudgeID, commitBoundary string) error {
	return s.setWaitTerminalState(id, map[string]string{
		"state":           waitStateClosed,
		"closed_at":       now.UTC().Format(time.RFC3339),
		"nudge_id":        nudgeID,
		"commit_boundary": commitBoundary,
	})
}

// FailWaitFromNudge fails a ready wait whose shadow nudge reached a terminal
// error, recording the terminal reason, nudge id and commit boundary.
func (s *Store) FailWaitFromNudge(id string, now time.Time, nudgeID, terminalReason, commitBoundary string) error {
	return s.setWaitTerminalState(id, map[string]string{
		"state":           waitStateFailed,
		"failed_at":       now.UTC().Format(time.RFC3339),
		"nudge_id":        nudgeID,
		"last_error":      terminalReason,
		"commit_boundary": commitBoundary,
	})
}

// MarkWaitReady stamps a wait ready without closing it (the dependency-satisfied
// paths). It emits a single SetMetadataBatch.
func (s *Store) MarkWaitReady(id string, now time.Time) error {
	return s.store.SetMetadataBatch(id, map[string]string{
		"state":           waitStateReady,
		"ready_at":        now.UTC().Format(time.RFC3339),
		"ready_owner":     "",
		"ready_operation": "",
	})
}

// MarkWaitReadyForRedelivery stamps a wait ready and, when nextAttempt is
// non-empty, bumps the delivery attempt and clears the eight terminal-bookkeeping
// keys so the wait can be re-dispatched. It emits a single SetMetadataBatch.
func (s *Store) MarkWaitReadyForRedelivery(id, nextAttempt string, now time.Time) error {
	batch := map[string]string{
		"state":           waitStateReady,
		"ready_at":        now.UTC().Format(time.RFC3339),
		"ready_owner":     "",
		"ready_operation": "",
	}
	if nextAttempt != "" {
		batch["delivery_attempt"] = nextAttempt
		batch["nudge_id"] = ""
		batch["commit_boundary"] = ""
		batch["last_error"] = ""
		batch["closed_at"] = ""
		batch["failed_at"] = ""
		batch["expired_at"] = ""
		batch["canceled_at"] = ""
	}
	return s.store.SetMetadataBatch(id, batch)
}

// SetWaitNudgeID records the shadow wait-nudge id on the wait bead. It emits a
// single-key SetMetadata (not a batch), byte-identical to the raw single-key
// write it replaces.
func (s *Store) SetWaitNudgeID(id, nudgeID string) error {
	return s.store.SetMetadata(id, "nudge_id", nudgeID)
}

// WaitSpec describes a durable dependency wait to register against a session.
type WaitSpec struct {
	// SessionID is the resolved session bead ID the wait registers against.
	SessionID string
	// Kind is the wait kind, e.g. "deps".
	Kind string
	// DepIDs are the dependency bead IDs the wait watches.
	DepIDs []string
	// DepMode is "all" or "any".
	DepMode string
	// Note is the reminder text delivered when the wait is satisfied (Description).
	Note string
	// CreatedBySession stamps the originating $GC_SESSION_ID.
	CreatedBySession string
	// Now is the registration time (callers pass time.Now().UTC()).
	Now time.Time
}

// CreateWait registers a durable wait bead for a session. It reads the session's
// persisted markers to build the wait title / session_name / registered_epoch,
// creates the bead with the canonical type, labels and pending metadata, and
// returns the WaitInfo projection of the created bead.
func (s *Store) CreateWait(spec WaitSpec) (WaitInfo, error) {
	markers, err := s.PersistedMarkers(spec.SessionID)
	if err != nil {
		return WaitInfo{}, err
	}
	meta := map[string]string{
		"session_id":         spec.SessionID,
		"session_name":       markers.SessionName,
		"kind":               spec.Kind,
		"state":              waitStatePending,
		"dep_ids":            strings.Join(spec.DepIDs, ","),
		"dep_mode":           spec.DepMode,
		"registered_epoch":   markers.ContinuationEpoch,
		"delivery_attempt":   "1",
		"created_by_session": spec.CreatedBySession,
		"created_at":         spec.Now.Format(time.RFC3339),
	}
	created, err := s.store.Create(beads.Bead{
		Title:       "wait:" + markers.Title,
		Type:        WaitBeadType,
		Description: spec.Note,
		Labels:      []string{WaitBeadLabel, "session:" + spec.SessionID},
		Metadata:    meta,
	})
	if err != nil {
		return WaitInfo{}, err
	}
	return WaitInfoFromBead(created), nil
}

// retryableWaitMetadata clones the carry-forward metadata for a wait retry. For
// deps waits it keeps only the registration-defining keys (dropping bookkeeping
// and unknown keys); for other kinds it keeps every non-empty key.
func retryableWaitMetadata(src map[string]string) map[string]string {
	if src["kind"] != "deps" {
		meta := make(map[string]string, len(src))
		for key, value := range src {
			if value == "" {
				continue
			}
			meta[key] = value
		}
		return meta
	}
	keys := []string{
		"session_id",
		"session_name",
		"kind",
		"dep_ids",
		"dep_mode",
		"registered_epoch",
		"created_by_session",
		"expires_at",
	}
	meta := make(map[string]string, len(keys)+8)
	for _, key := range keys {
		if value := src[key]; value != "" {
			meta[key] = value
		}
	}
	return meta
}

// RetryClosedWait re-registers a closed wait as a fresh ready wait. It gets the
// raw closed wait, clones its carry-forward metadata, applies the ready+clears
// block, refreshes registered_epoch / session_name from the session's persisted
// markers, and creates the replacement. nextAttempt is supplied by the caller
// (the nudges-class delivery-attempt read stays caller-side); an empty
// nextAttempt falls back to the wait's own delivery_attempt (default "1").
func (s *Store) RetryClosedWait(id, nextAttempt string, now time.Time) (WaitInfo, error) {
	wait, err := s.store.Get(id)
	if err != nil {
		return WaitInfo{}, err
	}
	w := WaitInfoFromBead(wait)
	if nextAttempt == "" {
		nextAttempt = w.DeliveryAttempt
		if nextAttempt == "" {
			nextAttempt = "1"
		}
	}
	nowStr := now.UTC().Format(time.RFC3339)
	meta := retryableWaitMetadata(wait.Metadata)
	meta["state"] = waitStateReady
	meta["ready_at"] = nowStr
	meta["delivery_attempt"] = nextAttempt
	meta["nudge_id"] = ""
	meta["commit_boundary"] = ""
	meta["last_error"] = ""
	meta["closed_at"] = ""
	meta["failed_at"] = ""
	meta["expired_at"] = ""
	meta["canceled_at"] = ""
	meta["created_at"] = nowStr
	meta["retried_from_wait"] = wait.ID
	if sessionID := w.SessionID; sessionID != "" {
		if markers, err := s.PersistedMarkers(sessionID); err == nil {
			if epoch := markers.ContinuationEpoch; epoch != "" {
				meta["registered_epoch"] = epoch
			}
			if meta["session_name"] == "" {
				meta["session_name"] = markers.SessionName
			}
		}
	}
	created, err := s.store.Create(beads.Bead{
		Title:       wait.Title,
		Type:        wait.Type,
		Description: wait.Description,
		Labels:      append([]string(nil), wait.Labels...),
		Metadata:    meta,
	})
	if err != nil {
		return WaitInfo{}, err
	}
	return WaitInfoFromBead(created), nil
}

// CancelWaits marks all non-terminal waits for the session canceled (closing the
// terminal ones idempotently) and returns every queued wait-nudge ID discovered
// across capped lookup pages, plus whether any lookup page was capped.
func (s *Store) CancelWaits(sessionID string, now time.Time) (nudgeIDs []string, capped bool, err error) {
	return s.cancelWaitsAndCollectNudgeIDs(sessionID, now)
}

func (s *Store) cancelWaitsAndCollectNudgeIDs(sessionID string, now time.Time) ([]string, bool, error) {
	ids := []string(nil)
	seen := map[string]bool{}
	capped := false
	canceledMetadata := map[string]string{
		"state":       waitStateCanceled,
		"canceled_at": now.UTC().Format(time.RFC3339),
	}
	for {
		waits, err := s.WaitsForSession(sessionID)
		if err != nil && !beads.IsLookupLimitError(err) {
			return ids, capped, err
		}
		lookupCapped := beads.IsLookupLimitError(err)
		capped = capped || lookupCapped
		cancelIDs := make([]string, 0, len(waits))
		terminalIDs := make([]string, 0, len(waits))
		for _, wait := range waits {
			if wait.NudgeID != "" && !seen[wait.NudgeID] {
				seen[wait.NudgeID] = true
				ids = append(ids, wait.NudgeID)
			}
			if IsWaitTerminalState(wait.State) {
				terminalIDs = append(terminalIDs, wait.ID)
				continue
			}
			cancelIDs = append(cancelIDs, wait.ID)
		}
		if len(cancelIDs) > 0 {
			if _, err := s.store.CloseAll(cancelIDs, canceledMetadata); err != nil {
				return ids, capped, err
			}
		}
		if len(terminalIDs) > 0 {
			if _, err := s.store.CloseAll(terminalIDs, nil); err != nil {
				return ids, capped, err
			}
		}
		canceled := len(cancelIDs) + len(terminalIDs)
		if !lookupCapped {
			return ids, capped, nil
		}
		if canceled == 0 {
			return ids, capped, err
		}
	}
}

// ReassignWaits moves open non-terminal waits from one session bead ID to another
// during canonical session repair, closing terminal waits it encounters.
func (s *Store) ReassignWaits(oldSessionID, newSessionID string) error {
	if s == nil || s.store.Store == nil {
		return nil
	}
	oldSessionID = strings.TrimSpace(oldSessionID)
	newSessionID = strings.TrimSpace(newSessionID)
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID {
		return nil
	}
	oldLabel := "session:" + oldSessionID
	newLabel := "session:" + newSessionID
	for {
		waits, err := s.WaitsForSession(oldSessionID)
		if err != nil && !beads.IsLookupLimitError(err) {
			return err
		}
		lookupCapped := beads.IsLookupLimitError(err)
		progressed := 0
		for _, wait := range waits {
			if IsWaitTerminalState(wait.State) {
				if err := s.store.Close(wait.ID); err != nil {
					return fmt.Errorf("closing terminal wait %s for session %s: %w", wait.ID, oldSessionID, err)
				}
				progressed++
				continue
			}
			labels := []string(nil)
			if !labelsContain(wait.Labels, newLabel) {
				labels = []string{newLabel}
			}
			if err := s.store.Update(wait.ID, beads.UpdateOpts{
				Labels:       labels,
				RemoveLabels: []string{oldLabel},
				Metadata:     map[string]string{"session_id": newSessionID},
			}); err != nil {
				return fmt.Errorf("reassign wait %s from session %s to %s: %w", wait.ID, oldSessionID, newSessionID, err)
			}
			progressed++
		}
		if !lookupCapped {
			return nil
		}
		if progressed == 0 {
			return err
		}
	}
}

// WakeOpts tunes WakeSession.
type WakeOpts struct {
	// RejectClosed makes a closed session a *WakeConflictError{State:"closed"}
	// before any write. Other callers pass the zero value.
	RejectClosed bool
}

// WakeResult carries the outcome of a WakeSession call.
type WakeResult struct {
	// NudgeIDs are the queued wait-nudge IDs to withdraw eagerly.
	NudgeIDs []string
	// Info is the pre-wake persisted projection of the session bead: SessionName
	// (for crash-history clearing) and MetadataState / Template (for the CLI's
	// post-wake checks).
	Info Info
}

// WakeSession clears hold/quarantine state and cancels open waits for a session,
// returning the queued wait-nudge IDs to withdraw and the pre-wake Info snapshot.
// It fuses the caller-side Get, session-bead guard, empty-type repair, optional
// closed-rejection, lifecycle-conflict check and wake batch into one call.
//
// The Get error is returned bare (errors.Is(err, beads.ErrNotFound) keeps
// caller mapping intact). A non-session bead returns an error wrapping
// ErrNotSessionBead. A lifecycle conflict — or a closed session when
// opts.RejectClosed is set — returns a *WakeConflictError.
func (s *Store) WakeSession(id string, now time.Time, opts WakeOpts) (WakeResult, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return WakeResult{}, err
	}
	if !IsSessionBeadOrRepairable(b) {
		return WakeResult{}, fmt.Errorf("%w: %s", ErrNotSessionBead, id)
	}
	RepairEmptyType(s.store.Store, &b)
	info := infoFromPersistedBead(b)
	if opts.RejectClosed && b.Status == "closed" {
		return WakeResult{}, &WakeConflictError{SessionID: id, State: "closed"}
	}
	nudgeIDs, err := s.wakeSessionFromBead(b, now)
	if err != nil {
		return WakeResult{}, err
	}
	return WakeResult{NudgeIDs: nudgeIDs, Info: info}, nil
}

// wakeSessionFromBead performs the lifecycle-conflict check, wait cancellation
// and wake batch over an already-fetched (and empty-type-repaired) session bead.
// It is shared by the fused WakeSession method and the deprecated package func.
func (s *Store) wakeSessionFromBead(sessionBead beads.Bead, now time.Time) ([]string, error) {
	if sessionBead.ID == "" {
		return nil, nil
	}
	lcInput := LifecycleInputFromMetadata(sessionBead.Status, sessionBead.Metadata)
	lcInput.Now = now
	view := ProjectLifecycle(lcInput)
	if state, conflict := lifecycleWakeConflictState(view); conflict {
		return nil, &WakeConflictError{SessionID: sessionBead.ID, State: state}
	}
	nudgeIDs, capped, err := s.cancelWaitsAndCollectNudgeIDs(sessionBead.ID, now)
	if err != nil {
		return nil, err
	}
	state := State(strings.TrimSpace(sessionBead.Metadata["state"]))
	batch := ClearWakeBlockersPatch(state, sessionBead.Metadata["sleep_reason"])
	for k, v := range RequestExplicitWakePatch(string(WakeCauseExplicit), now) {
		batch[k] = v
	}
	if view.BaseState == BaseStateArchived && view.ContinuityEligible {
		batch["archived_at"] = ""
		batch["continuity_eligible"] = "true"
	}
	if capped {
		StampWaitLookupCapMetadata(batch, "session:"+sessionBead.ID, SessionWaitLookupLimit, now, "wake-session")
	}
	if err := s.store.SetMetadataBatch(sessionBead.ID, batch); err != nil {
		return nil, err
	}
	return nudgeIDs, nil
}
