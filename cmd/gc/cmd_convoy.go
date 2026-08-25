package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

type convoyActionResult struct {
	SchemaVersion string   `json:"schema_version"`
	OK            bool     `json:"ok"`
	Command       string   `json:"command"`
	Action        string   `json:"action"`
	ConvoyID      string   `json:"convoy_id,omitempty"`
	Title         string   `json:"title,omitempty"`
	IssueIDs      []string `json:"issue_ids,omitempty"`
	Target        string   `json:"target,omitempty"`
	Closed        *int     `json:"closed,omitempty"`
	Stranded      *int     `json:"stranded,omitempty"`
	TotalChildren *int     `json:"total_children,omitempty"`
	OpenChildren  *int     `json:"open_children,omitempty"`
	DryRun        bool     `json:"dry_run,omitempty"`
	AlreadyClosed bool     `json:"already_closed,omitempty"`
	Forced        bool     `json:"forced,omitempty"`
	Notify        string   `json:"notify,omitempty"`
}

func intRef(value int) *int {
	return &value
}

func newConvoyCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "convoy",
		Short: "Manage convoys — graphs of related work",
		Long: `Manage convoys — graphs of related work beads.

A convoy is a named graph of beads with dependencies. Convoys
group related issues via tracks dependencies.

Convoys are distinct from workflows — the DAGs compiled from
v2 formulas and managed by the dispatch
subsystem. The convoy lifecycle subcommands (create, list, status,
target, add, close, check, stranded, land) do not operate on
workflow roots; the dispatch subcommands (control, delete,
delete-source, reopen-source) manage workflow trees and their
control beads.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc convoy: missing subcommand (create, list, status, target, add, close, check, stranded, land)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc convoy: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newConvoyCreateCmd(stdout, stderr),
		newConvoyListCmd(stdout, stderr),
		newConvoyStatusCmd(stdout, stderr),
		newConvoyTargetCmd(stdout, stderr),
		newConvoyAddCmd(stdout, stderr),
		newConvoyCloseCmd(stdout, stderr),
		newConvoyCheckCmd(stdout, stderr),
		newConvoyStrandedCmd(stdout, stderr),
		newConvoyAutocloseCmd(stdout, stderr),
		newConvoyLandCmd(stdout, stderr),
	)
	cmd.AddCommand(convoyDispatchSubcommands(stdout, stderr)...)
	return cmd
}

type convoyCreateOptions struct {
	Fields ConvoyFields
	Owned  bool
}

func newConvoyCreateCmd(stdout, stderr io.Writer) *cobra.Command {
	var owner, notify, merge, target string
	var owned, jsonOut bool
	cmd := &cobra.Command{
		Use:   "create <name> [issue-ids...]",
		Short: "Create a convoy and optionally track issues",
		Long: `Create a convoy and optionally link existing issues to it.

Creates a convoy bead and tracks any provided issue IDs. Issues can
also be added later with "gc convoy add".`,
		Example: `  gc convoy create sprint-42
  gc convoy create sprint-42 issue-1 issue-2 issue-3
  gc convoy create deploy --owner mayor --notify mayor --merge mr
  gc convoy create auth-rewrite --owned --target integration/auth-rewrite`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			opts := convoyCreateOptions{
				Fields: ConvoyFields{
					Owner:  owner,
					Notify: notify,
					Merge:  merge,
					Target: target,
				},
				Owned: owned,
			}
			code := 0
			if jsonOut {
				code = cmdConvoyCreateWithOptionsJSON(args, opts, true, stdout, stderr)
			} else {
				code = cmdConvoyCreateWithOptions(args, opts, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&owner, "owner", "", "convoy owner (who manages it)")
	cmd.Flags().StringVar(&notify, "notify", "", "notification target on completion")
	cmd.Flags().StringVar(&merge, "merge", "", "merge strategy: direct, mr, local")
	cmd.Flags().StringVar(&target, "target", "", "target branch inherited by child work beads")
	cmd.Flags().BoolVar(&owned, "owned", false, "mark convoy as owned (manual lifecycle, no auto-close)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

func cmdConvoyCreateWithOptions(args []string, opts convoyCreateOptions, stdout, stderr io.Writer) int {
	return cmdConvoyCreateWithOptionsJSON(args, opts, false, stdout, stderr)
}

func cmdConvoyCreateWithOptionsJSON(args []string, opts convoyCreateOptions, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	emitLoadCityConfigWarnings(configWarnWriter(jsonOut, stderr), prov)

	issueIDs := []string(nil)
	if len(args) > 1 {
		issueIDs = args[1:]
		if err := validateConvoyCreateStoreScope(cfg, cityPath, issueIDs); err != nil {
			fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Determine which store to use: if children are provided, use the
	// first child's rig store so convoy and children share a database.
	// This avoids cross-store parent references that bd can't resolve.
	storeDir := cityPath
	if len(issueIDs) > 0 {
		storeDir = convoyCreateStoreRoot(cfg, cityPath, issueIDs[0])
	}
	store, err := openStoreAtForCity(storeDir, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec := openCityRecorderAt(cityPath, stderr)
	return doConvoyCreateWithOptionsJSON(store, cfg, cityPath, rec, args, opts, jsonOut, stdout, stderr)
}

// doConvoyCreate creates a convoy bead and optionally adds issues to it.
// When cfg/cityPath are nil/empty, all beads are assumed to be in the same store.
func doConvoyCreate(store beads.Store, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doConvoyCreateWithOptions(store, rec, args, convoyCreateOptions{}, stdout, stderr)
}

func convoyCreateStoreRoot(cfg *config.City, cityPath, beadID string) string {
	if cfg != nil {
		if rd := rigDirForBead(cfg, beadID); rd != "" {
			return resolveStoreScopeRoot(cityPath, rd)
		}
	}
	return cityPath
}

func validateConvoyCreateStoreScope(cfg *config.City, cityPath string, issueIDs []string) error {
	if cfg == nil || cityPath == "" || len(issueIDs) < 2 {
		return nil
	}
	want := convoyCreateStoreRoot(cfg, cityPath, issueIDs[0])
	for _, id := range issueIDs[1:] {
		got := convoyCreateStoreRoot(cfg, cityPath, id)
		if !samePath(got, want) {
			return fmt.Errorf("issues span multiple stores; create separate convoys per scope")
		}
	}
	return nil
}

func doConvoyCreateWithOptions(store beads.Store, rec events.Recorder, args []string, opts convoyCreateOptions, stdout, stderr io.Writer) int {
	return doConvoyCreateWithOptionsJSON(store, nil, "", rec, args, opts, false, stdout, stderr)
}

func doConvoyCreateWithOptionsJSON(store beads.Store, cfg *config.City, cityPath string, rec events.Recorder, args []string, opts convoyCreateOptions, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc convoy create: missing convoy name") //nolint:errcheck // best-effort stderr
		return 1
	}
	name := args[0]
	issueIDs := args[1:]
	if err := validateConvoyCreateStoreScope(cfg, cityPath, issueIDs); err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	b := beads.Bead{Title: name, Type: "convoy"}
	if opts.Owned {
		b.Labels = []string{"owned"}
	}
	applyConvoyFields(&b, opts.Fields)

	convoy, err := store.Create(b)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Ensure metadata is persisted on all backends. MemStore carries Metadata
	// through Create, but BdStore/exec.Store may not. setConvoyFields uses
	// SetMetadata which works across all backends.
	if err := setConvoyFields(store, convoy.ID, opts.Fields); err != nil {
		fmt.Fprintf(stderr, "gc convoy create: warning: setting fields: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal: convoy already created and event will be emitted.
	}

	for _, id := range issueIDs {
		// Resolve the correct store for this child bead. Children may
		// live in a rig store (different from the city root store where
		// the convoy was created).
		childStore := store
		if cfg != nil {
			if rd := rigDirForBead(cfg, id); rd != "" {
				if rs, err := openStoreAtForCity(rd, cityPath); err == nil {
					childStore = rs
				}
			}
		}
		if _, err := childStore.Get(id); err != nil {
			fmt.Fprintf(stderr, "gc convoy create: issue %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := convoycore.TrackItem(childStore, convoy.ID, id); err != nil {
			fmt.Fprintf(stderr, "gc convoy create: tracking %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	rec.Record(events.Event{
		Type:    events.ConvoyCreated,
		Actor:   eventActor(),
		Subject: convoy.ID,
		Message: name,
	})

	switch {
	case jsonOut:
		return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy create", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.create", Action: "create", ConvoyID: convoy.ID, Title: name, IssueIDs: issueIDs})
	case len(issueIDs) > 0:
		fmt.Fprintf(stdout, "Created convoy %s %q tracking %d issue(s)\n", convoy.ID, name, len(issueIDs)) //nolint:errcheck // best-effort stdout
	default:
		fmt.Fprintf(stdout, "Created convoy %s %q\n", convoy.ID, name) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func newConvoyListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List open convoys with progress",
		Long: `List all open convoys with completion progress.

Shows each convoy's ID, title, and the number of closed vs total
child issues.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdConvoyList(jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// cmdConvoyList is the CLI entry point for listing convoys.
func cmdConvoyList(jsonOut bool, stdout, stderr io.Writer) int {
	return routeReadCmd("convoy list", stderr, convoyListAPIClient, func(cityPath string, c *api.Client, nilReason string) int {
		return routeConvoyList(cityPath, c, nilReason, jsonOut, stdout, stderr)
	})
}

// convoyListAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server or force a
// specific fallback reason without spinning up a real controller.
var convoyListAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeConvoyList dispatches `convoy list` to the supervisor API when a
// controller is up; otherwise falls back to the local multi-store iterator.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
//
// The API path queries /convoys for the convoy list and /convoy/{id}/check
// for each convoy's progress counts. If the per-convoy check returns a
// fallbackable error, the whole operation falls back to local reads so
// output is consistent (partial failure would produce surprising gaps).
func routeConvoyList(cityPath string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	var cr api.CachedRead[[]beads.Bead]
	var progress []api.ConvoyCheckView
	return routeRead(c, "convoy list", nilReason, stderr,
		func() error {
			var err error
			if cr, err = c.ListConvoys(); err != nil {
				return err
			}
			progress, err = fetchConvoyProgress(c, cr.Body)
			return err
		},
		func() int { return renderConvoyListFromAPI(cr, progress, jsonOut, stdout, stderr) },
		func() int { return doConvoyListFallback(cityPath, jsonOut, stdout, stderr) },
	)
}

// fetchConvoyProgress calls /convoy/{id}/check for each convoy in list and
// returns a parallel slice of progress views. Returns the first fallbackable
// error encountered so the caller can surface it for the whole operation.
func fetchConvoyProgress(c *api.Client, convoys []beads.Bead) ([]api.ConvoyCheckView, error) {
	out := make([]api.ConvoyCheckView, len(convoys))
	for i, convoy := range convoys {
		cr, err := c.CheckConvoy(convoy.ID)
		if err != nil {
			return nil, err
		}
		out[i] = cr.Body
	}
	return out, nil
}

// renderConvoyListFromAPI formats the API-sourced convoy list to match
// doConvoyListAcrossStores output. Stale banner appends when cache age > 30s.
func renderConvoyListFromAPI(cr api.CachedRead[[]beads.Bead], progress []api.ConvoyCheckView, jsonOut bool, stdout, stderr io.Writer) int {
	if jsonOut {
		items := make([]convoySummaryJSON, 0, len(cr.Body))
		for i, convoy := range cr.Body {
			items = append(items, convoySummaryFromAPI(convoy, progress[i]))
		}
		if err := writeCLIJSONLine(stdout, convoyListResultJSON{
			SchemaVersion: "1",
			Convoys:       items,
			Summary:       convoyListSummaryJSON{Total: len(items)},
		}); err != nil {
			fmt.Fprintf(stderr, "gc convoy list: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	if len(cr.Body) == 0 {
		fmt.Fprintln(stdout, "No open convoys") //nolint:errcheck // best-effort stdout
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tPROGRESS") //nolint:errcheck // best-effort stdout
	for i, convoy := range cr.Body {
		p := progress[i]
		fmt.Fprintf(tw, "%s\t%s\t%d/%d closed\n", convoy.ID, convoy.Title, p.Closed, p.Total) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

func convoySummaryFromAPI(convoy beads.Bead, progress api.ConvoyCheckView) convoySummaryJSON {
	return convoySummaryJSON{
		ID:       convoy.ID,
		Title:    convoy.Title,
		Status:   convoy.Status,
		Progress: convoyProgressFromAPI(progress),
		Owned:    hasLabel(convoy.Labels, "owned"),
		Fields:   convoyFieldsFromBead(convoy),
	}
}

func convoyProgressFromAPI(progress api.ConvoyCheckView) convoyProgressJSON {
	return convoyProgressJSON{
		Closed: progress.Closed,
		Total:  progress.Total,
	}
}

// doConvoyListFallback is the direct-bd path for "gc convoy list".
func doConvoyListFallback(cityPath string, jsonOut bool, stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStoresAt(cityPath, stderr, "gc convoy list")
	if stores == nil {
		return code
	}
	stores = convoyStoreViewsForRead(cityPath, stores, stderr, "gc convoy list")
	return doConvoyListAcrossStores(stores, jsonOut, stdout, stderr)
}

func convoyStoreCandidates(cfg *config.City, cityPath, beadID string) []string {
	return convoyStoreCandidatesWithProvider(cfg, cityPath, beadID, func(scopeRoot string) string {
		return rawBeadsProviderForScope(scopeRoot, cityPath)
	})
}

func convoyStoreCandidatesWithProvider(cfg *config.City, cityPath, beadID string, providerForScope func(string) string) []string {
	if providerForScope(cityPath) == "file" && !fileStoreUsesScopedRoots(cityPath) {
		legacyCityOnly := true
		if cfg != nil {
			for _, rig := range cfg.Rigs {
				if strings.TrimSpace(rig.Path) == "" {
					continue
				}
				scopeRoot := resolveStoreScopeRoot(cityPath, rig.Path)
				if providerForScope(scopeRoot) != "file" || (!samePath(scopeRoot, cityPath) && scopeUsesFileStoreContract(scopeRoot)) {
					legacyCityOnly = false
					break
				}
			}
		}
		if legacyCityOnly {
			return []string{cityPath}
		}
	}
	capacity := 2
	if cfg != nil {
		capacity += len(cfg.Rigs)
	}
	candidates := make([]string, 0, capacity)
	add := func(dir string) {
		if dir == "" {
			return
		}
		for _, existing := range candidates {
			if existing == dir {
				return
			}
		}
		candidates = append(candidates, dir)
	}
	if cfg != nil {
		if rd := rigDirForBead(cfg, beadID); rd != "" {
			add(resolveStoreScopeRoot(cityPath, rd))
		}
	}
	add(cityPath)
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			add(resolveStoreScopeRoot(cityPath, rig.Path))
		}
	}
	return candidates
}

type convoyStoreView struct {
	path  string
	store beads.Store
	role  convoyViewRole
}

// convoyViewRole says how a view's rows relate to the other views' rows. It is
// the only thing a fan-out merge needs to know about where a store came from.
type convoyViewRole int

const (
	// convoyViewOrdinary is a store the directory scan enumerated. Its ids are
	// its own: a city and a rig that each mint "gc-1" hold two different beads,
	// and a merge that collapsed them would delete one from the output.
	convoyViewOrdinary convoyViewRole = iota
	// convoyViewClassBinding is the city's relocated class binding — the store
	// `gc storage migrate` moved the infrastructure classes INTO, and the one
	// the city has gone on mutating since.
	convoyViewClassBinding
	// convoyViewMigrationSource is the city store the migration copied OUT of.
	// The copies it retained are frozen at cutover and share their ids with the
	// binding's live rows, so they are the one duplicate a merge can safely
	// collapse.
	convoyViewMigrationSource
)

// convoyBindingViewPath is what the class binding is called in fan-out output.
//
// It is deliberately not a directory. No `bd` invocation can be rooted there
// and no lock can be taken on it, so a caller that treats a view's path as a
// working directory has to check the view's role first.
const convoyBindingViewPath = "city (class binding)"

// isClassBinding reports whether this view is the city's relocated class
// binding rather than one of the directories the scan enumerated.
func (v convoyStoreView) isClassBinding() bool {
	return v.role == convoyViewClassBinding
}

// scopePath is the directory whose SCOPE owns the view's rows.
//
// Every view but the class binding is itself a scope root, so this is usually
// the view's own path. The binding is not a directory at all — it is where the
// city's infrastructure classes were relocated to — and its rows belong to the
// city scope, which is the answer three separate questions need: which store ref
// a workflow root implies when it carries none, which scope a source-workflow
// lock is keyed on, and whether two matched views are one scope or two.
func (v convoyStoreView) scopePath(cityPath string) string {
	if v.isClassBinding() {
		return cityPath
	}
	return v.path
}

// convoyStoreViewsWithBinding federates a directory scan's views with the
// city's relocated class binding, and reports a refusal rather than deciding
// what to do about it.
//
// The scan enumerates DIRECTORIES, and a relocated binding is not one of them.
// On a converged city that makes every fan-out read the copies `gc storage
// migrate` retained — frozen at cutover, never mutated since — while the rows
// the city actually works are invisible. Appending the binding as one more view
// is what closes that, and marking the city's view as the migration source is
// what lets the merge collapse the resulting duplicate.
//
// A REFUSED binding comes back as an error with the views unchanged, because
// the two kinds of caller need opposite things from it. A read can print what
// it has and say what is missing; a destructive sweep cannot, because reaching
// only some of the rows it was asked to delete is worse than deleting nothing.
// Deciding that here would force one of them to be wrong.
//
// Both shapes of refusal arrive as an error and neither arrives as "this city
// relocates nothing". A city configured for a binding it has not converged on
// resolves to a refusedClassStore carrying the boot gate's sentence; a city
// serving its classes from more than one binding is a topology this build
// refuses outright, and that is a STANDING verdict about the city rather than a
// fault this read ran into, so it is marked as one. Reporting either as
// unrelocated would send the fan-out to the directory scan alone — the retained
// pre-migration copies, printed as live rows — which is the confidently stale
// answer this federation exists to close.
func convoyStoreViewsWithBinding(cityPath string, views []convoyStoreView) ([]convoyStoreView, error) {
	relocated, isRelocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		return views, standingStorageRefusal{err: err}
	}
	binding := relocated.Store
	if !isRelocated || binding == nil {
		return views, nil
	}
	if refusal, refused := binding.(refusedClassStore); refused {
		return views, refusal.err
	}
	for _, view := range views {
		// The scan already opened this exact store, so the binding is not a
		// second leg — it is the one that is there. Adding it would duplicate
		// every row against itself. This is relocatedGraphLegFrom's identity
		// gate, applied to a list of views.
		if view.store == binding {
			return views, nil
		}
	}

	merged := make([]convoyStoreView, 0, len(views)+1)
	for _, view := range views {
		if samePath(view.path, cityPath) {
			view.role = convoyViewMigrationSource
		}
		merged = append(merged, view)
	}
	return append(merged, convoyStoreView{
		path:  convoyBindingViewPath,
		store: binding,
		role:  convoyViewClassBinding,
	}), nil
}

// convoyStoreViewsForRead is convoyStoreViewsWithBinding for the surfaces that
// only READ, which continue past a refusal and say so.
//
// A refused city still serves work from its work ledger, so failing the whole
// listing would take `gc beads list` and `gc convoy list` away from every city
// whose storage configuration is mid-repair. The rows the binding holds are
// missing from that output, though, so the omission is announced — every run,
// not once per process, because each invocation is a fresh answer and has to
// carry its own caveat.
//
// The caveat says both halves, because on a converged city the omission is not
// the whole hazard. The migration RETAINED a frozen copy of every row it moved,
// and it is the binding's answer that supersedes those copies — so while the
// refusal stands they are no longer superseded and print as live rows in place
// of the ones that are missing. A convoy the city closed in the binding lists
// open again for exactly as long as the binding cannot be read.
func convoyStoreViewsForRead(cityPath string, views []convoyStoreView, stderr io.Writer, cmdName string) []convoyStoreView {
	merged, err := convoyStoreViewsWithBinding(cityPath, views)
	if err != nil {
		fmt.Fprintf(stderr, "%s: the city's relocated class binding is unreadable, so any beads it holds are missing from this output and retained pre-migration copies of them may appear in their place as stale open rows: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
	}
	return merged
}

// convoyOwnershipProbeBatch bounds how many ids one ownership probe puts on the
// wire, because an unbounded IN-list is a query no sqlite binding can run.
//
// The IDs filter compiles to one bound variable per id (`b.id IN (?,...,?)` in
// sqliteListSQL, with no chunking of its own) and modernc sqlite refuses any
// statement carrying more than 32766 of them. The candidate set is every
// migration-source row matching the caller's query and `gc beads list` runs
// unbounded, so on a converged city with a long history the probe would carry
// the whole work ledger and come back "too many SQL variables" — failing the
// read outright on exactly the large migrated cities the federation exists for.
// Batching under the cap keeps the probe O(candidates/batch) round trips rather
// than the one-per-row it replaced.
const convoyOwnershipProbeBatch = 30000

// convoyBindingOwnedIDs asks the class binding which of these ids it HOLDS.
//
// This is deliberately not "which of these ids did the binding return for the
// caller's query". Those are different questions, and answering the second one
// is how a migrated city ends up serving frozen rows: `gc convoy list` asks
// every store for its OPEN convoys, so the moment the city closes a relocated
// convoy in the binding — the normal end of every migrated workflow, not an edge
// case — the binding stops answering for that id, the retained copy stops
// looking superseded, and a convoy the city finished prints open forever.
//
// So the ownership probe carries only the ids it is asking about and
// IncludeClosed, and no filter of the caller's reaches it. ListQuery.IDs is the
// IN-list form of a batch of Gets and counts as a filter, so it needs no
// AllowScan and costs one query per batch rather than one per row.
//
// A probe that FAILS is an error, never an empty answer. Reading a fault as "the
// binding holds none of these" would serve every frozen twin as live data and
// say nothing about why.
func convoyBindingOwnedIDs(binding beads.Store, ids []string) (map[string]bool, error) {
	owned := make(map[string]bool, len(ids))
	if binding == nil || len(ids) == 0 {
		return owned, nil
	}
	for start := 0; start < len(ids); start += convoyOwnershipProbeBatch {
		batch := ids[start:min(start+convoyOwnershipProbeBatch, len(ids))]
		held, err := binding.List(beads.ListQuery{IDs: batch, IncludeClosed: true})
		if err != nil {
			return nil, fmt.Errorf("asking %s which ids it holds: %w", convoyBindingViewPath, err)
		}
		for _, b := range held {
			owned[b.ID] = true
		}
	}
	return owned, nil
}

// mergeConvoyViewRows folds one row set per view into one, dropping the frozen
// copies a class binding supersedes.
//
// Ids are unique only WITHIN a store, so this is not an id dedupe: a city and a
// rig that each mint "gc-1" hold two different beads and both belong in the
// output. The one place an id means the same bead twice is the migration, which
// copies a class into the binding with ids preserved and deletes nothing — so
// the migration source's copy is a frozen duplicate of a row the binding now
// owns. That pair, and only that pair, collapses, with the binding winning.
//
// Which is why the supersede question is asked of the BINDING and only about the
// migration source's ids, rather than read off the two row sets. A rig's id is
// never put to the binding at all, so the role scoping is structural; and the
// answer does not depend on whether the binding's own row happened to survive
// the caller's filter, so a relocated bead the city has since closed still
// supersedes its twin.
func mergeConvoyViewRows[T any](views []convoyStoreView, rows [][]T, idOf func(T) string) ([]T, error) {
	var binding beads.Store
	for i, view := range views {
		if view.role == convoyViewClassBinding && len(rows) > i {
			binding = view.store
		}
	}
	var candidates []string
	for i, view := range views {
		if view.role != convoyViewMigrationSource || len(rows) <= i {
			continue
		}
		for _, row := range rows[i] {
			candidates = append(candidates, idOf(row))
		}
	}
	owned, err := convoyBindingOwnedIDs(binding, candidates)
	if err != nil {
		return nil, err
	}
	merged := make([]T, 0)
	for i, view := range views {
		if len(rows) <= i {
			continue
		}
		for _, row := range rows[i] {
			if view.role == convoyViewMigrationSource && owned[idOf(row)] {
				continue
			}
			merged = append(merged, row)
		}
	}
	return merged, nil
}

func openConvoyStores(cfg *config.City, cityPath, beadID string, openStore func(string) (beads.Store, error)) ([]convoyStoreView, error) {
	var (
		stores   []convoyStoreView
		firstErr error
	)
	for _, dir := range convoyStoreCandidates(cfg, cityPath, beadID) {
		store, err := openStore(dir)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stores = append(stores, convoyStoreView{path: dir, store: store, role: convoyViewOrdinary})
	}
	if len(stores) > 0 {
		return stores, nil
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("no convoy stores available")
}

func resolveConvoyStore(convoyID string, cfg *config.City, cityPath string, openStore func(string) (beads.Store, error)) (beads.Store, error) {
	store, _, err := resolveOwningStoreDir(convoyID, cfg, cityPath, openStore)
	return store, err
}

// resolveOwningStoreDir resolves the store that owns beadID and the candidate
// store directory it was found in, probing each prefix-aware convoy store
// candidate rooted at cityPath. It returns an error when beadID resolves in
// more than one store (ambiguous) and beads.ErrNotFound when no candidate
// holds it.
//
// The candidate set is the convoy class-store ordering (the graph store the
// convoy bead lives in, plus the per-rig work stores its members may live in).
// The scan does not stop at the first hit: it probes every candidate so a bead
// present in more than one store is rejected rather than silently resolved to
// one, enforcing the "resolution requires a uniquely addressable bead id"
// contract. A candidate's not-found probe is skipped; any other error is
// returned immediately. The returned directory maps back to the owning
// candidate.
//
// # The binding leg comes first, and it short-circuits the uniqueness contract
//
// A relocated class binding is not one of the city's directories, so the scan
// below cannot reach it. On a converged city that means an infrastructure bead
// is answered by the copy `gc storage migrate` RETAINED in the city store —
// frozen at migration time — and the caller then writes through it. So the
// binding is resolved first, through the shared residency contract.
//
// A binding hit returns without consulting the ambiguity rule, deliberately.
// Dual residency is not ambiguity: the migration copies with ids preserved and
// deletes nothing, so a relocated bead is SUPPOSED to exist twice and there is
// a known winner. Refusing it as ambiguous would break every convoy command on
// exactly the cities that completed the migration.
//
// The directory reported for a binding hit is the CITY's, not the binding's,
// and that is not a fudge. Callers map this directory to a store-ref
// ("city:<name>" / "rig:<name>") that scopes molecule-root lookups. These beads
// lived in the city store and carried the city's ref before the migration moved
// them; a binding is not a rig and has no ref of its own, and inventing one
// would strand every root recorded before the move.
func resolveOwningStoreDir(beadID string, cfg *config.City, cityPath string, openStore func(string) (beads.Store, error)) (beads.Store, string, error) {
	owner, ownedByBinding, err := cliByIDBindingOwner(cityPath, beadID)
	if err != nil {
		return nil, "", err
	}
	if ownedByBinding {
		if err := refuseBindingRigCollision(beadID, cfg, cityPath, openStore); err != nil {
			return nil, "", err
		}
		return owner.Store, cityPath, nil
	}

	candidates, err := openConvoyStores(cfg, cityPath, beadID, openStore)
	if err != nil {
		return nil, "", err
	}
	var (
		foundStore beads.Store
		foundDir   string
	)
	for _, candidate := range candidates {
		if candidate.store == nil {
			continue
		}
		if _, err := candidate.store.Get(beadID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, "", err
		}
		if foundStore != nil {
			return nil, "", fmt.Errorf("bead %s exists in multiple stores (%s and %s); resolution requires a uniquely addressable bead id", beadID, foundDir, candidate.path)
		}
		foundStore = candidate.store
		foundDir = candidate.path
	}
	if foundStore == nil {
		return nil, "", beads.ErrNotFound
	}
	return foundStore, foundDir, nil
}

// refuseBindingRigCollision restores the uniqueness contract to the one case
// the binding short-circuit takes it away from without meaning to.
//
// A binding hit returns before the scan runs, and the scan is where "this id
// exists in more than one store" is refused. Skipping it is right for the CITY
// store, whose copy of a relocated id is the migration working as designed, and
// wrong for a RIG: a rig is never a migration target, so it has no retained copy
// to be excused, and one holding the same id is two ledgers disagreeing by
// accident. Answering that silently from the binding means whichever caller
// follows — a close, a land, a convoy walk — acts on one copy while the other
// stays open forever, which is the failure the contract was written for.
//
// So the probe is the rigs and only the rigs, and only for an id no reserved
// class prefix claims. A reserved prefix is minted by the binding and by nothing
// else, so a rig cannot hold one legitimately and the walk would cost every
// reserved-id resolution — bd's on-close hook among them, and it closes in
// bursts — to learn nothing.
//
// Read faults are refusals, not skips. This runs in front of callers that
// MUTATE, and a rig that cannot answer has not established the id is uniquely
// addressable; the error policy is the scan's own, one function down, for the
// same probe.
func refuseBindingRigCollision(beadID string, cfg *config.City, cityPath string, openStore func(string) (beads.Store, error)) error {
	if bdIDIsClassReserved(beadID) {
		return nil
	}
	candidates, err := openConvoyStores(cfg, cityPath, beadID, openStore)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate.store == nil || samePath(candidate.path, cityPath) {
			continue
		}
		if _, err := candidate.store.Get(beadID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return err
		}
		return fmt.Errorf("bead %s exists in multiple stores (%s and %s); resolution requires a uniquely addressable bead id", beadID, convoyBindingViewPath, candidate.path)
	}
	return nil
}

func openAllConvoyStores(stderr io.Writer, cmdName string) ([]convoyStoreView, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return openAllConvoyStoresAt(cityPath, stderr, cmdName)
}

// openAllConvoyStoresAt is openAllConvoyStores with a pre-resolved cityPath,
// used by routed callers that already resolved the city before dispatching
// to the fallback path.
func openAllConvoyStoresAt(cityPath string, stderr io.Writer, cmdName string) ([]convoyStoreView, int) {
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	emitLoadCityConfigWarnings(stderr, prov)
	stores, err := openConvoyStores(cfg, cityPath, "", func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return stores, 0
}

type convoyWithStore struct {
	store beads.Store
	bead  beads.Bead
}

type convoyProgressJSON struct {
	Closed         int `json:"closed"`
	Total          int `json:"total"`
	DanglingTracks int `json:"dangling_tracks,omitempty"`
}

type convoyFieldsJSON struct {
	Owner  string `json:"owner,omitempty"`
	Notify string `json:"notify,omitempty"`
	Merge  string `json:"merge,omitempty"`
	Target string `json:"target,omitempty"`
}

type convoySummaryJSON struct {
	ID       string             `json:"id"`
	Title    string             `json:"title"`
	Status   string             `json:"status"`
	Progress convoyProgressJSON `json:"progress"`
	Owned    bool               `json:"owned"`
	Fields   convoyFieldsJSON   `json:"fields,omitempty"`
	ChildIDs []string           `json:"child_ids,omitempty"`
}

type convoyListSummaryJSON struct {
	Total int `json:"total"`
}

type convoyListResultJSON struct {
	SchemaVersion string                `json:"schema_version"`
	Convoys       []convoySummaryJSON   `json:"convoys"`
	Summary       convoyListSummaryJSON `json:"summary"`
}

type convoyChildJSON struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	Type          string `json:"type"`
	Assignee      string `json:"assignee,omitempty"`
	DanglingTrack bool   `json:"dangling_track,omitempty"`
}

type convoyDetailJSON struct {
	ID     string           `json:"id"`
	Title  string           `json:"title"`
	Status string           `json:"status"`
	Owned  bool             `json:"owned"`
	Fields convoyFieldsJSON `json:"fields,omitempty"`
	Labels []string         `json:"labels,omitempty"`
}

type convoyStatusResultJSON struct {
	SchemaVersion string             `json:"schema_version"`
	Convoy        convoyDetailJSON   `json:"convoy"`
	Progress      convoyProgressJSON `json:"progress"`
	Children      []convoyChildJSON  `json:"children"`
}

func collectOpenConvoys(stores []convoyStoreView) ([]convoyWithStore, error) {
	rows := make([][]convoyWithStore, len(stores))
	for i, candidate := range stores {
		all, err := candidate.store.List(beads.ListQuery{Type: "convoy"})
		if err != nil {
			return nil, err
		}
		for _, b := range all {
			rows[i] = append(rows[i], convoyWithStore{store: candidate.store, bead: b})
		}
	}
	convoys, err := mergeConvoyViewRows(stores, rows, func(c convoyWithStore) string { return c.bead.ID })
	if err != nil {
		return nil, err
	}
	sort.SliceStable(convoys, func(i, j int) bool {
		if convoys[i].bead.ID == convoys[j].bead.ID {
			return convoys[i].bead.Title < convoys[j].bead.Title
		}
		return convoys[i].bead.ID < convoys[j].bead.ID
	})
	return convoys, nil
}

func convoyProgressFromChildren(children []beads.Bead) convoyProgressJSON {
	progress := convoyProgressJSON{Total: len(children)}
	for _, ch := range children {
		if convoycore.IsTerminalStatus(ch.Status) {
			progress.Closed++
		}
		if convoycore.IsUnresolvedTrackedItem(ch) {
			progress.DanglingTracks++
		}
	}
	return progress
}

func formatConvoyProgress(progress convoyProgressJSON) string {
	text := fmt.Sprintf("%d/%d closed", progress.Closed, progress.Total)
	if progress.DanglingTracks > 0 {
		suffix := "tracks"
		if progress.DanglingTracks == 1 {
			suffix = "track"
		}
		text += fmt.Sprintf(" (%d dangling %s)", progress.DanglingTracks, suffix)
	}
	return text
}

func openConvoyStoreByID(convoyID string, stderr io.Writer, cmdName string) (beads.Store, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return openConvoyStoreByIDAt(convoyID, cityPath, stderr, cmdName)
}

// openConvoyStoreByIDAt is openConvoyStoreByID with a pre-resolved cityPath,
// used by routed callers that already resolved the city before dispatching
// to a fallback or mutation path.
func openConvoyStoreByIDAt(convoyID, cityPath string, stderr io.Writer, cmdName string) (beads.Store, int) {
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	emitLoadCityConfigWarnings(stderr, prov)
	store, err := resolveConvoyStore(convoyID, cfg, cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

// doConvoyList lists open convoys with progress counts.
func doConvoyList(store beads.Store, stdout, stderr io.Writer) int {
	return doConvoyListAcrossStores([]convoyStoreView{{store: store}}, false, stdout, stderr)
}

func listConvoyChildren(store beads.Store, convoyID string, includeClosed bool) ([]beads.Bead, error) {
	return convoycore.Members(store, convoyID, includeClosed)
}

func doConvoyListAcrossStores(stores []convoyStoreView, jsonOut bool, stdout, stderr io.Writer) int {
	convoys, err := collectOpenConvoys(stores)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		return writeConvoyListJSON(convoys, stdout, stderr)
	}

	if len(convoys) == 0 {
		fmt.Fprintln(stdout, "No open convoys") //nolint:errcheck // best-effort stdout
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tPROGRESS") //nolint:errcheck // best-effort stdout
	for _, c := range convoys {
		children, err := listConvoyChildren(c.store, c.bead.ID, true)
		if err != nil {
			fmt.Fprintf(stderr, "gc convoy list: children of %s: %v\n", c.bead.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		progress := convoyProgressFromChildren(children)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.bead.ID, c.bead.Title, formatConvoyProgress(progress)) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

func writeConvoyListJSON(convoys []convoyWithStore, stdout, stderr io.Writer) int {
	items := make([]convoySummaryJSON, 0, len(convoys))
	for _, c := range convoys {
		children, err := listConvoyChildren(c.store, c.bead.ID, true)
		if err != nil {
			fmt.Fprintf(stderr, "gc convoy list: children of %s: %v\n", c.bead.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		item := convoySummaryFromBead(c.bead, children)
		items = append(items, item)
	}
	if err := writeCLIJSONLine(stdout, convoyListResultJSON{
		SchemaVersion: "1",
		Convoys:       items,
		Summary:       convoyListSummaryJSON{Total: len(items)},
	}); err != nil {
		fmt.Fprintf(stderr, "gc convoy list: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func convoySummaryFromBead(convoy beads.Bead, children []beads.Bead) convoySummaryJSON {
	childIDs := make([]string, 0, len(children))
	for _, ch := range children {
		childIDs = append(childIDs, ch.ID)
	}
	return convoySummaryJSON{
		ID:       convoy.ID,
		Title:    convoy.Title,
		Status:   convoy.Status,
		Progress: convoyProgressFromChildren(children),
		Owned:    hasLabel(convoy.Labels, "owned"),
		Fields:   convoyFieldsFromBead(convoy),
		ChildIDs: childIDs,
	}
}

func convoyFieldsFromBead(convoy beads.Bead) convoyFieldsJSON {
	fields := getConvoyFields(convoy)
	return convoyFieldsJSON{
		Owner:  fields.Owner,
		Notify: fields.Notify,
		Merge:  fields.Merge,
		Target: fields.Target,
	}
}

func newConvoyStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status <id>",
		Short: "Show detailed convoy status",
		Long: `Show detailed status of a convoy and all its child issues.

Displays the convoy's ID, title, status, completion progress, and a
table of all child issues with their status and assignee.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdConvoyStatus(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// cmdConvoyStatus is the CLI entry point for convoy status.
func cmdConvoyStatus(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return doConvoyStatusWithJSON(nil, args, jsonOut, stdout, stderr)
	}
	convoyID := args[0]
	return routeReadCmd("convoy status", stderr, convoyStatusAPIClient, func(cityPath string, c *api.Client, nilReason string) int {
		return routeConvoyStatus(cityPath, convoyID, c, nilReason, jsonOut, stdout, stderr)
	})
}

// convoyStatusAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back.
var convoyStatusAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeConvoyStatus dispatches `convoy status` to the supervisor API when a
// controller is up; otherwise falls back to the local store resolver.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeConvoyStatus(cityPath, convoyID string, c *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	var cr api.CachedRead[api.ConvoyStatusView]
	return routeRead(c, "convoy status", nilReason, stderr,
		func() error {
			var err error
			if cr, err = c.GetConvoy(convoyID); err != nil {
				return err
			}
			// Graph/workflow convoys return an empty Convoy.ID — force a fallback
			// so the workflow-aware local path can render it.
			if cr.Body.Convoy.ID == "" {
				return fallbackAfterFetch{Reason: "workflow-convoy"}
			}
			return nil
		},
		func() int { return renderConvoyStatusFromAPI(cr, jsonOut, stdout, stderr) },
		func() int { return doConvoyStatusFallback(cityPath, convoyID, jsonOut, stdout, stderr) },
	)
}

// renderConvoyStatusFromAPI formats the API-sourced convoy detail to match
// doConvoyStatus output. Stale banner appends when cache age > 30s.
func renderConvoyStatusFromAPI(cr api.CachedRead[api.ConvoyStatusView], jsonOut bool, stdout, stderr io.Writer) int {
	convoy := cr.Body.Convoy
	children := cr.Body.Children
	progress := cr.Body.Progress
	if jsonOut {
		return writeConvoyStatusJSON(convoy, children, convoyProgressFromAPI(api.ConvoyCheckView{
			Closed: progress.Closed,
			Total:  progress.Total,
		}), stdout, stderr)
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("Convoy:   %s", convoy.ID))
	w(fmt.Sprintf("Title:    %s", convoy.Title))
	w(fmt.Sprintf("Status:   %s", convoy.Status))
	w(fmt.Sprintf("Progress: %d/%d closed", progress.Closed, progress.Total))
	fields := getConvoyFields(convoy)
	if hasLabel(convoy.Labels, "owned") {
		w("Lifecycle: owned")
	}
	if fields.Target != "" {
		w(fmt.Sprintf("Target:   %s", fields.Target))
	}
	if fields.Owner != "" {
		w(fmt.Sprintf("Owner:    %s", fields.Owner))
	}
	if fields.Notify != "" {
		w(fmt.Sprintf("Notify:   %s", fields.Notify))
	}
	if fields.Merge != "" {
		w(fmt.Sprintf("Merge:    %s", fields.Merge))
	}
	if len(children) > 0 {
		w("")
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tASSIGNEE") //nolint:errcheck // best-effort stdout
		for _, ch := range children {
			assignee := ch.Assignee
			if assignee == "" {
				assignee = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ch.ID, ch.Title, ch.Status, assignee) //nolint:errcheck // best-effort stdout
		}
		tw.Flush() //nolint:errcheck // best-effort stdout
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck
	}
	return 0
}

// doConvoyStatusFallback is the direct-bd path for "gc convoy status".
func doConvoyStatusFallback(cityPath, convoyID string, jsonOut bool, stdout, stderr io.Writer) int {
	store, code := openConvoyStoreByIDAt(convoyID, cityPath, stderr, "gc convoy status")
	if store == nil {
		return code
	}
	return doConvoyStatusWithJSON(store, []string{convoyID}, jsonOut, stdout, stderr)
}

// doConvoyStatus shows detailed status of a convoy and its children.
func doConvoyStatus(store beads.Store, args []string, stdout, stderr io.Writer) int {
	return doConvoyStatusWithJSON(store, args, false, stdout, stderr)
}

func doConvoyStatusWithJSON(store beads.Store, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc convoy status: missing convoy ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	convoy, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if convoy.Type != "convoy" {
		fmt.Fprintf(stderr, "gc convoy status: bead %s is not a convoy\n", id) //nolint:errcheck // best-effort stderr
		return 1
	}

	children, err := listConvoyChildren(store, id, true)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	progress := convoyProgressFromChildren(children)

	if jsonOut {
		return writeConvoyStatusJSON(convoy, children, progress, stdout, stderr)
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("Convoy:   %s", convoy.ID))
	w(fmt.Sprintf("Title:    %s", convoy.Title))
	w(fmt.Sprintf("Status:   %s", convoy.Status))
	w(fmt.Sprintf("Progress: %s", formatConvoyProgress(progress)))
	fields := getConvoyFields(convoy)
	if hasLabel(convoy.Labels, "owned") {
		w("Lifecycle: owned")
	}
	if fields.Target != "" {
		w(fmt.Sprintf("Target:   %s", fields.Target))
	}
	if fields.Owner != "" {
		w(fmt.Sprintf("Owner:    %s", fields.Owner))
	}
	if fields.Notify != "" {
		w(fmt.Sprintf("Notify:   %s", fields.Notify))
	}
	if fields.Merge != "" {
		w(fmt.Sprintf("Merge:    %s", fields.Merge))
	}

	if len(children) > 0 {
		w("")
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tASSIGNEE") //nolint:errcheck // best-effort stdout
		for _, ch := range children {
			assignee := ch.Assignee
			if assignee == "" {
				assignee = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ch.ID, ch.Title, ch.Status, assignee) //nolint:errcheck // best-effort stdout
		}
		tw.Flush() //nolint:errcheck // best-effort stdout
	}
	return 0
}

func writeConvoyStatusJSON(convoy beads.Bead, children []beads.Bead, progress convoyProgressJSON, stdout, stderr io.Writer) int {
	childItems := make([]convoyChildJSON, 0, len(children))
	for _, ch := range children {
		childItems = append(childItems, convoyChildJSON{
			ID:            ch.ID,
			Title:         ch.Title,
			Status:        ch.Status,
			Type:          ch.Type,
			Assignee:      ch.Assignee,
			DanglingTrack: convoycore.IsUnresolvedTrackedItem(ch),
		})
	}
	if err := writeCLIJSONLine(stdout, convoyStatusResultJSON{
		SchemaVersion: "1",
		Convoy: convoyDetailJSON{
			ID:     convoy.ID,
			Title:  convoy.Title,
			Status: convoy.Status,
			Owned:  hasLabel(convoy.Labels, "owned"),
			Fields: convoyFieldsFromBead(convoy),
			Labels: convoy.Labels,
		},
		Progress: progress,
		Children: childItems,
	}); err != nil {
		fmt.Fprintf(stderr, "gc convoy status: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func newConvoyTargetCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "target <convoy-id> <branch>",
		Short: "Set the target branch on a convoy",
		Long: `Set the target branch metadata on a convoy.

Child work beads can inherit this target branch when slung with
feature-branch formulas such as mol-polecat-work.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdConvoyTargetJSON(args, true, stdout, stderr)
			} else {
				code = cmdConvoyTarget(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

func cmdConvoyTarget(args []string, stdout, stderr io.Writer) int {
	return cmdConvoyTargetJSON(args, false, stdout, stderr)
}

func cmdConvoyTargetJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		return doConvoyTargetJSON(nil, args, jsonOut, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy target")
	if store == nil {
		return code
	}
	return doConvoyTargetJSON(store, args, jsonOut, stdout, stderr)
}

func doConvoyTarget(store beads.Store, args []string, stdout, stderr io.Writer) int {
	return doConvoyTargetJSON(store, args, false, stdout, stderr)
}

func doConvoyTargetJSON(store beads.Store, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "gc convoy target: missing convoy ID or branch") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]
	target := strings.TrimSpace(args[1])
	if target == "" {
		fmt.Fprintln(stderr, "gc convoy target: target branch cannot be empty") //nolint:errcheck // best-effort stderr
		return 1
	}

	convoy, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy target: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if convoy.Type != "convoy" {
		fmt.Fprintf(stderr, "gc convoy target: bead %s is not a convoy\n", id) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := setConvoyFields(store, id, ConvoyFields{Target: target}); err != nil {
		fmt.Fprintf(stderr, "gc convoy target: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy target", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.target", Action: "target", ConvoyID: id, Target: target})
	}
	fmt.Fprintf(stdout, "Set target of convoy %s to %s\n", id, target) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "add <convoy-id> <issue-id>",
		Short: "Add an issue to a convoy",
		Long: `Link an existing issue bead to a convoy.

Adds a tracks dependency from the convoy to the issue, making it appear
in the convoy's progress tracking without changing the issue parent.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdConvoyAddJSON(args, true, stdout, stderr)
			} else {
				code = cmdConvoyAdd(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// cmdConvoyAdd is the CLI entry point for adding an issue to a convoy.
func cmdConvoyAdd(args []string, stdout, stderr io.Writer) int {
	return cmdConvoyAddJSON(args, false, stdout, stderr)
}

func cmdConvoyAddJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		return doConvoyAddJSON(nil, args, jsonOut, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy add")
	if store == nil {
		return code
	}
	return doConvoyAddJSON(store, args, jsonOut, stdout, stderr)
}

// doConvoyAdd adds an issue to a convoy by recording a tracks dependency.
func doConvoyAdd(store beads.Store, args []string, stdout, stderr io.Writer) int {
	return doConvoyAddJSON(store, args, false, stdout, stderr)
}

func doConvoyAddJSON(store beads.Store, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "gc convoy add: usage: gc convoy add <convoy-id> <issue-id>") //nolint:errcheck // best-effort stderr
		return 1
	}
	convoyID := args[0]
	issueID := args[1]

	convoy, err := store.Get(convoyID)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if convoy.Type != "convoy" {
		fmt.Fprintf(stderr, "gc convoy add: bead %s is not a convoy\n", convoyID) //nolint:errcheck // best-effort stderr
		return 1
	}

	if _, err := store.Get(issueID); err != nil {
		fmt.Fprintf(stderr, "gc convoy add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := convoycore.TrackItem(store, convoyID, issueID); err != nil {
		fmt.Fprintf(stderr, "gc convoy add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy add", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.add", Action: "add", ConvoyID: convoyID, IssueIDs: []string{issueID}})
	}
	fmt.Fprintf(stdout, "Added %s to convoy %s\n", issueID, convoyID) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "close <id>",
		Short: "Close a convoy",
		Long: `Close a convoy bead manually.

Marks the convoy as closed regardless of child issue status. Use
"gc convoy check" to auto-close convoys where all issues are resolved.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			code := 0
			if jsonOut {
				code = cmdConvoyCloseJSON(args, true, stdout, stderr)
			} else {
				code = cmdConvoyClose(args, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// cmdConvoyClose is the CLI entry point for closing a convoy.
func cmdConvoyClose(args []string, stdout, stderr io.Writer) int {
	return cmdConvoyCloseJSON(args, false, stdout, stderr)
}

func cmdConvoyCloseJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return doConvoyCloseJSON(nil, events.Discard, args, jsonOut, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy close")
	if store == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doConvoyCloseJSON(store, rec, args, jsonOut, stdout, stderr)
}

// doConvoyClose closes a convoy bead.
func doConvoyClose(store beads.Store, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doConvoyCloseJSON(store, rec, args, false, stdout, stderr)
}

func doConvoyCloseJSON(store beads.Store, rec events.Recorder, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc convoy close: missing convoy ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	id := args[0]

	convoy, err := store.Get(id)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if convoy.Type != "convoy" {
		fmt.Fprintf(stderr, "gc convoy close: bead %s is not a convoy\n", id) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := closeConvoyWithReason(store, id, convoyManualCloseReason); err != nil {
		fmt.Fprintf(stderr, "gc convoy close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec.Record(events.Event{
		Type:    events.ConvoyClosed,
		Actor:   eventActor(),
		Subject: id,
	})

	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy close", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.close", Action: "close", ConvoyID: id})
	}
	fmt.Fprintf(stdout, "Closed convoy %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Auto-close convoys where all issues are closed",
		Long: `Scan open convoys and auto-close any where all child issues are resolved.

Evaluates each open convoy's children. If all children have status
"closed", the convoy is automatically closed and an event is recorded.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			code := 0
			if jsonOut {
				code = cmdConvoyCheckJSON(true, stdout, stderr)
			} else {
				code = cmdConvoyCheck(stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// cmdConvoyCheck is the CLI entry point for auto-closing completed convoys.
// It routes through the supervisor API to discover convoys + completion
// state, then performs the close mutations via local bd. Falls back to the
// all-local multi-store iterator when the API is unavailable.
func cmdConvoyCheck(stdout, stderr io.Writer) int {
	return cmdConvoyCheckJSON(false, stdout, stderr)
}

func cmdConvoyCheckJSON(jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return routeConvoyCheck(cityPath, nil, "requires-live-read", jsonOut, stdout, stderr)
}

// routeConvoyCheck always uses the local live store path because the command
// may auto-close convoys. The supervisor API is cache-backed, and cached data
// must not drive state mutations.
func routeConvoyCheck(cityPath string, _ *api.Client, nilReason string, jsonOut bool, stdout, stderr io.Writer) int {
	const cmdName = "convoy check"
	reason := nilReason
	if reason == "" {
		reason = "requires-live-read"
	}
	logRoute(stderr, cmdName, "fallback", reason)
	return doConvoyCheckFallback(cityPath, jsonOut, stdout, stderr)
}

// doConvoyCheckFallback is the direct-bd path for "gc convoy check".
func doConvoyCheckFallback(cityPath string, jsonOut bool, stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStoresAt(cityPath, stderr, "gc convoy check")
	if stores == nil {
		return code
	}
	stores = convoyStoreViewsForRead(cityPath, stores, stderr, "gc convoy check")
	rec := openCityRecorderAt(cityPath, stderr)
	return doConvoyCheckAcrossStoresJSON(stores, rec, jsonOut, stdout, stderr)
}

// hasLabel reports whether the labels slice contains the target label.
func hasLabel(labels []string, target string) bool { //nolint:unparam // general-purpose helper
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// convoyAutocloseReason is the close_reason metadata value stamped on
// convoys auto-closed because all of their children are closed. The
// 38-character form satisfies bd's validation.on-close=error length
// requirement while remaining a meaningful audit-trail entry.
const convoyAutocloseReason = "convoy autoclose: all children closed"

const convoyManualCloseReason = "convoy close: requested by operator"

const convoyLandCloseReason = "convoy land: completed owned convoy"

type explicitReasonCloser interface {
	CloseWithReason(id, reason string) error
}

// closeConvoyWithReason stamps a close_reason metadata key on the
// convoy bead before closing it. BdStore can receive the same reason
// directly as `bd close --reason ...`, which lets cities
// running with validation.on-close=error accept system-driven
// auto-closes (whose default reason "Closed" would otherwise be
// rejected as terse). For stores whose Close path does not consult
// the metadata, the field still serves as a permanent audit trail of
// why the convoy was closed.
func closeConvoyWithReason(store beads.Store, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return store.Close(id)
	}
	if err := store.SetMetadata(id, "close_reason", reason); err != nil {
		return fmt.Errorf("stamping convoy %s close reason: %w", id, err)
	}
	if closer, ok := store.(explicitReasonCloser); ok {
		return closer.CloseWithReason(id, reason)
	}
	return store.Close(id)
}

// doConvoyCheck auto-closes convoys where all children are closed.
// Convoys with the "owned" label are skipped — their lifecycle is
// managed manually.
func doConvoyCheck(store beads.Store, rec events.Recorder, stdout, stderr io.Writer) int {
	return doConvoyCheckAcrossStores([]convoyStoreView{{store: store}}, rec, stdout, stderr)
}

func doConvoyCheckAcrossStores(stores []convoyStoreView, rec events.Recorder, stdout, stderr io.Writer) int {
	return doConvoyCheckAcrossStoresJSON(stores, rec, false, stdout, stderr)
}

func doConvoyCheckAcrossStoresJSON(stores []convoyStoreView, rec events.Recorder, jsonOut bool, stdout, stderr io.Writer) int {
	convoys, err := collectOpenConvoys(stores)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	closed := 0
	for _, item := range convoys {
		if hasLabel(item.bead.Labels, "owned") {
			continue
		}
		children, err := listConvoyChildren(item.store, item.bead.ID, true)
		if err != nil {
			fmt.Fprintf(stderr, "gc convoy check: children of %s: %v\n", item.bead.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if len(children) == 0 {
			continue
		}
		allClosed := true
		for _, ch := range children {
			if !convoycore.IsTerminalStatus(ch.Status) {
				allClosed = false
				break
			}
		}
		if allClosed {
			if err := closeConvoyWithReason(item.store, item.bead.ID, convoyAutocloseReason); err != nil {
				fmt.Fprintf(stderr, "gc convoy check: closing %s: %v\n", item.bead.ID, err) //nolint:errcheck // best-effort stderr
				return 1
			}
			rec.Record(events.Event{
				Type:    events.ConvoyClosed,
				Actor:   eventActor(),
				Subject: item.bead.ID,
			})
			if !jsonOut {
				fmt.Fprintf(stdout, "Auto-closed convoy %s %q\n", item.bead.ID, item.bead.Title) //nolint:errcheck // best-effort stdout
			}
			closed++
		}
	}

	if jsonOut {
		if err := writeCLIJSONLine(stdout, convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.check", Action: "check", Closed: intRef(closed)}); err != nil {
			fmt.Fprintf(stderr, "gc convoy check: writing JSON result: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "%d convoy(s) auto-closed\n", closed) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func newConvoyStrandedCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stranded",
		Short: "Find convoys with ready work but no workers",
		Long: `Find open issues in convoys that have no assignee.

Lists issues that are ready for work but not claimed by any agent.
Useful for identifying bottlenecks in convoy processing.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			code := 0
			if jsonOut {
				code = cmdConvoyStrandedJSON(true, stdout, stderr)
			} else {
				code = cmdConvoyStranded(stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// cmdConvoyStranded is the CLI entry point for finding stranded convoys.
func cmdConvoyStranded(stdout, stderr io.Writer) int {
	return cmdConvoyStrandedJSON(false, stdout, stderr)
}

func cmdConvoyStrandedJSON(jsonOut bool, stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStores(stderr, "gc convoy stranded")
	if stores == nil {
		return code
	}
	return doConvoyStrandedAcrossStoresJSON(stores, jsonOut, stdout, stderr)
}

// doConvoyStranded finds open convoys with open children that have no assignee.
func doConvoyStranded(store beads.Store, stdout, stderr io.Writer) int {
	return doConvoyStrandedAcrossStores([]convoyStoreView{{store: store}}, stdout, stderr)
}

func doConvoyStrandedAcrossStores(stores []convoyStoreView, stdout, stderr io.Writer) int {
	return doConvoyStrandedAcrossStoresJSON(stores, false, stdout, stderr)
}

func doConvoyStrandedAcrossStoresJSON(stores []convoyStoreView, jsonOut bool, stdout, stderr io.Writer) int {
	convoys, err := collectOpenConvoys(stores)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy stranded: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	type strandedItem struct {
		convoyID string
		issue    beads.Bead
	}
	var items []strandedItem

	for _, item := range convoys {
		children, err := listConvoyChildren(item.store, item.bead.ID, false)
		if err != nil {
			fmt.Fprintf(stderr, "gc convoy stranded: children of %s: %v\n", item.bead.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		for _, ch := range children {
			if convoycore.IsUnresolvedTrackedItem(ch) {
				continue
			}
			if !convoycore.IsTerminalStatus(ch.Status) && ch.Assignee == "" {
				items = append(items, strandedItem{convoyID: item.bead.ID, issue: ch})
			}
		}
	}

	if len(items) == 0 {
		if jsonOut {
			return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy stranded", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.stranded", Action: "stranded", Stranded: intRef(0)})
		}
		fmt.Fprintln(stdout, "No stranded work") //nolint:errcheck // best-effort stdout
		return 0
	}
	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy stranded", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.stranded", Action: "stranded", Stranded: intRef(len(items))})
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONVOY\tISSUE\tTITLE") //nolint:errcheck // best-effort stdout
	for _, item := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", item.convoyID, item.issue.ID, item.issue.Title) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

// --- gc convoy land ---

func newConvoyLandCmd(stdout, stderr io.Writer) *cobra.Command {
	var force, dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "land <convoy-id>",
		Short: "Land an owned convoy (terminate + cleanup)",
		Long: `Land an owned convoy, verifying all children are closed.

Landing is the natural lifecycle termination for owned convoys created
via "gc sling --owned". It verifies all children are closed (or uses
--force), closes the convoy bead, and records a ConvoyClosed event.`,
		Example: `  gc convoy land gc-42
  gc convoy land gc-42 --force
  gc convoy land gc-42 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts := landOpts{
				Force:  force,
				DryRun: dryRun,
			}
			code := 0
			if jsonOut {
				code = cmdConvoyLandJSON(args, opts, true, stdout, stderr)
			} else {
				code = cmdConvoyLand(args, opts, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "land even with open children")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview what would happen")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

// landOpts controls the behavior of the land command.
type landOpts struct {
	Force  bool
	DryRun bool
}

// cmdConvoyLand is the CLI entry point for landing a convoy.
func cmdConvoyLand(args []string, opts landOpts, stdout, stderr io.Writer) int {
	return cmdConvoyLandJSON(args, opts, false, stdout, stderr)
}

func cmdConvoyLandJSON(args []string, opts landOpts, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return doConvoyLandJSON(nil, events.Discard, args, opts, jsonOut, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy land")
	if store == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doConvoyLandJSON(store, rec, args, opts, jsonOut, stdout, stderr)
}

// doConvoyLand verifies an owned convoy's children are closed, optionally
// cleans up worktrees, closes the convoy bead, and records an event.
func doConvoyLand(store beads.Store, rec events.Recorder, args []string, opts landOpts, stdout, stderr io.Writer) int {
	return doConvoyLandJSON(store, rec, args, opts, false, stdout, stderr)
}

func doConvoyLandJSON(store beads.Store, rec events.Recorder, args []string, opts landOpts, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc convoy land: missing convoy ID") //nolint:errcheck // best-effort stderr
		return 1
	}
	convoyID := args[0]

	convoy, err := store.Get(convoyID)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy land: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if convoy.Type != "convoy" {
		fmt.Fprintf(stderr, "gc convoy land: bead %s is not a convoy\n", convoyID) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !hasLabel(convoy.Labels, "owned") {
		fmt.Fprintf(stderr, "gc convoy land: convoy %s is not owned (missing 'owned' label)\n", convoyID) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Already closed → idempotent success.
	if convoycore.IsTerminalStatus(convoy.Status) {
		if jsonOut {
			return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy land", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.land", Action: "land", ConvoyID: convoyID, Title: convoy.Title, AlreadyClosed: true, DryRun: opts.DryRun, Forced: opts.Force})
		}
		fmt.Fprintf(stdout, "Convoy %s already closed\n", convoyID) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Check children.
	children, err := listConvoyChildren(store, convoyID, true)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy land: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	var openChildren []beads.Bead
	for _, ch := range children {
		if !convoycore.IsTerminalStatus(ch.Status) {
			openChildren = append(openChildren, ch)
		}
	}

	if len(openChildren) > 0 && !opts.Force {
		fmt.Fprintf(stderr, "gc convoy land: %d open child(ren):\n", len(openChildren)) //nolint:errcheck // best-effort stderr
		for _, ch := range openChildren {
			fmt.Fprintf(stderr, "  %s %s (%s)\n", ch.ID, ch.Title, ch.Status) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr, "Use --force to land anyway") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Dry-run: preview what would happen.
	if opts.DryRun {
		if jsonOut {
			return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy land", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.land", Action: "land", ConvoyID: convoyID, Title: convoy.Title, TotalChildren: intRef(len(children)), OpenChildren: intRef(len(openChildren)), DryRun: true, Forced: opts.Force})
		}
		fmt.Fprintf(stdout, "Would land convoy %s %q\n", convoyID, convoy.Title)                 //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "  Children: %d total, %d open\n", len(children), len(openChildren)) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Close the convoy.
	if err := closeConvoyWithReason(store, convoyID, convoyLandCloseReason); err != nil {
		fmt.Fprintf(stderr, "gc convoy land: closing convoy: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec.Record(events.Event{
		Type:    events.ConvoyClosed,
		Actor:   eventActor(),
		Subject: convoyID,
	})

	// Notification.
	fields := getConvoyFields(convoy)
	switch {
	case jsonOut:
		return writeCLIJSONLineOrExit(stdout, stderr, "gc convoy land", convoyActionResult{SchemaVersion: "1", OK: true, Command: "convoy.land", Action: "land", ConvoyID: convoyID, Title: convoy.Title, TotalChildren: intRef(len(children)), OpenChildren: intRef(len(openChildren)), Forced: opts.Force, Notify: fields.Notify})
	case fields.Notify != "":
		fmt.Fprintf(stdout, "Landed convoy %s %q (notify: %s)\n", convoyID, convoy.Title, fields.Notify) //nolint:errcheck // best-effort stdout
	default:
		fmt.Fprintf(stdout, "Landed convoy %s %q\n", convoyID, convoy.Title) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// --- gc convoy autoclose (hidden — called by bd on_close hook) ---

func newConvoyAutocloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "autoclose <bead-id>",
		Short:  "Auto-close completed convoys for a closed bead",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			doConvoyAutoclose(args[0], stdout, stderr)
			return nil // always succeed — best-effort infrastructure
		},
	}
}

// doConvoyAutoclose is the CLI entry point for convoy autoclose.
// It resolves the store that owns the closed bead and delegates to the
// testable core.
func doConvoyAutoclose(beadID string, stdout, stderr io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	storeRoot := convoyAutocloseStoreRoot(cwd)
	cityPath := autocloseCityPathForStoreRoot(storeRoot)
	rec := openCityRecorderAt(cityPath, stderr)

	// The bd on_close hook is spawned from the supervisor and inherits its
	// cwd/env, so storeRoot resolves to the supervisor's (city) store even
	// when the closed bead lives in a rig store. Resolve the store that
	// actually owns the bead — prefix-aware, across the city and every rig —
	// so rig-store closes autoclose their convoys instead of silently
	// no-op'ing (#3411).
	if store, _, ok := autocloseOwningStore(beadID, cityPath); ok {
		doConvoyAutocloseWith(store, rec, beadID, stdout, stderr)
		return
	}

	// Fallback: a standalone store reachable only via cwd/BEADS_DIR/
	// GC_STORE_ROOT (e.g. an external rig checkout with no city.toml), or a
	// city whose config could not be loaded. Preserve the original
	// single-store resolution.
	store, err := openStoreAtForCity(storeRoot, cityPath)
	if err != nil {
		return
	}
	doConvoyAutocloseWith(store, rec, beadID, stdout, stderr)
}

// autocloseOwningStore resolves the store that owns beadID, and the store
// directory it was found in, by probing each prefix-aware convoy store
// candidate (city + rigs) rooted at cityPath. It returns ok=false when the
// city config cannot be loaded or no candidate store holds the bead, so the
// caller can fall back to cwd-rooted resolution. The store directory lets
// molecule autoclose derive the matching store-ref label.
//
// A resolution that FAILED is still ok=false — the hooks cannot be made fatal
// without wedging every close on a city with a sick store — but it is announced
// first. See warnAutocloseResolutionFault.
func autocloseOwningStore(beadID, cityPath string) (beads.Store, string, bool) {
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, "", false
	}
	store, dir, err := resolveOwningStoreDir(beadID, cfg, cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		if !errors.Is(err, beads.ErrNotFound) {
			warnAutocloseResolutionFault(beadID, err)
		}
		return nil, "", false
	}
	return store, dir, true
}

// autocloseFaultOnce bounds warnAutocloseResolutionFault to one line per
// process.
var autocloseFaultOnce sync.Once

// warnAutocloseResolutionFault announces a by-id resolution that failed rather
// than missed, once per process.
//
// The autoclose hooks are best-effort by design: bd's on_close must not fail
// because a molecule root could not be found, so every error here is swallowed
// and the caller falls back to the cwd-rooted store. That is right for absence
// and wrong for a fault. A binding that cannot be read, or an id the scan
// refuses as ambiguous, makes the fallback close a root in a ledger that may
// not hold it — the silent wrong-store write this lane exists to end — and it
// cannot be made fatal without wedging closes on a city whose binding is sick.
// So it is made LOUD instead.
//
// Once per process, because bd closes beads in bursts: a persistent fault would
// otherwise print the same line on every close of a drain and bury the hook log
// it is trying to be seen in.
func warnAutocloseResolutionFault(beadID string, err error) {
	autocloseFaultOnce.Do(func() {
		fmt.Fprintf(cliStorageStderr, "gc autoclose: resolving the store that owns %s failed (%v); falling back to the cwd-rooted store, which may not hold it. Further occurrences this process are not repeated.\n", beadID, err) //nolint:errcheck // best-effort stderr
	})
}

func convoyAutocloseStoreRoot(cwd string) string {
	if root := strings.TrimSpace(os.Getenv("GC_STORE_ROOT")); root != "" {
		if !filepath.IsAbs(root) {
			root = filepath.Join(cwd, root)
		}
		return filepath.Clean(root)
	}
	if beadsDir := strings.TrimSpace(os.Getenv("BEADS_DIR")); beadsDir != "" {
		if !filepath.IsAbs(beadsDir) {
			beadsDir = filepath.Join(cwd, beadsDir)
		}
		return filepath.Clean(filepath.Dir(beadsDir))
	}
	return cwd
}

// autocloseCityPathForStoreRoot resolves the runtime city for bd hook cleanup.
// Precedence: a city.toml-backed discovery result from the store root wins
// outright; otherwise a validated explicit GC_CITY from the supervising
// process is preferred over a legacy `.gc/`-only discovery result (an external
// rig checkout with runtime state but no city.toml); the legacy runtime root
// is used when no explicit city is set; cityForStoreDir is the final fallback
// when discovery fails entirely.
func autocloseCityPathForStoreRoot(storeRoot string) string {
	if cityPath, err := findCity(storeRoot); err == nil {
		if _, statErr := os.Stat(filepath.Join(cityPath, "city.toml")); statErr == nil {
			return cityPath
		}
		if explicitCity, ok := resolveExplicitCityPathEnv(); ok {
			return explicitCity
		}
		return cityPath
	}
	return cityForStoreDir(storeRoot)
}

// doConvoyAutocloseWith checks whether the closed bead's legacy parent or
// tracks dependents are convoys with all children closed, and if so closes
// them. All errors are silently swallowed — this is best-effort
// infrastructure called from a bd hook script.
func doConvoyAutocloseWith(store beads.Store, rec events.Recorder, beadID string, stdout, _ io.Writer) {
	bead, err := store.Get(beadID)
	if err != nil {
		return
	}

	seen := make(map[string]bool)
	if bead.ParentID != "" {
		parent, err := store.Get(bead.ParentID)
		if err == nil {
			seen[parent.ID] = true
			autocloseConvoyIfComplete(store, rec, parent, stdout)
		}
	}

	trackingConvoys, err := convoycore.TrackingConvoysForItem(store, beadID)
	if err != nil {
		return
	}
	for _, convoy := range trackingConvoys {
		if seen[convoy.ID] {
			continue
		}
		seen[convoy.ID] = true
		autocloseConvoyIfComplete(store, rec, convoy, stdout)
	}
}

func autocloseConvoyIfComplete(store beads.Store, rec events.Recorder, convoy beads.Bead, stdout io.Writer) {
	if convoy.Type != "convoy" || convoycore.IsTerminalStatus(convoy.Status) || hasLabel(convoy.Labels, "owned") {
		return
	}

	children, err := listConvoyChildren(store, convoy.ID, true)
	if err != nil || len(children) == 0 {
		return
	}
	for _, ch := range children {
		if !convoycore.IsTerminalStatus(ch.Status) {
			return
		}
	}

	if err := closeConvoyWithReason(store, convoy.ID, convoyAutocloseReason); err != nil {
		return
	}

	rec.Record(events.Event{
		Type:    events.ConvoyClosed,
		Actor:   eventActor(),
		Subject: convoy.ID,
	})

	fmt.Fprintf(stdout, "Auto-closed convoy %s %q\n", convoy.ID, convoy.Title) //nolint:errcheck // best-effort stdout
}
