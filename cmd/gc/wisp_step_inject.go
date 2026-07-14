package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// wispStepInjectionContent resolves the agent's current in-progress formula
// step bead and returns it formatted as a <system-reminder> block, or "" if
// none is found or any error occurs. Designed for best-effort use in hook
// injection paths — callers must never fail hard on an empty return.
//
// Store priority: if GC_RIG_ROOT is set the rig store is queried (where
// rig-scoped polecat work beads live), otherwise the city store at cityPath.
// When cityPath is empty the function falls back to GC_CITY from the env.
func wispStepInjectionContent(cityPath string) string {
	effective := cityPath
	if effective == "" {
		effective = strings.TrimSpace(os.Getenv("GC_CITY"))
	}
	store := openWispStepStore(effective)
	if store == nil {
		return ""
	}
	assignees := wispStepAssignees()
	if len(assignees) == 0 {
		return ""
	}
	b, err := resolveActiveWispStep(store, assignees)
	if err != nil || b == nil {
		return ""
	}
	return formatWispStepReminder(b)
}

// openWispStepStore opens the bead store to query for active wisp steps.
// If GC_RIG_ROOT is set it opens that rig's store (where rig-scoped polecat
// work lives); otherwise it opens the city store at cityPath.
// Returns nil on any error — callers treat nil as "no store available".
func openWispStepStore(cityPath string) beads.Store {
	if rigRoot := strings.TrimSpace(os.Getenv("GC_RIG_ROOT")); rigRoot != "" {
		store, err := openStoreAtForCity(rigRoot, cityPath)
		if err == nil {
			return store
		}
	}
	if cityPath == "" {
		return nil
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil
	}
	return store
}

// wispStepAssignees returns the deduped set of identity strings to match
// against bead assignees. Uses GC_ALIAS (primary), GC_SESSION_NAME, and
// GC_SESSION_ID in that priority order.
func wispStepAssignees() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(os.Getenv("GC_ALIAS"))
	add(os.Getenv("GC_SESSION_NAME"))
	add(os.Getenv("GC_SESSION_ID"))
	return out
}

// resolveActiveWispStep returns the agent's current formula step bead.
//
// Resolution order:
//  1. Find the agent's in-progress molecule bead (type=molecule or type=wisp).
//  2. Find that molecule's in-progress type=step child — the current step.
//  3. If no in-progress step child exists, fall back to the entry step: the
//     first open type=step child (deterministic formula start position).
//  4. If no molecule bead is assigned to the agent, follow the molecule_id
//     bridge: an attached (v1) formula routes only the source work bead and
//     stamps its molecule_id with the (unrouted, unassigned) root, so resolve
//     the root's active step through that bridge.
//  5. If no molecule_id bridge exists either, fall back to any in-progress bead
//     with a non-empty Description (legacy behavior for agents not running a
//     formula).
//
// Returns nil, nil when no bead can be resolved. Never returns an error for
// not-found conditions — callers treat nil as "nothing to inject".
func resolveActiveWispStep(store beads.Store, assignees []string) (*beads.Bead, error) {
	if store == nil || len(assignees) == 0 {
		return nil, nil
	}

	molecule, err := resolveActiveMolecule(store, assignees)
	if err != nil {
		return nil, err
	}
	if molecule == nil {
		// No molecule root is assigned to the agent. Attached (v1) formulas
		// leave the root unrouted and stamp molecule_id on the routed source
		// bead, so follow that bridge to the root's active step before the
		// legacy description fallback. Best-effort: a resolution error or no
		// bridge drops to legacy.
		if root := resolveMoleculeRootViaBridge(store, assignees); root != nil {
			step, stepErr := resolveInProgressStepChild(store, root.ID)
			if stepErr != nil {
				log.Printf("wisp step inject: error resolving in-progress step for bridged molecule %s: %v", root.ID, stepErr)
				return nil, nil
			}
			if step != nil {
				return step, nil
			}
			return resolveEntryStepChild(store, root.ID)
		}
		// No molecule bridge; fall back to legacy: any in-progress bead with a description.
		return resolveBeadWithDescription(store, assignees)
	}

	// Prefer the in-progress step child (the agent is mid-step).
	step, err := resolveInProgressStepChild(store, molecule.ID)
	if err != nil {
		log.Printf("wisp step inject: error resolving in-progress step children for molecule %s: %v", molecule.ID, err)
		return nil, nil
	}
	if step != nil {
		return step, nil
	}

	// Fall back to the entry step: first open step child.
	log.Printf("wisp step inject: no in-progress step for molecule %s; resolving entry step", molecule.ID)
	return resolveEntryStepChild(store, molecule.ID)
}

// resolveActiveMolecule returns the agent's in-progress molecule bead.
// When multiple molecules are found, the most recently updated one is returned
// and the ambiguity is logged. Returns nil, nil when none is found.
func resolveActiveMolecule(store beads.Store, assignees []string) (*beads.Bead, error) {
	for _, molType := range []string{"molecule", "wisp"} {
		results, err := store.List(beads.ListQuery{
			Status:    "in_progress",
			Type:      molType,
			Assignees: assignees,
			TierMode:  beads.TierBoth,
			Limit:     5,
		})
		if err != nil {
			return nil, fmt.Errorf("listing in-progress %s beads: %w", molType, err)
		}
		if len(results) == 0 {
			continue
		}
		if len(results) > 1 {
			ids := make([]string, len(results))
			for i, r := range results {
				ids[i] = r.ID
			}
			log.Printf("wisp step inject: %d in-progress %s beads found (%s); using most recent", len(results), molType, strings.Join(ids, ", "))
		}
		best := results[0]
		for _, r := range results[1:] {
			if r.UpdatedAt.After(best.UpdatedAt) {
				best = r
			}
		}
		return &best, nil
	}
	return nil, nil
}

// resolveMoleculeRootViaBridge finds the molecule root reachable from an
// attached (v1) source work bead. Attached formulas route only the source bead
// and stamp its molecule_id metadata with the (unrouted, unassigned) molecule
// root, so resolveActiveMolecule — which filters molecule roots by assignee —
// never matches. This bridges from the routed, assignee-owned source bead to
// its root via the molecule_id metadata key.
//
// Returns nil on any error or when no bridge bead is found — callers treat nil
// as "no bridge available" and fall through to the legacy path.
func resolveMoleculeRootViaBridge(store beads.Store, assignees []string) *beads.Bead {
	results, err := store.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     10,
	})
	if err != nil {
		return nil
	}
	for i := range results {
		rootID := strings.TrimSpace(results[i].Metadata[beadmeta.MoleculeIDMetadataKey])
		if rootID == "" {
			continue
		}
		root, err := store.Get(rootID)
		if err != nil {
			log.Printf("wisp step inject: molecule_id %q on bead %s did not resolve: %v", rootID, results[i].ID, err)
			continue
		}
		return &root
	}
	return nil
}

// resolveInProgressStepChild returns the in-progress type=step child of moleculeID.
// When multiple are found, the most recently updated one is returned.
func resolveInProgressStepChild(store beads.Store, moleculeID string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:   "in_progress",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Limit:    5,
	})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	if len(results) > 1 {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.ID
		}
		log.Printf("wisp step inject: %d in-progress steps for molecule %s (%s); using most recent", len(results), moleculeID, strings.Join(ids, ", "))
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.UpdatedAt.After(best.UpdatedAt) {
			best = r
		}
	}
	return &best, nil
}

// resolveEntryStepChild returns the first open type=step child of moleculeID.
// This is the deterministic fallback when no step is in-progress: the formula's
// entry position — where execution should (re)start.
func resolveEntryStepChild(store beads.Store, moleculeID string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:   "open",
		Type:     "step",
		ParentID: moleculeID,
		TierMode: beads.TierBoth,
		Limit:    1,
		Sort:     beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("resolving entry step for molecule %s: %w", moleculeID, err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	b := results[0]
	return &b, nil
}

// resolveBeadWithDescription returns the first in-progress bead assigned to any
// of the given identities that has a non-empty Description. This is the legacy
// resolution path used when no molecule bead is assigned to the agent.
func resolveBeadWithDescription(store beads.Store, assignees []string) (*beads.Bead, error) {
	results, err := store.List(beads.ListQuery{
		Status:    "in_progress",
		Assignees: assignees,
		TierMode:  beads.TierBoth,
		Limit:     10,
	})
	if err != nil {
		return nil, err
	}
	for i := range results {
		if strings.TrimSpace(results[i].Description) != "" {
			b := results[i]
			return &b, nil
		}
	}
	return nil, nil
}

// formatWispStepReminder formats a formula step bead as a <system-reminder>
// block for injection into agent context.
func formatWispStepReminder(b *beads.Bead) string {
	title := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Title))
	desc := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Description))
	return fmt.Sprintf(
		"<system-reminder>\nYour current active work assignment:\n\n## %s (%s)\n\n%s\n</system-reminder>\n",
		title, b.ID, desc,
	)
}
