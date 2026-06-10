package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/state"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func newBeadsStateCmd(stdout, stderr io.Writer) *cobra.Command {
	var rigFlag, stateFilter string
	var showIDs, jsonOut bool
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Classify beads by effective state",
		Long: `Classify every bead into one of 16 effective states and display
the results grouped by state with owner and count.

Anomaly states (orphaned, ready-unrouted, routed-stalled-dispatch, unknown)
are prefixed with '!' in table output.`,
		Example: `  gc beads state
  gc beads state --json
  gc beads state --state routed-waiting
  gc beads state --ids`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsState(rigFlag, stateFilter, showIDs, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rigFlag, "rig", "", "limit to beads routed to this rig")
	cmd.Flags().StringVar(&stateFilter, "state", "", "show only beads in this effective state")
	cmd.Flags().BoolVar(&showIDs, "ids", false, "include bead IDs in table output")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

type beadsStateJSONResult struct {
	SchemaVersion string                         `json:"schema_version"`
	States        map[string]beadsStateJSONEntry `json:"states"`
}

type beadsStateJSONEntry struct {
	Owner string   `json:"owner"`
	Count int      `json:"count"`
	IDs   []string `json:"ids"`
}

type beadsStateRow struct {
	id    string
	title string
}

// anomalyStates marks states that represent actionable problems.
var anomalyStates = map[state.EffectiveState]bool{
	state.StateOrphaned:              true,
	state.StateReadyUnrouted:         true,
	state.StateRoutedStalledDispatch: true,
	state.StateUnknown:               true,
}

// cmdBeadsState implements "gc beads state". It classifies every bead into one
// of 16 effective states using internal/beads/state.Classify and renders the
// result as a grouped table or JSON object.
func cmdBeadsState(rigFlag, stateFilter string, showIDs, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	allBeads, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: listing beads: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	closedIDs := buildClosedSet(allBeads)
	blocked := buildBlockedSet(store, allBeads, closedIDs)

	now := time.Now()
	ready := make(map[string]bool, len(allBeads))
	for _, b := range allBeads {
		if !blocked[b.ID] && beads.IsReadyCandidate(b, now) {
			ready[b.ID] = true
		}
	}

	live, liveRigs := buildBeadsStateLiveSets(store)

	groups := make(map[state.EffectiveState][]beadsStateRow)
	for _, b := range allBeads {
		if rigFlag != "" {
			routed := b.Metadata["gc.routed_to"]
			rig, _, _ := strings.Cut(routed, "/")
			if rig != rigFlag {
				continue
			}
		}
		bv := beadStateView{b}
		s := state.Classify(bv, ready, blocked, live, liveRigs)
		if stateFilter != "" && string(s) != stateFilter {
			continue
		}
		groups[s] = append(groups[s], beadsStateRow{id: b.ID, title: b.Title})
	}

	if jsonOut {
		return writeBeadsStateJSON(groups, stdout, stderr)
	}
	return writeBeadsStateTable(groups, showIDs, stdout)
}

func writeBeadsStateJSON(groups map[state.EffectiveState][]beadsStateRow, stdout, stderr io.Writer) int {
	result := beadsStateJSONResult{
		SchemaVersion: "1",
		States:        make(map[string]beadsStateJSONEntry, len(groups)),
	}
	for s, rows := range groups {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.id)
		}
		sort.Strings(ids)
		result.States[string(s)] = beadsStateJSONEntry{
			Owner: state.Owner(s),
			Count: len(rows),
			IDs:   ids,
		}
	}
	if err := writeCLIJSONLine(stdout, result); err != nil {
		fmt.Fprintf(stderr, "gc beads state: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func writeBeadsStateTable(groups map[state.EffectiveState][]beadsStateRow, showIDs bool, stdout io.Writer) int {
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATE\tOWNER\tCOUNT") //nolint:errcheck // best-effort stdout
	for _, s := range state.DisplayOrder {
		rows, ok := groups[s]
		if !ok {
			continue
		}
		prefix := "  "
		if anomalyStates[s] {
			prefix = "! "
		}
		fmt.Fprintf(w, "%s%s\t%s\t%d\n", prefix, s, state.Owner(s), len(rows)) //nolint:errcheck // best-effort stdout
		if showIDs {
			for _, r := range rows {
				fmt.Fprintf(w, "    %s\t\t%s\n", r.id, r.title) //nolint:errcheck // best-effort stdout
			}
		}
	}
	_ = w.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

// beadStateView wraps beads.Bead to implement state.BeadView without storing
// a pointer, keeping the view's lifetime independent of the bead slice.
type beadStateView struct {
	b beads.Bead
}

func (v beadStateView) ID() string        { return v.b.ID }
func (v beadStateView) Status() string    { return v.b.Status }
func (v beadStateView) IssueType() string { return v.b.Type }
func (v beadStateView) Title() string     { return v.b.Title }
func (v beadStateView) Labels() []string  { return v.b.Labels }
func (v beadStateView) Meta(key string) string {
	if v.b.Metadata == nil {
		return ""
	}
	return v.b.Metadata[key]
}

// buildClosedSet returns the set of closed bead IDs, used by buildBlockedSet
// to determine whether a blocking dependency has been resolved.
func buildClosedSet(allBeads []beads.Bead) map[string]bool {
	closed := make(map[string]bool, len(allBeads))
	for _, b := range allBeads {
		if b.Status == "closed" {
			closed[b.ID] = true
		}
	}
	return closed
}

// buildBlockedSet returns the set of bead IDs that are blocked by at least one
// open blocking dependency. It uses the IsBlocked projection when the store
// provides it (bd/dolt), and falls back to DepList for stores that do not
// (file store, in-memory store).
func buildBlockedSet(store beads.Store, allBeads []beads.Bead, closedIDs map[string]bool) map[string]bool {
	blocked := make(map[string]bool)
	for _, b := range allBeads {
		if b.Status == "closed" || b.Status == "deferred" || b.Status == "pinned" {
			continue
		}
		// Use the denormalized projection when available.
		if b.IsBlocked != nil {
			if *b.IsBlocked {
				blocked[b.ID] = true
			}
			continue
		}
		// Fall back to DepList for stores without the projection.
		deps, err := store.DepList(b.ID, "down")
		if err != nil {
			continue
		}
		for _, dep := range deps {
			if beads.IsReadyBlockingDependencyType(dep.Type) && !closedIDs[dep.DependsOnID] {
				blocked[b.ID] = true
				break
			}
		}
	}
	return blocked
}

// buildBeadsStateLiveSets returns:
//   - live: non-closed, non-frozen session names (for orphan detection)
//   - liveRigs: rig names with at least one live session (for stalled-dispatch detection)
func buildBeadsStateLiveSets(store beads.Store) (live, liveRigs map[string]bool) {
	live = make(map[string]bool)
	liveRigs = make(map[string]bool)
	sessionBeads, err := session.ListAllSessionBeads(store, beads.ListQuery{IncludeClosed: false})
	if err != nil {
		return
	}
	for _, sb := range sessionBeads {
		if sb.Status == "closed" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(sb.Metadata["state"])) {
		case "suspended", "archived", "quarantined", "drained":
			continue
		}
		if sessName := sb.Metadata["session_name"]; sessName != "" {
			live[sessName] = true
		}
		if tmpl := strings.TrimSpace(sb.Metadata["template"]); tmpl != "" {
			if rig, _, ok := strings.Cut(tmpl, "/"); ok && rig != "" {
				liveRigs[rig] = true
			}
		}
	}
	return
}
