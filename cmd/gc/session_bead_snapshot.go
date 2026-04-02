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
				agentName = ""
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
