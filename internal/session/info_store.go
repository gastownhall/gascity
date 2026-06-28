package session

import (
	"fmt"
	"strings"
	"time"

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
	sessName := b.Metadata["session_name"]
	if sessName == "" {
		sessName = sessionNameFor(b.ID)
	}
	closed := b.Status == "closed"

	state := normalizeInfoState(State(b.Metadata["state"]))
	if closed {
		state = "" // closed beads have no runtime state
	}

	info := Info{
		ID:            b.ID,
		Template:      b.Metadata["template"],
		State:         state,
		Closed:        closed,
		Title:         b.Title,
		Alias:         b.Metadata["alias"],
		AgentName:     b.Metadata["agent_name"],
		Provider:      b.Metadata["provider"],
		Transport:     transportFromMetadata(b),
		Command:       b.Metadata["command"],
		WorkDir:       b.Metadata["work_dir"],
		SessionName:   sessName,
		SessionKey:    b.Metadata["session_key"],
		ResumeFlag:    b.Metadata["resume_flag"],
		ResumeStyle:   b.Metadata["resume_style"],
		ResumeCommand: b.Metadata["resume_command"],
		CreatedAt:     b.CreatedAt,
	}
	if raw := strings.TrimSpace(b.Metadata[MetadataLastNudgeDeliveredAt]); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			info.LastNudgeDeliveredAt = parsed
		}
	}
	return info
}

// InfoStore is an Info-typed domain store over a session-class bead store. It
// speaks session.Info: callers read and list sessions without touching
// *beads.Bead. Bead serialization is confined inside this type via the
// InfoFromPersistedBead codec.
//
// InfoStore returns the persisted projection only — no live runtime overlay.
//
// NOTE: this is the intended next-step read seam for the persisted view; it has
// no production callers yet. The API/response-building layer currently routes
// its persisted reads through Manager.GetWithPersistedResponse (which already
// uses the same InfoFromPersistedBead codec internally), not through InfoStore.
// Wiring the read path through InfoStore is a follow-up; until then this type is
// the documented seam, not a live path. Callers that need live runtime
// enrichment (liveness, attachment, detected transport) still go through
// session.Manager.
type InfoStore struct {
	store beads.SessionStore
}

// NewInfoStore wraps a strongly-typed session-class store as an Info-typed
// domain store. The wrapper holds the typed beads.SessionStore by value; the
// embedded .Store is used for all bead access internally.
func NewInfoStore(store beads.SessionStore) *InfoStore {
	return &InfoStore{store: store}
}

// Get returns the persisted session.Info for the given id. It returns
// ErrSessionNotFound when no session bead exists for the id.
func (s *InfoStore) Get(id string) (Info, error) {
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
func (s *InfoStore) List(stateFilter, templateFilter string) ([]Info, error) {
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
// template filters, using the same rules as Manager.ListFullFromBeads so the
// Info-typed listing stays projection-identical to the existing catalog path.
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
