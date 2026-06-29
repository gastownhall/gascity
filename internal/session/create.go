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

// CreateSession creates a session bead from spec and returns its id. It is the
// single front door for session-bead creation: the session Type and the
// [LabelSession, "agent:<AgentName>"] label pair are confined here, so no
// caller constructs a Type="session" bead directly. The emitted Create is
// byte-identical to the raw store.Create the create sites performed.
func (s *InfoStore) CreateSession(spec CreateSpec) (string, error) {
	created, err := s.store.Create(beads.Bead{
		ID:       spec.ID,
		Title:    spec.Title,
		Type:     BeadType,
		Labels:   []string{LabelSession, "agent:" + spec.AgentName},
		Metadata: spec.Metadata,
	})
	if err != nil {
		return "", err
	}
	return created.ID, nil
}
