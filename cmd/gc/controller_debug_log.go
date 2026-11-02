package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Per-tick controller diagnostics. The assigned-work snapshot and the pool
// counts used to print on every tick: one line per assigned work bead plus one
// per non-zero pool template, about 10k lines/hour in a production supervisor
// journal, burying the lines that carry signal. They are operator diagnostics,
// so they are opt-in through GC_DEBUG (route_log.go) like the other ones. The
// PARTIAL degradation warnings stay unconditional at their call sites: they
// report a fault, not routine state.

// logAssignedWorkBeads prints the tick's assigned-work snapshot when GC_DEBUG
// is on.
func logAssignedWorkBeads(stderr io.Writer, rows []beads.Bead, rigStores int) {
	if !gcDebugEnabled() {
		return
	}
	if len(rows) == 0 {
		fmt.Fprintf(stderr, "assignedWorkBeads: 0 beads (rigStores=%d)\n", rigStores) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "assignedWorkBeads: %d beads found\n", len(rows)) //nolint:errcheck
	for _, wb := range rows {
		fmt.Fprintf(stderr, "  %s assignee=%s routed=%s status=%s\n", wb.ID, wb.Assignee, wb.Metadata[beadmeta.RoutedToMetadataKey], wb.Status) //nolint:errcheck
	}
}

// logPoolCounts prints the non-zero per-template counts under label when
// GC_DEBUG is on, sorted by template so consecutive ticks diff cleanly.
func logPoolCounts(stderr io.Writer, label string, counts map[string]int) {
	if !gcDebugEnabled() {
		return
	}
	templates := make([]string, 0, len(counts))
	for tmpl, count := range counts {
		if count > 0 {
			templates = append(templates, tmpl)
		}
	}
	sort.Strings(templates)
	for _, tmpl := range templates {
		fmt.Fprintf(stderr, "%s: %s = %d\n", label, tmpl, counts[tmpl]) //nolint:errcheck
	}
}
