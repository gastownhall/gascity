package session

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// InfoFromPersistedBead projects a persisted session bead onto session.Info
// using only data stored on the bead — no live runtime overlay (no liveness
// probe, transport detection, or ACP routing). It is the pure, side-effect-free
// half of the manager codec: Manager.infoFromBead applies this projection and
// then enriches it with runtime state.
//
// Because the projection reads only bead fields, it is invariant across storage
// backends: a bead persisted to bd, sqlite, or postgres round-trips to the same
// Info. Callers that need live runtime state (Attached, runtime-downgraded
// State, detected transport) must go through Manager, not this function.
func InfoFromPersistedBead(b beads.Bead) Info {
	// Bead-level prologue: fields that are not metadata-derived. These MUST be
	// set before the codec table runs — the session_name setter reads info.ID
	// for its sessionNameFor fallback, and the state setter reads info.Closed to
	// blank State on closed beads (invariant I6).
	info := Info{
		ID:        b.ID,
		Type:      b.Type,
		Title:     b.Title,
		Labels:    b.Labels,
		CreatedAt: b.CreatedAt,
		Closed:    b.Status == "closed",
	}
	// Project every metadata-derived field through the shared codec table. An
	// absent key reads as "" (Go map default), matching the old struct literal's
	// zero-valued reads; each setter is total over "". Starting from a fresh
	// zero-valued Info, the table's ApplyPatch-form setters reproduce the old
	// projection exactly (invariant I1, gated by the parity oracle tests).
	for i := range infoKeyCodec {
		spec := &infoKeyCodec[i]
		spec.set(&info, b.Metadata[spec.key])
	}
	return info
}

// Store is the session-domain front door over a session-class bead store: the
// single typed seam through which callers read and write sessions without
// touching *beads.Bead. The read half (Get / List, projecting via
// InfoFromPersistedBead) lives here; the write half (ApplyPatch + the typed
// lifecycle methods) lives in store.go. Bead serialization — SetMetadataBatch,
// Update, Close, the metadata-key vocabulary — is confined inside this type.
// (Formerly named InfoStore, after its read return type, when it was read-only.)
//
// The Get/List projection is the persisted view only — no live runtime overlay.
// Callers that need live runtime enrichment (liveness, attachment, detected
// transport) still go through session.Manager. The API/response-building layer
// currently reads persisted state via Manager.GetWithPersistedResponse (same
// InfoFromPersistedBead codec); routing that read path through Store is a
// follow-up. The reconciler already routes its writes through this type.
type Store struct {
	store beads.SessionStore
}

// NewStore wraps a strongly-typed session-class store as the session-domain
// front door. The wrapper holds the typed beads.SessionStore by value; the
// embedded .Store is used for all bead access internally.
func NewStore(store beads.SessionStore) *Store {
	return &Store{store: store}
}

// Get returns the persisted session.Info for the given id. It returns
// ErrSessionNotFound when no session bead exists for the id.
func (s *Store) Get(id string) (Info, error) {
	b, err := s.store.Get(id)
	if err != nil {
		return Info{}, fmt.Errorf("loading session %q: %w", id, err)
	}
	if strings.TrimSpace(b.ID) == "" || !IsSessionBeadOrRepairable(b) {
		return Info{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return InfoFromPersistedBead(b), nil
}

// List returns the persisted session.Info for all session beads, applying the
// same state and template filtering semantics as the catalog listing. An empty
// stateFilter excludes closed sessions; stateFilter "all" includes everything.
// Only session.Info is returned — no raw beads cross this boundary.
func (s *Store) List(stateFilter, templateFilter string) ([]Info, error) {
	// IncludeClosed so the in-memory filter below can honor state=closed and
	// state=all; sessionMatchesFilters drops closed beads for the default and
	// non-closed filters, matching Manager.ListFullFromBeads semantics.
	all, err := s.store.List(beads.ListQuery{
		Label:         LabelSession,
		Sort:          beads.SortCreatedDesc,
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	out := make([]Info, 0, len(all))
	for _, b := range all {
		if !IsSessionBeadOrRepairable(b) {
			continue
		}
		if !sessionMatchesFilters(b, stateFilter, templateFilter) {
			continue
		}
		out = append(out, InfoFromPersistedBead(b))
	}
	return out, nil
}

// sessionMatchesFilters reports whether a session bead passes the state and
// template filters. It is the single predicate for session-list filtering,
// shared by both InfoStore listing and Manager.ListFullFromBeads.
func sessionMatchesFilters(b beads.Bead, stateFilter, templateFilter string) bool {
	state := normalizeInfoState(State(b.Metadata["state"]))

	switch {
	case stateFilter != "" && stateFilter != "all":
		match := false
		for _, sf := range strings.Split(stateFilter, ",") {
			switch {
			case sf == "closed" && b.Status == "closed":
				match = true
			case sf == "open" && b.Status == "open":
				match = true
			case b.Status != "closed" && sf == string(state):
				match = true
			}
			if match {
				break
			}
		}
		if !match {
			return false
		}
	case stateFilter == "":
		if b.Status == "closed" {
			return false
		}
	}

	if templateFilter != "" && b.Metadata["template"] != templateFilter {
		return false
	}
	return true
}
