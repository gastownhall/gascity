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
	openInfos []sessionpkg.Info
	// openCircuits is the persisted circuit-breaker cluster projection of open, in
	// lockstep order: openCircuits[i] == CircuitStateFromMetadata(open[i].Metadata).
	// The reconciler tick feed (OpenForReconcile) pairs it with openInfos so the
	// circuit cluster — deliberately off session.Info — reaches Phase 0.5 without a
	// per-id store Get. An Info-fed snapshot (FromInfos) has no backing circuit
	// metadata, so its entries are the zero CircuitState.
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
	openCircuits := make([]sessionpkg.CircuitState, 0, len(beadsIn))

	for _, b := range beadsIn {
		if b.Status == "closed" {
			continue
		}
		filtered = append(filtered, b)
		openInfos = append(openInfos, sessionpkg.InfoFromPersistedBead(b))
		openCircuits = append(openCircuits, sessionpkg.CircuitStateFromMetadata(b.Metadata))

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
		openCircuits:              openCircuits,
		beadIDByAgentName:         beadIDByAgentName,
		beadIDByTemplateHint:      beadIDByTemplateHint,
		sessionNameByAgentName:    sessionNameByAgentName,
		sessionNameByTemplateHint: sessionNameByTemplateHint,
		fingerprint:               sessionpkg.SessionSetFingerprint(filtered),
	}
}

// newSessionBeadSnapshotFromInfos builds a snapshot from a typed session.Info
// feed instead of raw beads. It is the full front-door constructor: it
// populates openInfos AND the four agent/template index maps, reading typed
// Info fields through the equivalence-proven classifier twins
// (sessionBeadAgentNameInfo, isPoolManagedSessionInfo,
// stampedPoolQualifiedIdentityInfo, isCanonicalPoolManagedSessionInfoForTemplate)
// in place of the raw metadata reads newSessionBeadSnapshot uses. The index
// precedence — canonical configured_named beads win the agent/template index,
// pool-managed beads skip the template-hint index, and common_name provides the
// last-resort hint — is identical to the raw constructor and pinned by
// TestSessionBeadSnapshotConstructorInfoEquivalence across the fixture corpus so
// an index-precedence divergence (which strands named sessions invisibly) fails
// the build.
//
// The raw open []beads.Bead slice is left nil: an Info-fed snapshot has no
// backing beads, so ONLY the bead-returning raw-half readers (Open, FindByID, the
// FindSessionBead* family) return empty on it — callers that need a raw
// beads.Bead must build the snapshot from beads via newSessionBeadSnapshot. The
// typed surface (OpenInfos, FindInfoByID, FindInfoByTemplate,
// FindInfoByNamedIdentity) is backed by openInfos and the index maps this
// constructor populates, so it works correctly on an Info-built snapshot.
func newSessionBeadSnapshotFromInfos(infos []sessionpkg.Info) *sessionBeadSnapshot {
	return newSessionBeadSnapshotFromInfosAndCircuits(infos, nil)
}

// newSessionBeadSnapshotFromReconcileRows builds a snapshot from a typed
// ReconcileSession feed, retaining each row's circuit cluster alongside its Info.
// It is the reconciler-tick constructor: the tick's working set is fed as rows
// (OpenForReconcile), and the retire/heal folds mutate rows in place, so the
// snapshot rebuilt from them must keep the circuit projections that OpenForReconcile
// needs. Like newSessionBeadSnapshotFromInfos, the raw open []beads.Bead half is
// left nil.
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

// replaceOpenLocked replaces the snapshot's open set and rebuilt lookup maps
// from `open`. Callers must hold s.mu.
func (s *sessionBeadSnapshot) replaceOpenLocked(open []beads.Bead) {
	rebuilt := newSessionBeadSnapshot(open)
	s.open = rebuilt.open
	s.openInfos = rebuilt.openInfos
	s.openCircuits = rebuilt.openCircuits
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

// addInfo appends a freshly created/reopened session's projected Info to the
// snapshot's typed half so same-cycle selection observes it. The pool create/reuse
// path (W-pool) inserts session.Info here now that it returns Info instead of a raw
// bead. It rebuilds the agent/template index maps from the extended openInfos via
// the equivalence-proven Info constructor while PRESERVING each existing row's
// circuit cluster and appending the zero CircuitState for the new row (a fresh bead
// carries no circuit metadata).
//
// The raw open []beads.Bead half is intentionally left untouched (addInfo holds no
// raw bead). Consumers that read the typed half — the build's own reuse scans
// (OpenInfos) and the reconcile tick (which re-loads the snapshot from the store
// after buildDesiredState) — observe the new session directly. The one same-cycle
// reader of the RAW half, the sync path via snapshotOrLoadSessionBeads, reconciles
// the resulting skew by reloading from the store whenever len(OpenInfos()) >
// len(Open()) (which holds iff addInfo ran this cycle), so sync never treats a
// just-created session_name as absent and mints a duplicate. The raw half is deleted
// with W-delete. Under Lock; safe for the parallel pool-create fan-out
// (gastownhall/gascity#2319).
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

// WI-6: the raw-bead half of sessionBeadSnapshot (Open / FindByID /
// FindSessionBeadByTemplate / FindSessionBeadByNamedIdentity /
// FindSessionNameByNamedIdentity and the raw stampedPoolQualifiedIdentity used by
// the constructor) survives WI-5. The W4 typed-half migration retired every
// reconciler-owned consumer, but Open() still has many non-front-door callers
// spanning cmd_start.go, build_desired_state.go, city_runtime.go and
// session_beads.go, and FindByID / FindSessionNameByNamedIdentity are still called
// from the WI-6-owned city_runtime.go / cmd_wait.go / providers.go lanes. Before
// deleting the raw half, grep the tree for the non-test call sites (e.g.
// `grep -rn --include='*.go' '\.Open()' cmd internal | grep -v _test.go`, and the
// same for FindByID / FindSessionNameByNamedIdentity) and migrate each onto
// OpenInfos()/FindInfoByID()/FindInfoByNamedIdentity() in WI-6.
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

// WriteBackReconcileInfos folds the reconciler's post-tick Info snapshot back onto
// the carrier's open rows, so post-tick consumers observe the tick's in-memory
// heals / dedup-retires / closes. Before W-tick the reconciler mutated the raw
// open beads in place, so the RESULTS trace recorder saw post-tick values; now the
// tick works on separate ReconcileSession rows, and this writeback restores that
// post-tick observation. For each open row whose id appears in infoByID the row's
// Info is replaced with the post-tick Info; rows absent from infoByID (e.g. a
// session created mid-tick via add) keep their current Info. Circuits and the raw
// open half are untouched. Under Lock.
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
// ReconcileSession (Info paired with its circuit-breaker cluster), in the same
// order as Open()/OpenInfos(). OpenForReconcile()[i].Info equals OpenInfos()[i]
// and OpenForReconcile()[i].Circuit equals the circuit projection of that bead.
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
// s.mu.RLock. It scans openInfos directly (not the raw open slice) so it works on
// both bead-built and Info-built snapshots: newSessionBeadSnapshotFromInfos leaves
// open nil but populates openInfos, and for a bead-built snapshot open/openInfos
// are lockstep so the first match is identical either way.
func (s *sessionBeadSnapshot) findInfoByIDLocked(id string) (sessionpkg.Info, bool) {
	for _, info := range s.openInfos {
		if info.ID == id {
			return info, true
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
// it returns the session.Info projection of the session whose configured named
// identity matches. It scans openInfos directly (matching the trimmed
// Info.ConfiguredNamedIdentity, which InfoFromPersistedBead carries verbatim) so
// it works on both bead-built and Info-built snapshots; for a bead-built snapshot
// open/openInfos are lockstep so the first match is identical to the raw form.
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
