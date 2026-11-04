package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hookRigGuard is the rig namespace guard of one `gc hook --claim`
// invocation. The hook claims through a raw bd subprocess store, not through
// a guarded Go store, so it checks every candidate and every claim-cycle
// write itself: a candidate whose bead id carries a guarded rig's prefix is
// dropped when it is Foreign, and no claim, promotion, reclaim, adoption
// re-stamp, workflow-stop close, done-workflow retirement or continuation pin
// is attempted (and no bd process spawned for it) unless the actor is
// admitted by that rig. A retirement leaves the workflow's Foreign rows open.
//
// A row is judged by the leg it was read from (rigNamespaceForLeg): every
// row on a guarded rig's leg by that rig, whatever its id prefix, and a
// prefix that maps to a different guarded rig (or to two) admits nothing. On
// a leg that is not a guarded rig's store, the prefix alone decides. Session
// provenance comes from a CLI resolver over this city's sessions store,
// opened as cliStores(cityPath, cityStore).Sessions(), never a rig store.
//
// A nil guard checks nothing; claimOps installs one only when the city has a
// guarded rig, so an unguarded city runs byte-for-byte as before.
type hookRigGuard struct {
	cityPath string
	cfg      *config.City
	sessions ownSessionResolver
	// legSet reports that forLeg bound the guard to one leg; leg is that
	// leg's guarded namespace, nil when the leg is not a guarded rig's store.
	legSet bool
	leg    *rigNamespace
}

// newHookRigGuard returns the guard for a city, or nil when the city guards
// no rig.
func newHookRigGuard(cityPath string, cfg *config.City, sessions ownSessionResolver) *hookRigGuard {
	if !cityHasGuardedRig(cfg) {
		return nil
	}
	return &hookRigGuard{cityPath: cityPath, cfg: cfg, sessions: sessions}
}

// forLeg returns the guard bound to one leg of the claim's store fan-out.
// The leg's store is named three ways: the leg's rig (hookStore.rig), the
// BEADS_DIR its bd commands run with, and the directory they run in. Each
// that names a guarded rig's store is a candidate; one guarded rig named by
// every candidate binds the leg, and candidates that disagree (or a store
// the shared helper refuses) admit nothing.
func (g *hookRigGuard) forLeg(rigName, dir string, env ...string) *hookRigGuard {
	if g == nil {
		return nil
	}
	bound := *g
	bound.legSet = true
	var candidates []*rigNamespace
	add := func(ns *rigNamespace) {
		if ns != nil {
			candidates = append(candidates, ns)
		}
	}
	if _, _, known := rigNamespaceForRigName(g.cfg, strings.TrimSpace(rigName), g.sessions); known {
		add(guardedRigNamespaceForRigName(g.cfg, g.cityPath, rigName, g.sessions))
	}
	if beadsDir := hookClaimEnvValue(env, "BEADS_DIR"); beadsDir != "" {
		add(guardedRigNamespaceForDir(g.cfg, g.cityPath, filepath.Dir(filepath.Clean(beadsDir)), g.sessions))
	}
	add(guardedRigNamespaceForDir(g.cfg, g.cityPath, dir, g.sessions))
	bound.leg = agreeingRigNamespace(candidates, g.sessions)
	return &bound
}

// agreeingRigNamespace returns the one guarded namespace every candidate
// names, nil for none, and a deny-all namespace when they disagree or one
// of them is already deny-all (which keeps its reason).
func agreeingRigNamespace(candidates []*rigNamespace, sessions ownSessionResolver) *rigNamespace {
	if len(candidates) == 0 {
		return nil
	}
	first := candidates[0]
	for _, ns := range candidates {
		if ns.namespace() == "" {
			return ns
		}
		if ns.rigName() != first.rigName() {
			return denyAllRigNamespaceBecause(sessions, fmt.Errorf("the leg's rig, BEADS_DIR and directory name different guarded rigs (%q, %q)", first.rigName(), ns.rigName()))
		}
	}
	return first
}

// boundTo returns the guard bound to the leg at dir, keeping a binding
// forLeg already made.
func (g *hookRigGuard) boundTo(dir string) *hookRigGuard {
	if g == nil || g.legSet {
		return g
	}
	return g.forLeg("", dir)
}

// namespaceFor returns the guarded namespace a row with id beadID on this
// guard's leg is judged by, or nil.
func (g *hookRigGuard) namespaceFor(beadID string) *rigNamespace {
	if g == nil {
		return nil
	}
	return rigNamespaceForLeg(g.leg, g.cfg, strings.TrimSpace(beadID), g.sessions)
}

// filterCandidates drops the candidates a guarded rig does not let this city
// act on: Foreign rows, judged from the row in hand (no read). Unattributed
// rows are kept; their claims still need an admitted actor.
func (g *hookRigGuard) filterCandidates(candidates []beads.Bead, stderr io.Writer) []beads.Bead {
	if g == nil || len(candidates) == 0 {
		return candidates
	}
	kept := candidates[:0:0]
	for _, c := range candidates {
		ns := g.namespaceFor(c.ID)
		if ns == nil {
			kept = append(kept, c)
			continue
		}
		cls := ns.classifyRow(c)
		if cls.Class != rowForeign {
			kept = append(kept, c)
			continue
		}
		owner := cls.Value
		if ns.namespace() == "" {
			fmt.Fprintf(stderr, "gc hook --claim: skipping %s: rig %s (federation = \"rig\") admits no row in this scope\n", c.ID, rigNamespaceLabel(ns)) //nolint:errcheck
			continue
		}
		if cls.Err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: skipping %s: rig %s serves only %s (federation = \"rig\"); its owner %q could not be checked: %v\n", c.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), owner, cls.Err) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stderr, "gc hook --claim: skipping %s: rig %s serves only %s (federation = \"rig\"); its owner %q is outside it\n", c.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), owner) //nolint:errcheck
	}
	return kept
}

// claimRefusal returns the refusal of a claim-cycle write on bead as each of
// actors, or nil when the write may proceed. The row must not be Foreign and
// every non-empty actor must be admitted; an empty actor list or an empty
// actor is refused on a guarded rig, since the write would then run as an
// identity nobody checked.
func (g *hookRigGuard) claimRefusal(bead beads.Bead, actors ...string) error {
	ns := g.namespaceFor(bead.ID)
	if ns == nil {
		return nil
	}
	if ns.namespace() == "" {
		return newRigNamespacePathError("gc hook --claim: not claiming %s: rig %s (federation = \"rig\") admits no row in this scope", bead.ID, rigNamespaceLabel(ns))
	}
	if cls := ns.classifyRow(bead); cls.Class == rowForeign {
		return newRigNamespacePathError("gc hook --claim: not claiming %s: rig %s (federation = \"rig\") serves only %s; its owner %q is outside it", bead.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), cls.Value)
	}
	if len(actors) == 0 {
		actors = []string{""}
	}
	for _, actor := range actors {
		if rigNamespacePinAdmitted(ns, actor) {
			continue
		}
		return newRigNamespacePathError("gc hook --claim: not claiming %s: rig %s (federation = \"rig\") serves only %s and this city's pool sessions; claim actor %q is outside it%s", bead.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), strings.TrimSpace(actor), rigNamespaceActorHint(ns, actor))
	}
	return nil
}

// retireFilter returns the rows a done-workflow retirement on this guard's
// leg may close: every row but a Foreign one (another city's step or root
// that carries the workflow's root id, such as a step handed off to it). It
// returns nil, which closes every row, when the city guards no rig.
func (g *hookRigGuard) retireFilter() func(beads.Bead) bool {
	if g == nil {
		return nil
	}
	return func(row beads.Bead) bool {
		ns := g.namespaceFor(row.ID)
		return ns == nil || ns.classify(row) != rowForeign
	}
}

// refuseClaim reports a refused claim-cycle write on stderr and returns true,
// or returns false when the guard admits it. A refusal is final for this
// invocation: the caller moves on without retrying and without spawning bd.
func (g *hookRigGuard) refuseClaim(bead beads.Bead, stderr io.Writer, actors ...string) bool {
	if g == nil {
		return false
	}
	err := g.claimRefusal(bead, actors...)
	if err == nil {
		return false
	}
	fmt.Fprintln(stderr, err.Error()) //nolint:errcheck
	return true
}

// continuationPin returns the identity a continuation sibling of bead may be
// pinned to on a guarded rig: the session's bead id when it is admitted (a
// pool's 128-bit id), else the claim identity when it is admitted (the alias
// of a rig-scoped named session), else "" (do not pin; the sibling stays
// routed). Off a guarded rig it is continuationPinAssignee.
func (g *hookRigGuard) continuationPin(sibling beads.Bead, opts hookClaimOptions) string {
	ns := g.namespaceFor(sibling.ID)
	if ns == nil {
		return continuationPinAssignee(opts)
	}
	if ns.classify(sibling) == rowForeign {
		return ""
	}
	for _, v := range []string{opts.SessionID, opts.Assignee} {
		if rigNamespacePinAdmitted(ns, v) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// stripOwnerStamp removes the creation stamp from a claim-time metadata
// patch: only a guarded store may write it, and the hook's raw store is not
// one (the creation-stamp rule).
func stripOwnerStamp(patch map[string]string) map[string]string {
	for k := range patch {
		if strings.TrimSpace(k) == rigNamespaceOwnerStampKey {
			delete(patch, k)
		}
	}
	return patch
}
