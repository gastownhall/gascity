package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// hookRigGuard is the rig namespace guard of one `gc hook --claim`
// invocation. The hook claims through a raw bd subprocess store, not through
// a guarded Go store, so it checks every candidate and every claim-cycle
// write itself: a candidate whose bead id carries a guarded rig's prefix is
// dropped when it is Foreign, and no claim, promotion, reclaim, adoption
// re-stamp, workflow-stop close or continuation pin is attempted (and no bd
// process spawned for it) unless the actor is admitted by that rig.
//
// The namespace is resolved per bead id (rigNamespaceForBeadID): a prefix two
// guarded rigs share gives a deny-all namespace. Session provenance comes
// from a CLI resolver over this city's sessions store, opened through
// cliSessionStore(cityStore, ...), never a rig store.
//
// A nil guard checks nothing; claimOps installs one only when the city has a
// guarded rig, so an unguarded city runs byte-for-byte as before.
type hookRigGuard struct {
	cfg      *config.City
	sessions ownSessionResolver
}

// newHookRigGuard returns the guard for a city, or nil when the city guards
// no rig.
func newHookRigGuard(cfg *config.City, sessions ownSessionResolver) *hookRigGuard {
	if !cityHasGuardedRig(cfg) {
		return nil
	}
	return &hookRigGuard{cfg: cfg, sessions: sessions}
}

// namespaceFor returns the guarded namespace beadID belongs to, or nil.
func (g *hookRigGuard) namespaceFor(beadID string) *rigNamespace {
	if g == nil || strings.TrimSpace(beadID) == "" {
		return nil
	}
	ns, guarded := rigNamespaceForBeadID(g.cfg, beadID, g.sessions)
	if !guarded {
		return nil
	}
	return ns
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
		if cls.Err != nil {
			fmt.Fprintf(stderr, "gc hook --claim: skipping %s: rig %q serves only %s (federation = \"rig\"); its owner %q could not be checked: %v\n", c.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), owner, cls.Err) //nolint:errcheck
			continue
		}
		fmt.Fprintf(stderr, "gc hook --claim: skipping %s: rig %q serves only %s (federation = \"rig\"); its owner %q is outside it\n", c.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), owner) //nolint:errcheck
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
	if cls := ns.classifyRow(bead); cls.Class == rowForeign {
		return newRigNamespacePathError("gc hook --claim: not claiming %s: rig %q (federation = \"rig\") serves only %s; its owner %q is outside it", bead.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), cls.Value)
	}
	if len(actors) == 0 {
		actors = []string{""}
	}
	for _, actor := range actors {
		if rigNamespacePinAdmitted(ns, actor) {
			continue
		}
		return newRigNamespacePathError("gc hook --claim: not claiming %s: rig %q (federation = \"rig\") serves only %s and this city's pool sessions; claim actor %q is outside it%s", bead.ID, rigNamespaceLabel(ns), rigNamespaceShown(ns), strings.TrimSpace(actor), rigNamespaceActorHint(ns, actor))
	}
	return nil
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
