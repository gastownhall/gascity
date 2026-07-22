package dispatch

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// resolveDrainItemScope resolves the target scope for one drain item root.
//
// INHERIT FIRST. A drain runs inside a parent workflow whose root already
// carries a scope, and the parent's #16 equality — parentVars[base_branch] ==
// parent-scope.branch — combined with drain deriving its item vars FROM
// parentVars means the inherited scope EQUALS what runtimeVars substitutes. So
// the scope is inherited from the control root rather than re-resolved: one
// fact, one home, and no chance for the two to drift.
//
// A present-invalid control scope is a HARD STOP. "I could not read the scope
// governing this drain" must never degrade to absent and re-enable the cwd
// writers on the item's stages.
//
// Absent control — a drain with no scoped parent, i.e. the pre-seam population —
// falls back to the drain's OWN substitution inputs. runtimeVars already folds
// the item formula's var defaults (drainItemRuntimeVars), so it IS the map the
// item substitutes; resolving the scope from it makes scope.branch == the
// substituted branch by construction, the same D4 rule the order and cook
// boundaries follow. A carrierless item resolves to unknown, which stamps a
// present-valid field-empty object (§2c), never nothing.
func resolveDrainItemScope(store beads.Store, control beads.Bead, runtimeVars map[string]string, storeRoot string) (targetscope.Scope, error) {
	switch res := targetscope.ResolveInherited(store, control); res.State {
	case targetscope.StateValid:
		return res.Scope, nil
	case targetscope.StateInvalid:
		return targetscope.Scope{}, fmt.Errorf("drain control %s carries an unusable target scope: %w", control.ID, res.Reason)
	}
	resolved, err := targetscope.Resolve(targetscope.Sources{Routing: runtimeVars}, storeRoot)
	if err != nil {
		return targetscope.Scope{}, fmt.Errorf("resolving drain item scope from runtime vars: %w", err)
	}
	return resolved.Scope, nil
}

// prepareDrainItemLaunch declares the unit's tracked member under its
// single-winner exclusion, then stamps the item recipe root — BEFORE the root is
// materialized, so a rejection costs a launch and never an orphaned graph.
//
// The tracked member gets its OWN declaration (§5b). A drain item root does not
// point back at the outer workflow root, and the inherited walk runs forward
// only along parent/root edges — never in reverse along a convoy track — so the
// member the item ultimately closes would otherwise resolve ABSENT and let its
// stale flat keys stay behaviorally authoritative. The member is declared in the
// store that OWNS it (drainMemberDeclarationStore), preferring CAS and falling
// back to a guarded stamp where the store forwards no CAS. An unresolved tracked
// item is skipped: there is no real bead to declare.
func prepareDrainItemLaunch(recipe *formula.Recipe, scope targetscope.Scope, member beads.Bead, store beads.Store, opts ProcessOptions) error {
	if strings.TrimSpace(member.ID) != "" && !convoycore.IsUnresolvedTrackedItem(member) {
		// Declare the member in the store that OWNS it. A drain's convoy members
		// can live in a separate per-class store (ProcessOptions.MemberStores),
		// absent from the control's graph store — declaring against the graph
		// store would fail-close the drain for the one bead the close gate reads.
		owner, err := drainMemberDeclarationStore(store, member.ID, opts)
		if err != nil {
			return fmt.Errorf("resolving declaration store for member %s: %w", member.ID, err)
		}
		// nil owner means no known store holds the member (e.g. it was deleted
		// before unit creation). Skip rather than fail: the member stays at its
		// current state, never worse than before the seam.
		if owner != nil {
			if err := declareOrStampDrainMember(owner, member.ID, scope); err != nil {
				return err
			}
		}
	}
	return targetscope.StampRoot(recipe, scope)
}

// drainMemberDeclarationStore returns the store that owns the member — the graph
// store, then each per-class member store — as the first whose Get succeeds,
// reusing the same probe set drain reservations use. It differs from
// drainMemberOwningStore in ONE way that matters for a declaration: when no
// probed store holds the member it returns nil, not the primary, so a member
// deleted before unit creation is SKIPPED rather than declared against a store
// that will then fail its read. A non-not-found probe error is surfaced.
func drainMemberDeclarationStore(store beads.Store, memberID string, opts ProcessOptions) (beads.Store, error) {
	for _, probe := range drainMemberProbeSet(store, opts) {
		if probe == nil {
			continue
		}
		if _, err := probe.Get(memberID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, err
		}
		return probe, nil
	}
	return nil, nil
}

// declareOrStampDrainMember commits the tracked member's scope, preferring the
// single-winner CAS declaration and falling back to a guarded stamp only when
// the store forwards no metadata-CAS capability.
//
// A drain runs in the controller on whatever store the deployment gives it, and
// a store that decorates reads/writes without re-exposing the CAS interface
// would otherwise FAIL the whole drain — the commonest control-plane path — on a
// capability gap. The ItemRootKey lock this call already runs under serializes
// item-root creation, and the scope is inherited deterministically per member,
// so the stamp fallback has the exclusion the CAS would provide; its immutability
// guard rejects a present-valid-different member exactly as the CAS would.
func declareOrStampDrainMember(store beads.Store, memberID string, scope targetscope.Scope) error {
	if _, err := targetscope.Declare(store, memberID, scope); err != nil {
		if errors.Is(err, targetscope.ErrDeclarationUnsupported) {
			_, stampErr := targetscope.StampMemberScope(store, memberID, scope)
			return stampErr
		}
		return err
	}
	return nil
}
