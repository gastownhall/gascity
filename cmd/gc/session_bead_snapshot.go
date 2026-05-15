package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionBeadSnapshot caches active session-bead state for a single reconcile
// cycle. Closed-session history is intentionally not loaded here: the
// reconciler calls this several times per tick, and closed history grows
// without bound. Callers that need a closed record must fetch that one ID
// explicitly.
//
// loadErr captures a non-fatal load failure (timeout, list error) so callers
// can distinguish "snapshot loaded clean, the bead simply isn't present" from
// "snapshot is degraded and may be missing entries it would otherwise have".
// See gastownhall/gascity#2148 for the named-session lookup-error visibility
// regression this field exists to surface.
type sessionBeadSnapshot struct {
	open                      []beads.Bead
	beadIDByAgentName         map[string]string
	beadIDByTemplateHint      map[string]string
	sessionNameByAgentName    map[string]string
	sessionNameByTemplateHint map[string]string
	loadErr                   error
}

// LoadError reports a non-fatal error from the snapshot's load path (timeout
// or list error). Returns nil when the snapshot loaded cleanly or when the
// receiver is nil. Callers in degraded-fail-soft paths (status rendering,
// named-session lookups) check this to surface the failure to operators
// instead of returning a synthetic "not present" result.
func (s *sessionBeadSnapshot) LoadError() error {
	if s == nil {
		return nil
	}
	return s.loadErr
}

// newSessionBeadSnapshotWithError builds a snapshot from beadsIn and tags it
// with a non-fatal load error. Callers that fail-soft on load (returning an
// empty snapshot instead of nil) use this so downstream consumers can still
// see the underlying failure via LoadError.
func newSessionBeadSnapshotWithError(beadsIn []beads.Bead, err error) *sessionBeadSnapshot {
	s := newSessionBeadSnapshot(beadsIn)
	s.loadErr = err
	return s
}

func loadSessionBeadSnapshot(store beads.Store) (*sessionBeadSnapshot, error) {
	if store == nil {
		return newSessionBeadSnapshot(nil), nil
	}
	all, err := store.List(beads.ListQuery{
		Label: sessionBeadLabel,
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
	beadIDByAgentName := make(map[string]string)
	beadIDByTemplateHint := make(map[string]string)
	sessionNameByAgentName := make(map[string]string)
	sessionNameByTemplateHint := make(map[string]string)

	for _, b := range beadsIn {
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
				agentName = stampedPoolQualifiedIdentity(b)
			}
			if agentName == "" {
				continue
			}
			// Canonical named session beads always win the index so
			// resolveSessionName returns the correct session_name even
			// when leaked pool-style beads exist for the same template.
			if _, exists := sessionNameByAgentName[agentName]; !exists || isCanonicalNamed {
				beadIDByAgentName[agentName] = b.ID
				sessionNameByAgentName[agentName] = sn
			}
		}
		if isPoolManagedSessionBead(b) {
			continue
		}
		if template := b.Metadata["template"]; template != "" {
			if _, exists := sessionNameByTemplateHint[template]; !exists || isCanonicalNamed {
				beadIDByTemplateHint[template] = b.ID
				sessionNameByTemplateHint[template] = sn
			}
		}
		if commonName := b.Metadata["common_name"]; commonName != "" {
			if _, exists := sessionNameByTemplateHint[commonName]; !exists {
				beadIDByTemplateHint[commonName] = b.ID
				sessionNameByTemplateHint[commonName] = sn
			}
		}
	}

	return &sessionBeadSnapshot{
		open:                      filtered,
		beadIDByAgentName:         beadIDByAgentName,
		beadIDByTemplateHint:      beadIDByTemplateHint,
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

func (s *sessionBeadSnapshot) FindSessionBeadByTemplate(template string) (beads.Bead, bool) {
	if s == nil {
		return beads.Bead{}, false
	}
	if id := s.beadIDByAgentName[template]; id != "" {
		return s.FindByID(id)
	}
	if id := s.beadIDByTemplateHint[template]; id != "" {
		return s.FindByID(id)
	}
	return beads.Bead{}, false
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

func (s *sessionBeadSnapshot) FindSessionNameByNamedIdentity(identity string) string {
	bead, ok := s.FindSessionBeadByNamedIdentity(identity)
	if !ok {
		return ""
	}
	return strings.TrimSpace(bead.Metadata["session_name"])
}

func (s *sessionBeadSnapshot) FindSessionBeadByNamedIdentity(identity string) (beads.Bead, bool) {
	if s == nil || strings.TrimSpace(identity) == "" {
		return beads.Bead{}, false
	}
	for _, bead := range s.open {
		if strings.TrimSpace(bead.Metadata["configured_named_identity"]) != identity {
			continue
		}
		return bead, true
	}
	return beads.Bead{}, false
}

func stampedPoolQualifiedIdentity(bead beads.Bead) string {
	if !isPoolManagedSessionBead(bead) {
		return ""
	}
	slot, err := strconv.Atoi(strings.TrimSpace(bead.Metadata["pool_slot"]))
	if err != nil || slot <= 0 {
		return ""
	}
	template := strings.TrimSpace(bead.Metadata["template"])
	if template == "" {
		return ""
	}
	scope, name := config.ParseQualifiedName(template)
	if name == "" {
		return ""
	}
	instance := fmt.Sprintf("%s-%d", name, slot)
	if scope != "" {
		return scope + "/" + instance
	}
	return instance
}
