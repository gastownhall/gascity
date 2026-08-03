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
func infoFromPersistedBead(b beads.Bead) Info {
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
// reads persisted state through this type's GetPersistedResponse and pairs it
// with Manager.EnrichInfo for the runtime overlay (see the api sessionGetEnriched
// composition and worker.sessionRecordViaManager). The reconciler
// already routes its writes through this type.
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
// ErrSessionNotFound when the bead EXISTS but is not a session bead (or carries
// an empty id); an ABSENT id surfaces the store's not-found error wrapped as
// `loading session %q` (NOT ErrSessionNotFound). Callers must not
// errors.Is(err, ErrSessionNotFound) to detect absence — check for the wrapped
// beads.ErrNotFound instead. See validatedBead.
func (s *Store) Get(id string) (Info, error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return Info{}, err
	}
	return infoFromPersistedBead(b), nil
}

// GetPersistedResponse returns the persisted session.Info paired with the
// persisted-response projection (status + metadata) for id, in a single store
// fetch. It is the persisted-read half of the session Get read model — pair it
// with Manager.EnrichInfo for the runtime overlay (the api sessionGetEnriched
// composition and worker.sessionRecordViaManager do exactly
// that): the caller gets both projections without a raw *beads.Bead crossing the
// boundary and without a second store.Get. It shares Get's exact
// error contract (both route through validatedBead): ErrSessionNotFound for a
// present-but-non-session bead, and the wrapped store not-found error (NOT
// ErrSessionNotFound) for an absent id.
func (s *Store) GetPersistedResponse(id string) (Info, PersistedResponse, error) {
	b, err := s.validatedBead(id)
	if err != nil {
		return Info{}, PersistedResponse{}, err
	}
	return infoFromPersistedBead(b), PersistedResponseFromBead(b), nil
}

// validatedBead loads the session bead for id. A load failure (including an
// absent id) is wrapped with `loading session %q` context; a loaded bead that is
// not a session bead (or has an empty id) is rejected with ErrSessionNotFound. It
// is the shared read behind Get and GetPersistedResponse so the two agree on
// validation and error text (a single source of truth for "is this a session
// bead"). Note the split: absence yields the wrapped store error, NOT
// ErrSessionNotFound — that sentinel is reserved for a present non-session bead.
func (s *Store) validatedBead(id string) (beads.Bead, error) {
	// Nil-inner-store short-circuit, mirroring ListAll's listAllBeads guard
	// (s == nil || s.store.Store == nil): a nil backing store cannot produce the
	// bead, so treat it as absence — the wrapped store not-found error, matching
	// the absent-id path below and NOT the ErrSessionNotFound sentinel (reserved
	// for a present non-session bead). Without this a non-nil *Store wrapping a
	// nil inner store would panic in s.store.Get.
	if s == nil || s.store.Store == nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, beads.ErrNotFound)
	}
	b, err := s.store.Get(id)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("loading session %q: %w", id, err)
	}
	if strings.TrimSpace(b.ID) == "" || !IsSessionBeadOrRepairable(b) {
		return beads.Bead{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return b, nil
}

// List returns the persisted session.Info for all session beads, applying the
// same state and template filtering semantics as the catalog listing. An empty
// stateFilter excludes closed sessions; stateFilter "all" includes everything.
// Only session.Info is returned — no raw beads cross this boundary.
func (s *Store) List(stateFilter, templateFilter string) ([]Info, error) {
	// IncludeClosed so the in-memory filter below can honor state=closed and
	// state=all; sessionMatchesFilters drops closed beads for the default and
	// non-closed filters, matching the shared session-list filtering semantics.
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
		out = append(out, infoFromPersistedBead(b))
	}
	return out, nil
}

// ListByMetadataInfos returns the Info projection of every bead matching the given
// metadata filters, keeping the raw-bead codec confined to this edge. It is the typed
// front door for the session-log workdir fallback's ListByMetadata scans (the callers
// need only Info fields). limit is passed through to the store; a zero limit is
// unbounded. No raw bead escapes.
func (s *Store) ListByMetadataInfos(filters map[string]string, limit int) ([]Info, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	found, err := s.store.ListByMetadata(filters, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(found))
	for _, b := range found {
		out = append(out, infoFromPersistedBead(b))
	}
	return out, nil
}

// ListLabeledSessionInfosUnfiltered returns the Info projection of every OPEN bead
// carrying the gc:session label, WITHOUT the IsSessionBeadOrRepairable narrowing that
// List applies. It is the label-only, closed-excluded, unfiltered lister the
// city-stop sleep-reason sweep needs: that sweep marks possibly-damaged
// gc:session-labeled beads whose type is a non-empty non-"session" value, which
// List's IsSessionBeadOrRepairable filter would drop, and it must NOT widen to the
// ListAll type+label union (which would also mark label-lost type-only beads — a
// behavior change). ListByLabel already excludes closed beads by default; the
// explicit closed skip keeps the closed-excluded contract byte-stable across store
// backends. No raw bead escapes.
func (s *Store) ListLabeledSessionInfosUnfiltered() ([]Info, error) {
	if s == nil || s.store.Store == nil {
		return nil, nil
	}
	labeled, err := s.store.ListByLabel(LabelSession, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(labeled))
	for _, b := range labeled {
		if b.Status == "closed" {
			continue
		}
		out = append(out, infoFromPersistedBead(b))
	}
	return out, nil
}

// sessionMatchesFilters reports whether a session bead passes the state and
// template filters. It is the single predicate for session-list filtering,
// shared by the Store.List projection and (via sessionMatchesFiltersInfo) the
// Info-fed listing.
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

// sessionMatchesFiltersInfo is the Info-taking twin of sessionMatchesFilters. It
// recomputes the state from MetadataState (the RAW state metadata, NOT the
// closed-blanked/normalized Info.State — the bead form derives `state` from
// b.Metadata["state"] regardless of close), reads Closed for the open/closed
// status compares, and Template for the template filter.
//
// ACCEPTED DELTA: the bead form compares the exact status string
// (b.Status=="open"), while this twin has only Closed (== b.Status=="closed") to
// work with — Info carries no raw Status. For the SDK's binary open/closed
// invariant the two are identical; for a hypothetical out-of-band status
// (e.g. "archived") the twin's `sf=="open"` (== !Closed) is WIDER than the bead
// form's exact `b.Status=="open"`. They diverge ONLY on the "open" filter for a
// non-open, non-closed status; everywhere else they agree.
// TestSessionMatchesFiltersInfoEquivalence pins the byte-identity across the
// open/closed corpus (including the closed-with-raw-state trap) AND pins this
// documented open-filter delta against an exotic-status row.
func sessionMatchesFiltersInfo(info Info, stateFilter, templateFilter string) bool {
	state := normalizeInfoState(State(info.MetadataState))

	switch {
	case stateFilter != "" && stateFilter != "all":
		match := false
		for _, sf := range strings.Split(stateFilter, ",") {
			switch {
			case sf == "closed" && info.Closed:
				match = true
			case sf == "open" && !info.Closed:
				match = true
			case !info.Closed && sf == string(state):
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
		if info.Closed {
			return false
		}
	}

	if templateFilter != "" && info.Template != templateFilter {
		return false
	}
	return true
}
