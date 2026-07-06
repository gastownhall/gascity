package main

import (
	"fmt"
	"os"
	"strings"

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

// resolveActiveWispStep queries store for in-progress beads assigned to any
// of the given identities and returns the first one that has a non-empty
// Description. Returns nil, nil when no matching bead is found.
func resolveActiveWispStep(store beads.Store, assignees []string) (*beads.Bead, error) {
	if store == nil || len(assignees) == 0 {
		return nil, nil
	}
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
