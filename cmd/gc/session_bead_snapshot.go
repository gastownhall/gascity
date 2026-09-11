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
	// mu guards openInfos/openCircuits + the four lookup maps. addInfo() (called
	// from the pool create/reuse path) can fire from multiple goroutines when
	// realizePoolDesiredSessions parallelizes pool session bead creates across
	// distinct aliases — see gastownhall/gascity#2319. All read methods take RLock;
	// addInfo() takes Lock.
	mu sync.RWMutex
	// openInfos is the typed session.Info projection of every open session, the
	// snapshot's sole domain surface (the raw-bead half was deleted in WI-7 W-delete;
	// callers that genuinely need raw beads read the store directly).
	openInfos []sessionpkg.Info
	// openCircuits is the persisted circuit-breaker cluster projection, in lockstep
	// order with openInfos. The reconciler tick feed (OpenForReconcile) pairs it with
	// openInfos so the circuit cluster — deliberately off session.Info — reaches
	// Phase 0.5 without a per-id store Get. An Info-fed snapshot (FromInfos) has no
	// backing circuit metadata, so its entries are the zero CircuitState.
	openCircuits              []sessionpkg.CircuitState
	beadIDByAgentName         map[string]string
	beadIDByTemplateHint      map[string]string
	sessionNameByAgentName    map[string]string
	sessionNameByTemplateHint map[string]string
	loadErr                   error
	// fingerprint is the config-change cache key (sessionBeadSnapshotFingerprint):
	// a hash of every open bead's ID + Status + Assignee + ALL metadata keys. It is
	// computed at the store edge from the raw beads — session.Info deliberately drops
	// unknown keys, so it CANNOT be recomputed after the raw half is gone — and
	// carried here as a field. Set at construction (before publication, like loadErr);
	// empty on snapshots built without raw beads (they never reach the getter).
	fingerprint string
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
	s := newSessionBeadSnapshotFromInfos(nil)
	// loadErr is set during construction, before s is published to any other
	// goroutine, so no s.mu lock is needed here even though LoadError() reads
	// it under RLock.
	s.loadErr = err
	return s
}

func loadSessionBeadSnapshot(store beads.Store) (*sessionBeadSnapshot, error) {
	if store == nil {
		snap := newSessionBeadSnapshotFromInfos(nil)
		snap.fingerprint = sessionpkg.SetFingerprint(nil)
		return snap, nil
	}
	// Typed reconcile feed via the session front door: the same Type+Label union
	// ListAllSessionBeads applied (so canonical session beads that lost their
	// gc:session label after a crash or migration still surface — a label-only query
	// strands them invisible to the reconciler, which then never heals their
	// state=awake metadata after a runtime is lost, and their alias reservations live
	// forever blocking pool replacements), projected to ReconcileSession rows and
	// paired with the raw-bead config-change fingerprint in ONE list. The snapshot no
	// longer holds raw beads; the fingerprint is computed edge-side (it hashes ALL
	// metadata, which Info drops) and carried as a field.
	//
	// Closed history is intentionally not loaded here — the reconciler calls this
	// several times per tick and closed history grows without bound. Callers that need
	// a closed record must fetch that one ID explicitly.
	rows, fingerprint, err := sessionFrontDoor(store).ListAllForReconcileWithFingerprint(sessionpkg.ListAllOptions{})
	if err != nil {
		return nil, err
	}
	snap := newSessionBeadSnapshotFromReconcileRows(rows)
	snap.fingerprint = fingerprint
	return snap, nil
}

// newSessionBeadSnapshotFromInfos builds a snapshot from a typed session.Info feed.
// It populates openInfos AND the four agent/template index maps, reading typed Info
// fields through the classifier twins (sessionBeadAgentNameInfo,
// isPoolManagedSessionInfo, stampedPoolQualifiedIdentityInfo,
// isCanonicalPoolManagedSessionInfoForTemplate). The index precedence — canonical
// configured_named beads win the agent/template index, pool-managed beads skip the
// template-hint index, and common_name provides the last-resort hint — is pinned by
// TestSessionBeadSnapshotFromReconcileRowsIndexPrecedence across the fixture corpus so
// an index-precedence divergence (which strands named sessions invisibly) fails the
// build. Circuits are zero-valued (an Info-only feed carries no circuit metadata).
func newSessionBeadSnapshotFromInfos(infos []sessionpkg.Info) *sessionBeadSnapshot {
	return newSessionBeadSnapshotFromInfosAndCircuits(infos, nil)
}

// newSessionBeadSnapshotFromReconcileRows builds a snapshot from a typed
// ReconcileSession feed, retaining each row's circuit cluster alongside its Info. It
// is the reconciler-tick + store-load constructor: the tick's working set is fed as
// rows (OpenForReconcile / Store.ListAllForReconcile), and the retire/heal folds
// mutate rows in place, so the snapshot rebuilt from them must keep the circuit
// projections OpenForReconcile needs.
func newSessionBeadSnapshotFromReconcileRows(rows []sessionpkg.ReconcileSession) *sessionBeadSnapshot {
	infos := make([]sessionpkg.Info, len(rows))
	circuits := make([]sessionpkg.CircuitState, len(rows))
	for i := range rows {
		infos[i] = rows[i].Info
		circuits[i] = rows[i].Circuit
	}
	return newSessionBeadSnapshotFromInfosAndCircuits(infos, circuits)
}

// newSessionBeadSnapshotFromInfosAndCircuits is the shared index-map builder
// behind newSessionBeadSnapshotFromInfos and newSessionBeadSnapshotFromReconcileRows.
// circuits, when non-nil, is parallel to infos (same length, same order) and is
// filtered in lockstep with the closed-drop; a nil circuits yields the zero
// CircuitState for every open row (an Info-fed snapshot has no circuit metadata).
func newSessionBeadSnapshotFromInfosAndCircuits(infos []sessionpkg.Info, circuits []sessionpkg.CircuitState) *sessionBeadSnapshot {
	beadIDByAgentName := make(map[string]string)
	beadIDByTemplateHint := make(map[string]string)
	sessionNameByAgentName := make(map[string]string)
	sessionNameByTemplateHint := make(map[string]string)

	openInfos := make([]sessionpkg.Info, 0, len(infos))
	openCircuits := make([]sessionpkg.CircuitState, 0, len(infos))

	for i, in := range infos {
		if in.Closed {
			continue
		}
		openInfos = append(openInfos, in)
		if circuits != nil {
			openCircuits = append(openCircuits, circuits[i])
		} else {
			openCircuits = append(openCircuits, sessionpkg.CircuitState{})
		}

		sn := in.SessionNameMetadata
		if sn == "" {
			continue
		}
		isCanonicalNamed := strings.TrimSpace(in.ConfiguredNamedIdentity) != ""
		if agentName := sessionBeadAgentNameInfo(in); agentName != "" {
			if isPoolManagedSessionInfo(in) && agentName == in.Template {
				if stamped := stampedPoolQualifiedIdentityInfo(in); stamped != "" {
					agentName = stamped
				} else if !isCanonicalPoolManagedSessionInfoForTemplate(in, agentName) {
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
				beadIDByAgentName[agentName] = in.ID
				sessionNameByAgentName[agentName] = sn
			}
		}
		if isPoolManagedSessionInfo(in) {
			continue
		}
		if template := in.Template; template != "" {
			if _, exists := sessionNameByTemplateHint[template]; !exists || isCanonicalNamed {
				beadIDByTemplateHint[template] = in.ID
				sessionNameByTemplateHint[template] = sn
			}
		}
		if commonName := in.CommonName; commonName != "" {
			if _, exists := sessionNameByTemplateHint[commonName]; !exists {
				beadIDByTemplateHint[commonName] = in.ID
				sessionNameByTemplateHint[commonName] = sn
			}
		}
	}

	return &sessionBeadSnapshot{
		openInfos:                 openInfos,
		openCircuits:              openCircuits,
		beadIDByAgentName:         beadIDByAgentName,
		beadIDByTemplateHint:      beadIDByTemplateHint,
		sessionNameByAgentName:    sessionNameByAgentName,
		sessionNameByTemplateHint: sessionNameByTemplateHint,
	}
}

// addInfo appends a freshly created/reopened session's projected Info to the
// snapshot's typed half so same-cycle selection observes it. The pool create/reuse
// path inserts session.Info here (its typed create front door returns Info, not a raw
// bead). It rebuilds the agent/template index maps from the extended openInfos via the
// equivalence-proven Info constructor while PRESERVING each existing row's circuit
// cluster and appending the zero CircuitState for the new row (a fresh bead carries no
// circuit metadata).
//
// Consumers read the typed half — the build's own reuse scans (OpenInfos) and the
// reconcile tick (which re-loads the snapshot from the store after buildDesiredState) —
// so they observe the new session directly. The sync path re-lists raw beads from the
// store every cycle, so a just-created session_name is durably visible there too
// (CreateSessionInfo persists the bead before projecting it). Under Lock; safe for the
// parallel pool-create fan-out (gastownhall/gascity#2319).
func (s *sessionBeadSnapshot) addInfo(info sessionpkg.Info) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	infos := make([]sessionpkg.Info, 0, len(s.openInfos)+1)
	infos = append(infos, s.openInfos...)
	infos = append(infos, info)
	circuits := make([]sessionpkg.CircuitState, 0, len(s.openCircuits)+1)
	circuits = append(circuits, s.openCircuits...)
	circuits = append(circuits, sessionpkg.CircuitState{})
	rebuilt := newSessionBeadSnapshotFromInfosAndCircuits(infos, circuits)
	s.openInfos = rebuilt.openInfos
	s.openCircuits = rebuilt.openCircuits
	s.beadIDByAgentName = rebuilt.beadIDByAgentName
	s.beadIDByTemplateHint = rebuilt.beadIDByTemplateHint
	s.sessionNameByAgentName = rebuilt.sessionNameByAgentName
	s.sessionNameByTemplateHint = rebuilt.sessionNameByTemplateHint
}

// OpenInfos is a copy of the session.Info projection of every open session, in the
// snapshot's canonical order (the order OpenForReconcile also uses).
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

// WriteBackReconcileInfos folds the reconciler's post-tick Info snapshot back onto
// the carrier's open rows, so post-tick consumers observe the tick's in-memory
// heals / dedup-retires / closes. Before W-tick the reconciler mutated the raw
// open beads in place, so the RESULTS trace recorder saw post-tick values; now the
// tick works on separate ReconcileSession rows, and this writeback restores that
// post-tick observation. For each open row whose id appears in infoByID the row's
// Info is replaced with the post-tick Info; rows absent from infoByID (e.g. a
// session created mid-tick via addInfo) keep their current Info. Circuits are
// untouched. Under Lock.
func (s *sessionBeadSnapshot) WriteBackReconcileInfos(infoByID map[string]sessionpkg.Info) {
	if s == nil || len(infoByID) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.openInfos {
		if post, ok := infoByID[s.openInfos[i].ID]; ok {
			s.openInfos[i] = post
		}
	}
}

// OpenForReconcile is the reconciler tick feed: a copy of every open session's
// ReconcileSession (Info paired with its circuit-breaker cluster), in the same order as
// OpenInfos(). OpenForReconcile()[i].Info equals OpenInfos()[i] and
// OpenForReconcile()[i].Circuit equals that session's circuit projection.
func (s *sessionBeadSnapshot) OpenForReconcile() []sessionpkg.ReconcileSession {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]sessionpkg.ReconcileSession, len(s.openInfos))
	for i := range s.openInfos {
		circuit := sessionpkg.CircuitState{}
		if i < len(s.openCircuits) {
			circuit = s.openCircuits[i]
		}
		result[i] = sessionpkg.ReconcileSession{Info: s.openInfos[i], Circuit: circuit}
	}
	return result
}

// ApplyOpenInfoPatch folds a metadata patch onto the matching open row's Info
// (openInfos[i] where openInfos[i].ID == id), via Info.ApplyPatch, under Lock. It
// is the explicit carrier for the stranded-throttle marker (§2.5n): before its
// durable SetMarker, emitSessionStrandedDiagnostic folds the throttle key here so
// a REUSED snapshot's OpenForReconcile row carries the marker even when the store
// write failed — reproducing the emit-once guarantee the shared-metadata-map
// aliasing used to provide accidentally. No-op when id is absent.
func (s *sessionBeadSnapshot) ApplyOpenInfoPatch(id string, patch sessionpkg.MetadataPatch) {
	if s == nil || strings.TrimSpace(id) == "" || len(patch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.openInfos {
		if s.openInfos[i].ID == id {
			s.openInfos[i] = s.openInfos[i].ApplyPatch(patch)
			return
		}
	}
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

// FindInfoByTemplate returns the session.Info of the bead the template resolves to,
// preferring the agent-name index over the template-hint index.
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

// FindInfoByID returns the session.Info of the open session with the given id.
func (s *sessionBeadSnapshot) FindInfoByID(id string) (sessionpkg.Info, bool) {
	if s == nil || strings.TrimSpace(id) == "" {
		return sessionpkg.Info{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findInfoByIDLocked(id)
}

// findInfoByIDLocked is the inner lookup over openInfos; callers must hold at least
// s.mu.RLock.
func (s *sessionBeadSnapshot) findInfoByIDLocked(id string) (sessionpkg.Info, bool) {
	for _, info := range s.openInfos {
		if info.ID == id {
			return info, true
		}
	}
	return sessionpkg.Info{}, false
}

// FindInfoByNamedIdentity returns the session.Info of the open session whose
// configured named identity matches (trimmed Info.ConfiguredNamedIdentity).
func (s *sessionBeadSnapshot) FindInfoByNamedIdentity(identity string) (sessionpkg.Info, bool) {
	if s == nil || strings.TrimSpace(identity) == "" {
		return sessionpkg.Info{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, info := range s.openInfos {
		if strings.TrimSpace(info.ConfiguredNamedIdentity) != identity {
			continue
		}
		return info, true
	}
	return sessionpkg.Info{}, false
}

// stampedPoolQualifiedIdentityInfo derives the qualified pool instance identity
// ("scope/name-slot") from a session.Info, or "" when the session is not a slotted
// pool-managed session.
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
