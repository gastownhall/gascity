package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/state"
	"github.com/gastownhall/gascity/internal/config"
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
are prefixed with '!' in table output.

Four states — delivering, waiting-review, waiting-decision, and the delivery
route into done — key on gc.phase metadata that nothing writes yet, so they
cannot appear in a report today. A session whose bead still reads active but
whose process is gone (a zombie) also still counts as live, so
routed-stalled-dispatch fires only for rigs with no live-stated sessions at
all. See engdocs/contributors/bead-effective-state.md.`,
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
	OK            bool                           `json:"ok"`
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

// cmdBeadsState implements "gc beads state". It classifies every bead into one
// of 16 effective states using internal/beads/state.Classify and renders the
// result as a grouped table or JSON object.
func cmdBeadsState(rigFlag, stateFilter string, showIDs, jsonOut bool, stdout, stderr io.Writer) int {
	// Validate the filter before touching the store. An unrecognized --state
	// would otherwise match nothing and print an empty report with exit 0, so a
	// typo ("routed_waiting") and a genuinely clean city would look identical —
	// the one confusion a triage tool must never create.
	if stateFilter != "" && !isKnownBeadsState(stateFilter) {
		fmt.Fprintf(stderr, "gc beads state: unknown --state %q; valid states: %s\n", //nolint:errcheck // best-effort stderr
			stateFilter, strings.Join(knownBeadsStateNames(), ", "))
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// The warning writer is threaded through so config-load warnings reach the
	// operator instead of being dropped
	// (TestNonTestLoadCityConfigCallersPassWarningWriter).
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Validate --rig against the configured rigs rather than against "did any
	// bead match". A rig with genuinely no routed beads is a real answer and
	// must still exit 0; only a name the city does not know is an error.
	if rigFlag != "" {
		known := beadsStateKnownRigs(cfg)
		if !known[rigFlag] {
			fmt.Fprintf(stderr, "gc beads state: unknown --rig %q; configured rigs: %s\n", //nolint:errcheck // best-effort stderr
				rigFlag, strings.Join(beadsStateSortedRigNames(known), ", "))
			return 1
		}
	}

	// Every store the city reads, the same federation "gc beads list" uses:
	// routed work usually lives in a rig store, so reading the city store alone
	// would leave most routed-* verdicts unreachable.
	views, code := openAllConvoyStoresAt(cityPath, stderr, "gc beads state")
	if views == nil {
		return code
	}
	views = convoyStoreViewsForRead(cityPath, views, stderr, "gc beads state")
	allBeads, blocked, err := listBeadsStateAcrossStores(views)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Sessions are read through the session-class front door, so a
	// [beads.classes.sessions] relocation is honored.
	cityStore, err := openCityStoreAt(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	now := time.Now()
	ready := make(map[string]bool, len(allBeads))
	for _, b := range allBeads {
		if !blocked[b.ID] && beads.IsReadyCandidate(b, now) {
			ready[b.ID] = true
		}
	}

	live, liveRigs, err := buildBeadsStateLiveSets(cliSessionFrontDoor(cityStore, cfg, cityPath))
	if err != nil {
		// No report at all is the right answer here. With an incomplete session
		// view every bead bound to a session we cannot see classifies as
		// orphaned and every routed bead as routed-stalled-dispatch, so
		// rendering would not merely hide anomalies, it would manufacture them.
		fmt.Fprintf(stderr, //nolint:errcheck // best-effort stderr
			"gc beads state: listing live sessions: %v\nno report produced: without a complete session view the orphaned and routed-stalled-dispatch verdicts would be fabricated\n",
			err)
		return 1
	}

	groups := make(map[state.EffectiveState][]beadsStateRow)
	for _, b := range allBeads {
		if rigFlag != "" {
			// gc.routed_to is a qualified "<rig>/<agent>" identity, so it splits
			// on the LAST slash via the canonical parser — a rig path that
			// itself contains a slash is parsed correctly here and was not by
			// the first-slash split this replaced.
			rig, _ := config.ParseQualifiedName(b.Metadata[beadmeta.RoutedToMetadataKey])
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

// isKnownBeadsState reports whether name is one of the classifier's effective
// states. state.DisplayOrder is the single enumeration of the vocabulary, so
// this cannot drift from the classifier.
func isKnownBeadsState(name string) bool {
	for _, s := range state.DisplayOrder {
		if string(s) == name {
			return true
		}
	}
	return false
}

// beadsStateKnownRigs returns the set of rig names configured for the city, so
// --rig can distinguish a misspelled rig from a rig that is merely idle.
func beadsStateKnownRigs(cfg *config.City) map[string]bool {
	known := make(map[string]bool, len(cfg.Rigs))
	for i := range cfg.Rigs {
		if name := strings.TrimSpace(cfg.Rigs[i].Name); name != "" {
			known[name] = true
		}
	}
	return known
}

// beadsStateSortedRigNames returns the rig names in sorted order for stable diagnostics.
func beadsStateSortedRigNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// knownBeadsStateNames returns the valid --state values for diagnostics.
func knownBeadsStateNames() []string {
	names := make([]string, 0, len(state.DisplayOrder))
	for _, s := range state.DisplayOrder {
		names = append(names, string(s))
	}
	sort.Strings(names)
	return names
}

func writeBeadsStateJSON(groups map[state.EffectiveState][]beadsStateRow, stdout, stderr io.Writer) int {
	result := beadsStateJSONResult{
		SchemaVersion: "1",
		OK:            true,
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
		if state.IsAnomaly(s) {
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

// beadsStateStoredRow is a listed bead together with the index of the store
// view it came from, so the DepList fallback asks the bead's own store.
type beadsStateStoredRow struct {
	view int
	bead beads.Bead
}

// listBeadsStateAcrossStores lists every bead, closed included, from every
// view and merges them the way "gc beads list" does: rows a class binding
// supersedes are dropped, and same-id rows from different stores are kept. It
// also returns the blocked set, computed per store because dependency edges are
// read from the store that holds the bead.
func listBeadsStateAcrossStores(views []convoyStoreView) ([]beads.Bead, map[string]bool, error) {
	rows := make([][]beadsStateStoredRow, len(views))
	for i, view := range views {
		list, err := view.store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
		if err != nil {
			return nil, nil, fmt.Errorf("listing beads in %s: %w", view.path, err)
		}
		rows[i] = make([]beadsStateStoredRow, 0, len(list))
		for _, b := range list {
			rows[i] = append(rows[i], beadsStateStoredRow{view: i, bead: b})
		}
	}
	merged, err := mergeConvoyViewRows(views, rows, func(r beadsStateStoredRow) string { return r.bead.ID })
	if err != nil {
		return nil, nil, err
	}

	all := make([]beads.Bead, 0, len(merged))
	perView := make([][]beads.Bead, len(views))
	for _, r := range merged {
		all = append(all, r.bead)
		perView[r.view] = append(perView[r.view], r.bead)
	}
	closedIDs := buildClosedSet(all)
	blocked := make(map[string]bool)
	for i, list := range perView {
		markBlockedBeads(blocked, list, closedIDs, views[i].store.DepList)
	}
	return all, blocked, nil
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

// markBlockedBeads adds to blocked every bead in list that has at least one
// open blocking dependency. It uses the IsBlocked projection when the store
// provides it (bd/dolt), and falls back to depList, the holding store's
// DepList, for stores that do not (file store, in-memory store).
func markBlockedBeads(blocked map[string]bool, list []beads.Bead, closedIDs map[string]bool, depList func(id, direction string) ([]beads.Dep, error)) {
	for _, b := range list {
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
		deps, err := depList(b.ID, "down")
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
}

// beadsStateSessionIsLive reports whether a session bead still occupies a slot,
// which is what "live" means for the classifier's orphaned and
// routed-stalled-dispatch verdicts.
//
// The judgement is delegated to the canonical lifecycle projection
// (session.ProjectLifecycle) rather than re-stated as a local denylist. An
// ad-hoc list of dead states is guaranteed to rot: the first version of this
// command listed only suspended/archived/quarantined/drained and therefore
// counted failed-create, orphaned, closing and stopped sessions as live, so a
// bead pointing at a failed-create session reported in-progress instead of
// orphaned. CountsAgainstCap is the same predicate the capacity accounting
// uses, so the two can no longer disagree.
//
// It is not a clean swap, though: a quarantined session still holds its slot
// (so the controller does not replace it while it is held out of wake) but is
// not working anything. Counting it live would hide exactly the bead the
// orphaned verdict exists for, and would mute routed-stalled-dispatch for a rig
// whose only sessions are quarantined, so it is excluded explicitly.
//
// A bead with no lifecycle state stamped at all is treated as live on purpose:
// the projection cannot judge it, and for an anomaly detector a false negative
// (a missed orphan) is much cheaper than a false positive (a fabricated one
// that sends an operator reclaiming a healthy session).
func beadsStateSessionIsLive(info session.Info) bool {
	if info.Closed {
		return false
	}
	if strings.TrimSpace(info.MetadataState) == "" {
		return true
	}
	projected := session.ProjectLifecycle(session.LifecycleInputFromInfo(info))
	return projected.CountsAgainstCap && projected.BaseState != session.BaseStateQuarantined
}

// buildBeadsStateLiveSets returns:
//   - live: live session names (for orphan detection)
//   - liveRigs: rig names with at least one live session (for stalled-dispatch detection)
//
// It returns an error instead of empty sets when the session listing fails.
// state.Classify reads nil/empty live sets as "detection disabled", so
// swallowing the error here would turn every store hiccup into a clean bill of
// health: every session-bound bead would read in-progress and every routed bead
// routed-waiting, with exit 0 and nothing on stderr. An anomaly detector must
// fail loudly when it cannot see, not report calm.
//
// A partial listing is an error for the same reason, and a sharper one: a
// session missing from the union does not merely hide an anomaly, it invents
// one, because beads pointing at the missing session classify as orphaned.
// Callers surface the error rather than rendering fabricated anomalies.
func buildBeadsStateLiveSets(sessions *session.Store) (live, liveRigs map[string]bool, err error) {
	live = make(map[string]bool)
	liveRigs = make(map[string]bool)
	infos, listErr := sessions.ListAll(session.ListAllOptions{})
	if listErr != nil {
		return nil, nil, listErr
	}
	for _, info := range infos {
		if !beadsStateSessionIsLive(info) {
			continue
		}
		if info.SessionName != "" {
			live[info.SessionName] = true
		}
		if tmpl := strings.TrimSpace(info.Template); tmpl != "" {
			if rig, _ := config.ParseQualifiedName(tmpl); rig != "" {
				liveRigs[rig] = true
			}
		}
	}
	return live, liveRigs, nil
}
