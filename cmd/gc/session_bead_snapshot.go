package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// sessionBeadSnapshot caches open session-bead state for a single reconcile
// cycle so build/sync/reconcile can reuse one store scan.
type sessionBeadSnapshot struct {
	open                      []beads.Bead
	sessionNameByAgentName    map[string]string
	sessionNameByTemplateHint map[string]string
}

func loadSessionBeadSnapshot(store beads.Store) (*sessionBeadSnapshot, error) {
	open, err := loadSessionBeads(store)
	if err != nil {
		return nil, err
	}
	return newSessionBeadSnapshot(open), nil
}

// newSessionBeadSnapshot indexes a set of open session beads for template- and
// instance-name lookups used during desired-state resolution.
//
// Pool-managed session beads are created identified by their pool template
// (e.g. agent_name="gascity/polecat") and only later acquire a themed or
// slot-numbered identity when realizePoolDesiredSessions claims a slot and
// writes pool_instance metadata. The indexing rules below reflect that two-
// phase identity:
//
//   - A pool bead whose agent_name still matches the template is substituted
//     with its pool_instance (e.g. "gascity/furiosa" or "gascity/dog-1") so
//     lookups by the realized instance name find the bead's session_name.
//   - A pool bead that has not yet had pool_instance written is intentionally
//     dropped from the agent-name index — indexing every slot under the bare
//     template would make one arbitrary slot win the template lookup.
//   - Canonical named-session beads (configured_named_identity set) always
//     win over pool substitutes for the same key, preserving correct
//     resolution when leaked pool beads coexist with canonical beads.
//   - Non-pool beads are indexed under agent_name and template unchanged.
func newSessionBeadSnapshot(open []beads.Bead) *sessionBeadSnapshot {
	filtered := make([]beads.Bead, 0, len(open))
	sessionNameByAgentName := make(map[string]string)
	sessionNameByTemplateHint := make(map[string]string)

	for _, b := range open {
		if b.Status == "closed" {
			continue
		}
		filtered = append(filtered, b)

		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		isCanonicalNamed := strings.TrimSpace(b.Metadata["configured_named_identity"]) != ""
		if agentName := sessionBeadAgentName(b); agentName != "" {
			if isPoolManagedSessionBead(b) && agentName == b.Metadata["template"] {
				// Pool bead still identified by the pool template. Substitute
				// the realized pool_instance (written after slot claim in
				// realizePoolDesiredSessions) so themed/numeric instance names
				// map back to this bead's session_name.
				if pi := strings.TrimSpace(b.Metadata["pool_instance"]); pi != "" {
					agentName = pi
				} else {
					agentName = ""
				}
			}
			if agentName == "" {
				continue
			}
			// Canonical named session beads always win the index so
			// resolveSessionName returns the correct session_name even
			// when leaked pool-style beads exist for the same template.
			if _, exists := sessionNameByAgentName[agentName]; !exists || isCanonicalNamed {
				sessionNameByAgentName[agentName] = sn
			}
		}
		if isPoolManagedSessionBead(b) {
			continue
		}
		if template := b.Metadata["template"]; template != "" {
			if _, exists := sessionNameByTemplateHint[template]; !exists || isCanonicalNamed {
				sessionNameByTemplateHint[template] = sn
			}
		}
		if commonName := b.Metadata["common_name"]; commonName != "" {
			if _, exists := sessionNameByTemplateHint[commonName]; !exists {
				sessionNameByTemplateHint[commonName] = sn
			}
		}
	}

	return &sessionBeadSnapshot{
		open:                      filtered,
		sessionNameByAgentName:    sessionNameByAgentName,
		sessionNameByTemplateHint: sessionNameByTemplateHint,
	}
}

func (s *sessionBeadSnapshot) replaceOpen(open []beads.Bead) {
	if s == nil {
		return
	}
	rebuilt := newSessionBeadSnapshot(open)
	if rebuilt == nil {
		s.open = nil
		s.sessionNameByAgentName = nil
		s.sessionNameByTemplateHint = nil
		return
	}
	*s = *rebuilt
}

func (s *sessionBeadSnapshot) add(bead beads.Bead) {
	if s == nil {
		return
	}
	open := s.Open()
	open = append(open, bead)
	s.replaceOpen(open)
}

func (s *sessionBeadSnapshot) Open() []beads.Bead {
	if s == nil {
		return nil
	}
	result := make([]beads.Bead, len(s.open))
	copy(result, s.open)
	return result
}

func (s *sessionBeadSnapshot) FindSessionNameByTemplate(template string) string {
	if s == nil {
		return ""
	}
	if sn := s.sessionNameByAgentName[template]; sn != "" {
		return sn
	}
	return s.sessionNameByTemplateHint[template]
}
