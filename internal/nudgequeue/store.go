package nudgequeue

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file is the nudge-class front-door skeleton per
// OBJECT-MODEL-FRONT-DOOR-DESIGN sec 3.2 / 6.3.
//
// THE BEAD IS A SHADOW. The canonical nudge queue is the flock'd state.json
// (state.go, WithState over []Item). The nudge bead exists only for
// observability / event emission. So this wrapper is a thin veneer over the
// existing leaf helpers (cmd/gc/nudge_beads.go: ensure/markTerminal/find), NOT
// a new storage authority. The wrapper's write methods MUST remain callable
// inside the withNudgeQueueState transaction so the bead shadow and the
// state.json authority stay coherent under one flock.
//
// PHASE 0 STATUS: the wrapper type + Save/Terminalize/Find/FindIncludingTerminal
// SIGNATURES are the contract; their bodies are routed in Phase 2. The one
// genuinely net-new piece — decodeNudgeItem, the MISSING HALF of the codec
// (today only Item->Bead exists; reference_json is written but never read back)
// — is implemented and golden round-trip tested here.

// nudgeBeadLabel mirrors cmd/gc/nudge_beads.go (nudgeBeadType "chore" beads
// carry this label). coordclass also mirrors it privately for routing; all
// three must stay in sync.
const nudgeBeadLabel = "gc:nudge"

// nudgeBeadType is the bead type used for queued-nudge shadow beads.
const nudgeBeadType = "chore"

// NudgeShadow is the partial, read-only view decoded from a nudge shadow bead.
// It carries ONLY the fields the bead is authoritative for: the controller-
// stamped terminal fields (State / TerminalReason / CommitBoundary) plus
// identity. Queue-only runtime fields (Attempts, ClaimedAt, LeaseUntil, DeadAt,
// CreatedAt) live exclusively in state.json and are deliberately absent here so
// callers cannot trust a zero value for them — per the design's open question,
// a narrow view is preferred over a half-populated Item.
type NudgeShadow struct {
	// ID is the durable nudge id (the queue Item.ID; metadata["nudge_id"]).
	ID string
	// BeadID is the shadow bead's own id.
	BeadID string
	// State is the lifecycle state stamped on the bead ("queued" or a terminal
	// state like "injected"/"failed"/"expired"/"superseded").
	State string
	// TerminalReason is the controller-stamped reason set at terminalization.
	TerminalReason string
	// CommitBoundary is the controller-stamped commit boundary at terminalization.
	CommitBoundary string
	// Reference is the optional decoded reference (the previously write-only
	// reference_json field — this decoder is the first reader of it).
	Reference *Reference
	// Agent / SessionID / Source / Message are carried verbatim from metadata.
	Agent     string
	SessionID string
	Source    string
	Message   string
	// DeliverAfter / ExpiresAt are the parsed scheduling timestamps if present.
	DeliverAfter time.Time
	ExpiresAt    time.Time
}

// Store is the nudge-class domain wrapper. It holds the strongly-typed
// beads.NudgesStore by value and confines the Item<->Bead codec.
//
// Every method is nil-receiver safe: a nil *Store (the value cmd/gc passes when
// the shadow bead store fails to open) and a *Store over a nil embedded store both
// degrade to a no-op. The flock'd state.json — not this shadow bead — is the queue
// authority, so a missing shadow store must never panic a caller mid-transaction.
type Store struct {
	store beads.NudgesStore
}

// NewStore wraps a strongly-typed nudges-class store as the nudge front door.
func NewStore(store beads.NudgesStore) *Store {
	return &Store{store: store}
}

// decodeNudgeItem projects a nudge shadow bead onto a NudgeShadow view. It is
// the missing read half of the nudge codec: it reads the controller-stamped
// terminal fields and, for the first time, the previously write-only
// reference_json. It is pure, side-effect-free, and backend-invariant (reads
// only bead fields), matching the projection-invariance invariant.
func decodeNudgeItem(b beads.Bead) NudgeShadow {
	s := NudgeShadow{
		BeadID:         b.ID,
		ID:             b.Metadata["nudge_id"],
		State:          b.Metadata["state"],
		TerminalReason: b.Metadata["terminal_reason"],
		CommitBoundary: b.Metadata["commit_boundary"],
		Agent:          b.Metadata["agent"],
		SessionID:      b.Metadata["session_id"],
		Source:         b.Metadata["source"],
		Message:        b.Metadata["message"],
	}
	if raw := b.Metadata["reference_json"]; raw != "" {
		var ref Reference
		if err := json.Unmarshal([]byte(raw), &ref); err == nil {
			s.Reference = &ref
		}
	}
	if raw := b.Metadata["deliver_after"]; raw != "" {
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			s.DeliverAfter = ts
		}
	}
	if raw := b.Metadata["expires_at"]; raw != "" {
		if ts, err := time.Parse(time.RFC3339, raw); err == nil {
			s.ExpiresAt = ts
		}
	}
	return s
}

// DecodeShadow exposes decodeNudgeItem for callers in package main that route
// their Metadata[...] cracks through the typed view in Phase 2. It is the public
// face of the read codec; the unexported name keeps the codec confined.
func DecodeShadow(b beads.Bead) NudgeShadow { return decodeNudgeItem(b) }

// EnqueueRollbackCloseReason is the close_reason metadata value stamped on a
// partially-created nudge shadow bead when the enqueue transaction fails after
// the bead was created. RollbackEnqueue stamps it before Close so BdStore.Close
// forwards it as `bd close --reason`, satisfying validation.on-close=error.
// The 42-character form satisfies the >=20 char validator floor.
const EnqueueRollbackCloseReason = "nudge rollback: enqueue transaction failed"

// Save creates the nudge shadow bead for item if one does not already exist,
// returning the bead id and whether a new bead was created. It is the write
// half of the codec: the Item->Bead serialization (metadata map, labels, title,
// type) is confined here. The flock'd state.json remains the queue authority;
// this bead is the observability shadow, so Save must stay callable inside the
// withNudgeQueueState transaction.
//
// Save emits byte-identical bead writes to the prior raw ensureQueuedNudgeBead
// helper: an existence check by the durable nudge label, then a single Create
// when absent.
func (s *Store) Save(item Item) (beadID string, created bool, err error) {
	if s == nil || s.store.Store == nil {
		return "", false, nil
	}
	existing, ok, err := s.find(item.ID, false)
	if err != nil {
		return "", false, err
	}
	if ok {
		return existing.ID, false, nil
	}
	meta := map[string]string{
		"nudge_id":           item.ID,
		"agent":              item.Agent,
		"session_id":         item.SessionID,
		"continuation_epoch": item.ContinuationEpoch,
		"state":              "queued",
		"source":             item.Source,
		"message":            item.Message,
		"deliver_after":      item.DeliverAfter.UTC().Format(time.RFC3339),
		"expires_at":         item.ExpiresAt.UTC().Format(time.RFC3339),
		"reference_json":     marshalReference(item.Reference),
		"last_attempt_at":    formatOptionalTime(item.LastAttemptAt),
		"last_error":         item.LastError,
		"terminal_reason":    "",
		"commit_boundary":    "",
		"terminal_at":        "",
	}
	createdBead, err := s.store.Create(beads.Bead{
		Title: "nudge:" + item.ID,
		Type:  nudgeBeadType,
		Labels: []string{
			nudgeBeadLabel,
			"agent:" + item.Agent,
			"nudge:" + item.ID,
			"source:" + item.Source,
		},
		Metadata: meta,
	})
	if err != nil {
		return "", false, err
	}
	return createdBead.ID, true, nil
}

// Terminalize stamps the controller-supplied terminal fields on the shadow bead
// and closes it. It is the write half of the terminal codec: the update map, the
// canonical close_reason floor, and the BeadID-then-find fallback are confined
// here. It emits byte-identical bead writes to the prior raw markQueuedNudgeTerminal
// helper (SetMetadataBatch with the same keys, then Close), tolerating a missing
// bead as a no-op.
func (s *Store) Terminalize(item Item, state, reason, commitBoundary string, now time.Time) error {
	if s == nil || s.store.Store == nil {
		return nil
	}
	update := map[string]string{
		"state":           state,
		"last_attempt_at": formatOptionalTime(item.LastAttemptAt),
		"last_error":      item.LastError,
		"terminal_reason": reason,
		"commit_boundary": commitBoundary,
		"terminal_at":     now.UTC().Format(time.RFC3339),
		"close_reason":    canonicalCloseReason(state),
	}

	tryTerminalize := func(beadID string) error {
		if beadID == "" {
			return beads.ErrNotFound
		}
		if err := s.store.SetMetadataBatch(beadID, update); err != nil {
			if isMissingNudgeBeadErr(err, beadID) {
				return beads.ErrNotFound
			}
			return err
		}
		if err := s.store.Close(beadID); err != nil {
			if isMissingNudgeBeadErr(err, beadID) {
				return beads.ErrNotFound
			}
			return err
		}
		return nil
	}

	if err := tryTerminalize(item.BeadID); err == nil {
		return nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return err
	}

	b, ok, err := s.find(item.ID, true)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := tryTerminalize(b.ID); err != nil && !errors.Is(err, beads.ErrNotFound) {
		return err
	}
	return nil
}

// RollbackEnqueue stamps the canonical rollback close_reason on a leaked nudge
// shadow bead and closes it. It is the rollback path for a failed enqueue
// transaction whose bead was already created. It emits byte-identical bead
// writes to the prior inline SetMetadata(close_reason)+Close in
// enqueueQueuedNudgeWithStore. Errors are joined and returned by the caller so a
// leaked open bead is diagnosable.
func (s *Store) RollbackEnqueue(beadID string) error {
	if s == nil || s.store.Store == nil || beadID == "" {
		return nil
	}
	var errs error
	if err := s.store.SetMetadata(beadID, "close_reason", EnqueueRollbackCloseReason); err != nil {
		errs = errors.Join(errs, err)
	}
	if err := s.store.Close(beadID); err != nil {
		errs = errors.Join(errs, err)
	}
	return errs
}

// Find returns the OPEN (or terminal-but-decodable) nudge shadow for nudgeID as
// a typed NudgeShadow, plus whether one was found. It is the existence gate used
// by wait readiness; callers receive the decoded view rather than a raw bead.
func (s *Store) Find(nudgeID string) (NudgeShadow, bool, error) {
	b, ok, err := s.find(nudgeID, false)
	if err != nil || !ok {
		return NudgeShadow{}, ok, err
	}
	return decodeNudgeItem(b), true, nil
}

// FindIncludingTerminal returns the nudge shadow for nudgeID including closed,
// terminal beads, as a typed NudgeShadow. Callers read the controller-stamped
// terminal fields (State / TerminalReason / CommitBoundary) off the decoded view
// instead of cracking bead Metadata directly.
func (s *Store) FindIncludingTerminal(nudgeID string) (NudgeShadow, bool, error) {
	b, ok, err := s.find(nudgeID, true)
	if err != nil || !ok {
		return NudgeShadow{}, ok, err
	}
	return decodeNudgeItem(b), true, nil
}

// find is the shared read primitive behind Find / FindIncludingTerminal / Save /
// Terminalize. It resolves the most recent nudge shadow bead for nudgeID by the
// durable "nudge:<id>" label, applying the lookup cap and the open-vs-terminal
// selection rules. It is backend-invariant: it reads only bead fields.
func (s *Store) find(nudgeID string, includeClosed bool) (beads.Bead, bool, error) {
	if s == nil || s.store.Store == nil || nudgeID == "" {
		return beads.Bead{}, false, nil
	}
	items, err := s.store.List(beads.ListQuery{
		Label:         "nudge:" + nudgeID,
		IncludeClosed: includeClosed,
		Limit:         NudgeLookupLimit + 1,
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return beads.Bead{}, false, err
	}
	capped := len(items) > NudgeLookupLimit
	var fallback beads.Bead
	hasFallback := false
	for _, item := range items {
		if item.Status != "closed" {
			return item, true, nil
		}
		if !includeClosed {
			continue
		}
		if isTerminalNudgeState(item.Metadata["state"]) {
			return item, true, nil
		}
		if !capped && !hasFallback {
			fallback = item
			hasFallback = true
		}
	}
	if capped {
		return beads.Bead{}, false, beads.LookupLimitError{Kind: "nudge", Label: "nudge:" + nudgeID, Limit: NudgeLookupLimit}
	}
	if includeClosed && hasFallback {
		return fallback, true, nil
	}
	return beads.Bead{}, false, nil
}

// IsTerminalState reports whether a nudge lifecycle state code is terminal. It
// is the package-canonical predicate; callers route their state checks through
// it (or through the decoded NudgeShadow) rather than re-listing the codes.
func IsTerminalState(state string) bool { return isTerminalNudgeState(state) }

// FindBead returns the raw OPEN nudge shadow bead for nudgeID. It exists for the
// cmd/gc thin adapters (and their existing tests) that still inspect the bead
// directly; new callers should prefer Find, which returns the decoded shadow.
func (s *Store) FindBead(nudgeID string) (beads.Bead, bool, error) {
	return s.find(nudgeID, false)
}

// FindBeadIncludingTerminal returns the raw nudge shadow bead for nudgeID,
// including closed terminal beads. Cmd/gc adapter shim; prefer
// FindIncludingTerminal for new callers.
func (s *Store) FindBeadIncludingTerminal(nudgeID string) (beads.Bead, bool, error) {
	return s.find(nudgeID, true)
}

// CanonicalCloseReason is the exported face of the close_reason floor codec, for
// the cmd/gc adapter test that guards the >=20 char validator floor.
func CanonicalCloseReason(stateCode string) string { return canonicalCloseReason(stateCode) }

func isTerminalNudgeState(state string) bool {
	switch state {
	case "accepted_for_injection", "injected", "expired", "failed", "superseded":
		return true
	default:
		return false
	}
}

// canonicalCloseReason maps a nudge terminalization state code to a
// human-readable close_reason of at least 20 characters, suitable for
// `bd close --reason` under validation.on-close=error. Terminalize stamps the
// result in metadata.close_reason before Close; BdStore.Close forwards it as the
// --reason argument. Without the canonical reason, validators reject the close,
// the withNudgeQueueState transaction rolls back, and the nudge bounces between
// InFlight and Pending until expires_at cuts in.
func canonicalCloseReason(stateCode string) string {
	switch stateCode {
	case "failed":
		return "nudge failed: queue terminalization rejected delivery"
	case "expired":
		return "nudge expired past deliver-by deadline"
	case "superseded":
		return "nudge superseded by newer queued entry"
	case "injected":
		return "nudge delivered via provider injection"
	case "accepted_for_injection":
		return "nudge accepted for hook-transport injection"
	}
	if len(stateCode) >= 20 {
		return stateCode
	}
	if stateCode == "" {
		return "nudge terminalized: unknown-state"
	}
	return "nudge terminalized: " + stateCode
}

func isMissingNudgeBeadErr(err error, beadID string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, beads.ErrNotFound) {
		return true
	}
	beadID = strings.ToLower(strings.TrimSpace(beadID))
	if beadID == "" {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no issue found matching "+strings.ToLower(strconv.Quote(beadID))) ||
		strings.Contains(msg, "error resolving "+beadID+": no issue found") ||
		strings.Contains(msg, "ambiguous id") ||
		strings.Contains(msg, "use more characters to disambiguate")
}

func marshalReference(ref *Reference) string {
	if ref == nil {
		return ""
	}
	data, err := json.Marshal(ref)
	if err != nil {
		return ""
	}
	return string(data)
}

func formatOptionalTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}
