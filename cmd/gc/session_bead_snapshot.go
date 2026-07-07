package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

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
	// mu guards open + the four lookup maps. add() (called from inside
	// createPoolSessionBead) can fire from multiple goroutines when
	// realizePoolDesiredSessions parallelizes pool session bead creates
	// across distinct aliases — see gastownhall/gascity#2319. All read
	// methods take RLock; add() takes Lock.
	mu   sync.RWMutex
	open []beads.Bead
	// openInfos is the session.Info projection of open, in lockstep order:
	// openInfos[i] == InfoFromPersistedBead(open[i]). It is the typed front
	// door the P4 consumers migrate onto; the raw open slice and the index
	// maps below stay byte-identical for the current callers.
	openInfos                 []sessionpkg.Info
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// newSessionBeadSnapshotWithError builds an empty snapshot and tags it with a
// non-fatal load error. Callers that fail-soft on load (returning an empty
// snapshot instead of nil) use this so downstream consumers can still see the
// underlying failure via LoadError.
func newSessionBeadSnapshotWithError(err error) *sessionBeadSnapshot {
	s := newSessionBeadSnapshot(nil)
	// loadErr is set during construction, before s is published to any other
	// goroutine, so no s.mu lock is needed here even though LoadError() reads
	// it under RLock.
	s.loadErr = err
	return s
}

func loadSessionBeadSnapshot(store beads.Store) (*sessionBeadSnapshot, error) {
	if store == nil {
		return newSessionBeadSnapshot(nil), nil
	}
	// Type+Label union via the shared helper. The motivating bug:
	// canonical configured_named_session beads can lose their gc:session
	// label after crashes or schema migrations but retain
	// issue_type=session; a label-only query strands them invisible to
	// the reconciler, which then never heals their state=awake metadata
	// after a runtime is lost. Their alias reservations live forever,
	// blocking createPoolSessionBead from materializing replacements
	// ("alias … already belongs to gm-XXXX") and preventing the pool
	// from spawning for that template until manual intervention.
	//
	// Closed history is intentionally not loaded here — the reconciler
	// calls this several times per tick and closed history grows
	// without bound. Callers that need a closed record must fetch that
	// one ID explicitly.
	sessions, err := sessionpkg.ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		return nil, err
	}
	return newSessionBeadSnapshot(sessions), nil
}

func newSessionBeadSnapshot(beadsIn []beads.Bead) *sessionBeadSnapshot {
	filtered := make([]beads.Bead, 0, len(beadsIn))
	beadIDByAgentName := make(map[string]string)
	beadIDByTemplateHint := make(map[string]string)
	sessionNameByAgentName := make(map[string]string)
	sessionNameByTemplateHint := make(map[string]string)

	openInfos := make([]sessionpkg.Info, 0, len(beadsIn))

	for _, b := range beadsIn {
		if b.Status == "closed" {
			continue
		}
		filtered = append(filtered, b)
		openInfos = append(openInfos, sessionpkg.InfoFromPersistedBead(b))

		sn := b.Metadata["session_name"]
		if sn == "" {
			continue
		}
		isCanonicalNamed := strings.TrimSpace(b.Metadata["configured_named_identity"]) != ""
		if agentName := sessionBeadAgentName(b); agentName != "" {
			if isPoolManagedSessionBead(b) && agentName == b.Metadata["template"] {
				if stamped := stampedPoolQualifiedIdentity(b); stamped != "" {
					agentName = stamped
				} else if !isCanonicalPoolManagedSessionBeadForTemplate(b, agentName) {
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
		openInfos:                 openInfos,
		beadIDByAgentName:         beadIDByAgentName,
		beadIDByTemplateHint:      beadIDByTemplateHint,
		sessionNameByAgentName:    sessionNameByAgentName,
		sessionNameByTemplateHint: sessionNameByTemplateHint,
	}
}

// newSessionBeadSnapshotFromInfos builds a snapshot from a typed session.Info
// feed instead of raw beads. It populates ONLY openInfos — the non-closed
// entries (filtered by info.Closed) in the caller's order. The raw
// open []beads.Bead slice and the agent/template index maps are left nil
// because this constructor backs resolvePreservedConfiguredNamedSessionTemplate's
// feed, whose sole reachable snapshot read is OpenInfos(); the beadNames
// pre-seed short-circuits FindSessionNameByTemplate, so the index maps are never
// consulted on that path. Do NOT call Open(), the raw Find* methods, or the
// Find*ByTemplate index lookups on a snapshot built this way — they return
// empty. This is the front-door replacement for newSessionBeadSnapshot(ordered)
// at the reconciler's mid-tick preserve call: feeding the live infoByID rather
// than the raw working set keeps membership tracking mid-tick closes once the
// raw Status lockstep is dropped.
func newSessionBeadSnapshotFromInfos(infos []sessionpkg.Info) *sessionBeadSnapshot {
	openInfos := make([]sessionpkg.Info, 0, len(infos))
	for _, in := range infos {
		if in.Closed {
			continue
		}
		openInfos = append(openInfos, in)
	}
	return &sessionBeadSnapshot{openInfos: openInfos}
}

// replaceOpenLocked replaces the snapshot's open set and rebuilt lookup maps
// from `open`. Callers must hold s.mu.
func (s *sessionBeadSnapshot) replaceOpenLocked(open []beads.Bead) {
	rebuilt := newSessionBeadSnapshot(open)
	s.open = rebuilt.open
	s.openInfos = rebuilt.openInfos
	s.beadIDByAgentName = rebuilt.beadIDByAgentName
	s.beadIDByTemplateHint = rebuilt.beadIDByTemplateHint
	s.sessionNameByAgentName = rebuilt.sessionNameByAgentName
	s.sessionNameByTemplateHint = rebuilt.sessionNameByTemplateHint
}

func (s *sessionBeadSnapshot) add(bead beads.Bead) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	open := make([]beads.Bead, 0, len(s.open)+1)
	open = append(open, s.open...)
	open = append(open, bead)
	s.replaceOpenLocked(open)
}

func (s *sessionBeadSnapshot) Open() []beads.Bead {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]beads.Bead, len(s.open))
	copy(result, s.open)
	return result
}

// OpenInfos is the typed mirror of Open: a copy of the session.Info projection
// of every open bead, in the same order as Open(). OpenInfos()[i] equals
// InfoFromPersistedBead(Open()[i]) for all i.
func (s *sessionBeadSnapshot) OpenInfos() []sessionpkg.Info {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]sessionpkg.Info, len(s.openInfos))
	copy(result, s.openInfos)
	return result
}

func (s *sessionBeadSnapshot) FindSessionNameByTemplate(template string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sn := s.sessionNameByAgentName[template]; sn != "" {
		return sn
	}
	return s.sessionNameByTemplateHint[template]
}

func (s *sessionBeadSnapshot) FindSessionBeadByTemplate(template string) (beads.Bead, bool) {
	if s == nil {
		return beads.Bead{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id := s.beadIDByAgentName[template]; id != "" {
		return s.findByIDLocked(id)
	}
	if id := s.beadIDByTemplateHint[template]; id != "" {
		return s.findByIDLocked(id)
	}
	return beads.Bead{}, false
}

// FindInfoByTemplate is the typed mirror of FindSessionBeadByTemplate: it
// returns the session.Info projection of the same bead that method would
// resolve for template.
func (s *sessionBeadSnapshot) FindInfoByTemplate(template string) (sessionpkg.Info, bool) {
	if s == nil {
		return sessionpkg.Info{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id := s.beadIDByAgentName[template]; id != "" {
		return s.findInfoByIDLocked(id)
	}
	if id := s.beadIDByTemplateHint[template]; id != "" {
		return s.findInfoByIDLocked(id)
	}
	return sessionpkg.Info{}, false
}

func (s *sessionBeadSnapshot) FindByID(id string) (beads.Bead, bool) {
	if s == nil || strings.TrimSpace(id) == "" {
		return beads.Bead{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findByIDLocked(id)
}

// findByIDLocked is the inner lookup; callers must hold at least s.mu.RLock.
func (s *sessionBeadSnapshot) findByIDLocked(id string) (beads.Bead, bool) {
	for _, bead := range s.open {
		if bead.ID == id {
			return bead, true
		}
	}
	return beads.Bead{}, false
}

// FindInfoByID is the typed mirror of FindByID: it returns the session.Info
// projection of the same bead FindByID would return for id.
func (s *sessionBeadSnapshot) FindInfoByID(id string) (sessionpkg.Info, bool) {
	if s == nil || strings.TrimSpace(id) == "" {
		return sessionpkg.Info{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findInfoByIDLocked(id)
}

// findInfoByIDLocked is the typed inner lookup; callers must hold at least
// s.mu.RLock. open and openInfos are kept in lockstep order, so the matching
// index into open yields the corresponding Info.
func (s *sessionBeadSnapshot) findInfoByIDLocked(id string) (sessionpkg.Info, bool) {
	for i, bead := range s.open {
		if bead.ID == id {
			return s.openInfos[i], true
		}
	}
	return sessionpkg.Info{}, false
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, bead := range s.open {
		if strings.TrimSpace(bead.Metadata["configured_named_identity"]) != identity {
			continue
		}
		return bead, true
	}
	return beads.Bead{}, false
}

// FindInfoByNamedIdentity is the typed mirror of FindSessionBeadByNamedIdentity:
// it returns the session.Info projection of the same bead that method would
// resolve for identity. open and openInfos share an index, so the first
// matching bead's Info is returned.
func (s *sessionBeadSnapshot) FindInfoByNamedIdentity(identity string) (sessionpkg.Info, bool) {
	if s == nil || strings.TrimSpace(identity) == "" {
		return sessionpkg.Info{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, bead := range s.open {
		if strings.TrimSpace(bead.Metadata["configured_named_identity"]) != identity {
			continue
		}
		return s.openInfos[i], true
	}
	return sessionpkg.Info{}, false
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

// stampedPoolQualifiedIdentityInfo is the session.Info mirror of
// stampedPoolQualifiedIdentity.
func stampedPoolQualifiedIdentityInfo(i sessionpkg.Info) string {
	if !isPoolManagedSessionInfo(i) {
		return ""
	}
	slot, err := strconv.Atoi(strings.TrimSpace(i.PoolSlot))
	if err != nil || slot <= 0 {
		return ""
	}
	template := strings.TrimSpace(i.Template)
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
