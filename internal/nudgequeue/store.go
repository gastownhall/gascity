package nudgequeue

import (
	"encoding/json"
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
