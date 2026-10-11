package session

import "github.com/gastownhall/gascity/internal/beads"

// CreateSpec captures the typed vocabulary for creating a session bead through
// the front door. It is the byte-identical replacement for the inline
// beads.Bead{Type: session, Labels: [gc:session, agent:<name>], ...} literals
// the raw create sites assemble (cmd/gc/session_beads.go and
// cmd/gc/session_name_lookup.go).
//
// The front door owns the bead envelope — the session Type and the
// [LabelSession, "agent:<AgentName>"] label pair — so no caller re-declares
// that shape. The caller still assembles the metadata vocabulary (the create
// sites build it inline from template/provider/pool inputs, which are not
// session-domain concerns) and passes it verbatim as Metadata; CreateSession
// does not interpret or mutate it.
type CreateSpec struct {
	// ID, when non-empty, is the explicit bead id to assign (the pool create
	// site pre-allocates an id for deterministic pool-session naming). When
	// empty the store assigns an id, which CreateSession returns.
	ID string

	// Title is the bead Title (the agent name for configured-named sessions,
	// the target basename or agent name for pool sessions).
	Title string

	// AgentName drives the "agent:<AgentName>" label that selects a session by
	// its owning agent. It is the same value the raw sites passed to the
	// "agent:" + agentName label construction.
	AgentName string

	// Metadata is the assembled session-bead metadata map, written verbatim.
	Metadata map[string]string
}

// CreateSessionInfo creates a session bead from spec and returns the projected
// session.Info of the just-created bead. It is the write-returns-Info create
// front door: the store's Create returns the persisted bead, so the Info is a
// LOCAL InfoFromPersistedBead fold on that bead — never a post-create Get. The
// session Type and the [LabelSession, "agent:<AgentName>"] label pair are
// confined here, so no caller constructs a Type="session" bead directly, and the
// emitted Create is byte-identical to the raw store.Create the create sites
// performed.
//
// Error contract: on a store Create error, NO bead is persisted and (Info{}, err)
// is returned — there is no silent half-create. On success the projection is
// total (InfoFromPersistedBead never fails over a just-created session bead), so
// the created bead is always reported as Info; a caller must never receive a
// created-but-unreported bead. CreateSession is the id-only sibling for callers
// that need only the id.
//
// Backend parity: because this projects the Create ECHO instead of re-Getting, the
// guarantee that the returned Info equals a subsequent Get's projection rests on the
// store backend faithfully echoing the created bead's fields on Create (memstore
// clones the stored bead; the CachingStore Get-refreshes write-through; BdStore and
// the Dolt stores reconstruct the bead from bd's create response). That parity is
// pinned across every backend by the beadstest conformance case
// CreateEchoMatchesGetOnMetadata, not just by the memstore-backed oracle here.
func (s *Store) CreateSessionInfo(spec CreateSpec) (Info, error) {
	created, err := s.store.Create(beads.Bead{
		ID:       spec.ID,
		Title:    spec.Title,
		Type:     BeadType,
		Labels:   []string{LabelSession, "agent:" + spec.AgentName},
		Metadata: spec.Metadata,
	})
	if err != nil {
		return Info{}, err
	}
	return infoFromPersistedBead(created), nil
}

// CreateSession creates a session bead from spec and returns its id. It is the
// id-only sibling of CreateSessionInfo (the single front door for session-bead
// creation) and delegates to it, so both emit the byte-identical Create; callers
// that need the projected Info without a post-create Get use CreateSessionInfo.
func (s *Store) CreateSession(spec CreateSpec) (string, error) {
	info, err := s.CreateSessionInfo(spec)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}
