package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionBeadSnapshot caches session-bead state for a single reconcile cycle.
// Open-session lookups stay open-only; closed records are retained by ID for
// lifecycle guards such as stale wait epoch cancellation.
type sessionBeadSnapshot struct {
	open                      []beads.Bead
	recordByID                map[string]beads.Bead
	sessionNameByAgentName    map[string]string
	sessionNameByTemplateHint map[string]string
}

func loadSessionBeadSnapshot(store beads.Store) (*sessionBeadSnapshot, error) {
	if store == nil {
		return newSessionBeadSnapshot(nil), nil
	}
	all, err := store.List(beads.ListQuery{
		Label:         sessionBeadLabel,
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing session beads: %w", err)
	}
	sessions := make([]beads.Bead, 0, len(all))
	for _, bead := range all {
		if sessionpkg.IsSessionBeadOrRepairable(bead) {
			sessions = append(sessions, bead)
		}
	}
	return newSessionBeadSnapshot(sessions), nil
}

func newSessionBeadSnapshot(beadsIn []beads.Bead) *sessionBeadSnapshot {
	filtered := make([]beads.Bead, 0, len(beadsIn))
	byID := make(map[string]beads.Bead)
	sessionNameByAgentName := make(map[string]string)
	sessionNameByTemplateHint := make(map[string]string)

	for _, b := range beadsIn {
		if b.ID != "" {
			byID[b.ID] = b
		}
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
		recordByID:                byID,
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
		s.recordByID = nil
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

func (s *sessionBeadSnapshot) FindByID(id string) (beads.Bead, bool) {
	if s == nil || strings.TrimSpace(id) == "" {
		return beads.Bead{}, false
	}
	for _, bead := range s.open {
		if bead.ID == id {
			return bead, true
		}
	}
	return beads.Bead{}, false
}

func (s *sessionBeadSnapshot) findByIDIncludingClosed(id string) (beads.Bead, bool) {
	if s == nil || strings.TrimSpace(id) == "" {
		return beads.Bead{}, false
	}
	bead, ok := s.recordByID[id]
	if !ok {
		return beads.Bead{}, false
	}
	return bead, true
}

func (s *sessionBeadSnapshot) FindSessionNameByNamedIdentity(identity string) string {
	if s == nil || strings.TrimSpace(identity) == "" {
		return ""
	}
	for _, bead := range s.open {
		if strings.TrimSpace(bead.Metadata["configured_named_identity"]) != identity {
			continue
		}
		if sessionName := strings.TrimSpace(bead.Metadata["session_name"]); sessionName != "" {
			return sessionName
		}
	}
	return ""
}
