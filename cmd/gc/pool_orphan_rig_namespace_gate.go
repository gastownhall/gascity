package main

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// poolRigNamespaceGate is the pool dead-assignee sweep's gate for claims in a
// rig with federation = "rig": a claim whose identity the rig's namespace
// does not admit (outside "<rig>/" and none of this city's own pool sessions
// of the rig) belongs to another city that attaches the same store, or to a
// person, so the sweep keeps it without probing liveness. Those claims are
// counted per rig, not named: on a shared store they are the normal state,
// and the count is logged only when it changes. A failed session lookup also
// keeps the claim; that one is named in the protected summary.
//
// With the guarded rig store in place such rows rarely reach the sweep; this
// is the second layer.
type poolRigNamespaceGate struct {
	cfg      *config.City
	sessions ownSessionResolver
	keepLog  *deadAssigneeKeepLog
	// kept counts the claims kept per rig name this pass.
	kept map[string]int
}

// newPoolRigNamespaceGate returns the gate for one sweep pass, or nil when no
// rig of cfg is guarded. Its session resolver is poolRigNamespaceSessions'.
func newPoolRigNamespaceGate(cfg *config.City, cityPath string, sessionStore beads.SessionStore, keepLog *deadAssigneeKeepLog) *poolRigNamespaceGate {
	if !cityHasGuardedRig(cfg) {
		// Still report a change to zero when the last guarded rig is gone.
		keepLog.logNamespaceKept("")
		return nil
	}
	return &poolRigNamespaceGate{cfg: cfg, sessions: poolRigNamespaceSessions(cfg, cityPath, sessionStore), keepLog: keepLog}
}

// poolRigNamespaceSessions returns the own-session resolver the sweep judges
// a guarded rig's claims with:
//
//   - in a controller, the city's one resolver, registered when its
//     controller state was built (rigNamespaceSessions): the same resolver,
//     snapshot and remembered answers as the city's guarded rig stores, kept
//     across passes, pointed at the sessions store on every reload, and
//     reset by the controller's beginPass;
//   - in a controller process where the city has registered none, a resolver
//     that admits no session, so every pool claim in a guarded rig is kept and
//     named, as the guarded stores refuse them;
//   - outside a controller (the one-shot pass of gc start), a resolver over
//     sessionStore, the city's sessions store, for this call.
func poolRigNamespaceSessions(cfg *config.City, cityPath string, sessionStore beads.SessionStore) ownSessionResolver {
	if h, controllerProcess := registeredRigNamespaceSessions(cityPath); h != nil {
		return h
	} else if controllerProcess {
		return unavailableSessionResolver{err: fmt.Errorf("city %s: %w (its controller has not registered its resolver)", cityPath, errRigNamespaceSessionsUnavailable)}
	}
	return newControllerSessionResolver(cityPath, nil, func() (beads.SessionStore, error) {
		if noSessionStore(sessionStore) {
			return beads.SessionStore{}, fmt.Errorf("no session store")
		}
		return sessionStore, nil
	}, ownSessionIDPrefixes(cfg, sessionStore)...)
}

// registeredRigNamespaceSessions returns the controller resolver registered
// for the city at cityPath (registerRigNamespaceSessions), or nil, and
// whether this process runs a controller.
func registeredRigNamespaceSessions(cityPath string) (*rigNamespaceSessions, bool) {
	rigNamespaceResolvers.mu.Lock()
	defer rigNamespaceResolvers.mu.Unlock()
	return rigNamespaceResolvers.controllers[filepath.Clean(cityPath)], rigNamespaceResolvers.controllerProcess > 0
}

// namespaceFor returns the namespace guarding the claim's store: the rig its
// leg names ("rig:<name>"), or, when the leg is unknown or names no
// configured rig, the guarded rig whose bead prefix workID carries.
func (n *poolRigNamespaceGate) namespaceFor(workID, leg string) (*rigNamespace, bool) {
	if name, ok := strings.CutPrefix(strings.TrimSpace(leg), "rig:"); ok {
		if ns, guarded, known := rigNamespaceForRigName(n.cfg, name, n.sessions); known {
			return ns, guarded
		}
	} else if strings.TrimSpace(leg) != "" {
		// A city-scope leg: not a rig's store.
		return nil, false
	}
	return rigNamespaceForBeadID(n.cfg, workID, n.sessions)
}

// keeps reports whether the claim must be kept because it sits in a guarded
// rig and the rig's namespace does not admit assignee. A lookup failure keeps
// it too, recorded on g as a lookup error and named as protected.
func (n *poolRigNamespaceGate) keeps(g *foreignClaimGate, assignee, workID, leg string) bool {
	if n == nil {
		return false
	}
	ns, guarded := n.namespaceFor(workID, leg)
	if !guarded {
		return false
	}
	a := ns.admission(assignee)
	if a.Admitted() {
		return false
	}
	if a.Err != nil {
		if g.lookupErr == nil {
			g.lookupErr = a.Err
		}
		g.protected.add(assignee, workID)
		return true
	}
	if n.kept == nil {
		n.kept = make(map[string]int)
	}
	name := ns.rigName()
	if name == "" {
		name = "(ambiguous)"
	}
	n.kept[name]++
	return true
}

// summary renders the pass's count line, or "" when nothing was kept.
func (n *poolRigNamespaceGate) summary() string {
	if n == nil || len(n.kept) == 0 {
		return ""
	}
	rigs := make([]string, 0, len(n.kept))
	total := 0
	for rig, count := range n.kept {
		rigs = append(rigs, rig)
		total += count
	}
	sort.Strings(rigs)
	parts := make([]string, 0, len(rigs))
	for _, rig := range rigs {
		parts = append(parts, fmt.Sprintf("%s: %d", rig, n.kept[rig]))
	}
	return fmt.Sprintf("releaseOrphanedPoolAssignments: kept %d claims outside guarded rig namespaces (%s)", total, strings.Join(parts, ", "))
}

// log logs the pass's count line when it differs from the last one logged.
func (n *poolRigNamespaceGate) log() {
	if n == nil {
		return
	}
	n.keepLog.logNamespaceKept(n.summary())
}

// logNamespaceKept logs line, the sweep's count of claims kept outside
// guarded rig namespaces, when it changed since the last pass, and reports
// whether it logged. An empty line after a non-empty one logs that none are
// kept any more. A nil log logs every non-empty line.
func (l *deadAssigneeKeepLog) logNamespaceKept(line string) bool {
	if l == nil {
		if line == "" {
			return false
		}
		log.Print(line)
		return true
	}
	l.mu.Lock()
	prev := l.namespaceKept
	l.namespaceKept = line
	l.mu.Unlock()
	if prev == line {
		return false
	}
	if line == "" {
		log.Print("releaseOrphanedPoolAssignments: kept no claims outside guarded rig namespaces")
		return true
	}
	log.Print(line)
	return true
}
