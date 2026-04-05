package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newConvoyCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "convoy",
		Short: "Manage convoys — graphs of related work",
		Long: `Manage convoys — graphs of related work beads.

A convoy is a named graph of beads with dependencies. Simple convoys
group related issues via parent-child relationships. Complex convoys
use formula-compiled DAGs with control beads for orchestration.`,
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
	var owned bool
	cmd := &cobra.Command{
		Use:   "create <name> [issue-ids...]",
		Short: "Create a convoy and optionally track issues",
		Long: `Create a convoy and optionally link existing issues to it.

Creates a convoy bead and sets the parent of any provided issue IDs to
the new convoy. Issues can also be added later with "gc convoy add".`,
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
			if cmdConvoyCreateWithOptions(args, opts, stdout, stderr) != 0 {
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
	return cmd
}

func cmdConvoyCreateWithOptions(args []string, opts convoyCreateOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	readDoltPort(cityPath)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Determine which store to use: if children are provided, use the
	// first child's rig store so convoy and children share a database.
	// This avoids cross-store parent references that bd can't resolve.
	storeDir := cityPath
	if len(args) > 1 {
		if rd := rigDirForBead(cfg, args[1]); rd != "" {
			storeDir = rd
		}
	}
	store, err := openStoreAtForCity(storeDir, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy create: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec := openCityRecorder(stderr)
	return doConvoyCreateWithOptions(store, cfg, cityPath, rec, args, opts, stdout, stderr)
}

// doConvoyCreate creates a convoy bead and optionally adds issues to it.
// When cfg/cityPath are nil/empty, all beads are assumed to be in the same store.
func doConvoyCreate(store beads.Store, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
	return doConvoyCreateWithOptions(store, nil, "", rec, args, convoyCreateOptions{}, stdout, stderr)
}

func doConvoyCreateWithOptions(store beads.Store, cfg *config.City, cityPath string, rec events.Recorder, args []string, opts convoyCreateOptions, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc convoy create: missing convoy name") //nolint:errcheck // best-effort stderr
		return 1
	}
	name := args[0]
	issueIDs := args[1:]

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
		parentID := convoy.ID
		if err := childStore.Update(id, beads.UpdateOpts{ParentID: &parentID}); err != nil {
			fmt.Fprintf(stderr, "gc convoy create: setting parent on %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	rec.Record(events.Event{
		Type:    events.ConvoyCreated,
		Actor:   eventActor(),
		Subject: convoy.ID,
		Message: name,
	})

	if len(issueIDs) > 0 {
		fmt.Fprintf(stdout, "Created convoy %s %q tracking %d issue(s)\n", convoy.ID, name, len(issueIDs)) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Created convoy %s %q\n", convoy.ID, name) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func newConvoyListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List open convoys with progress",
		Long: `List all open convoys with completion progress.

Shows each convoy's ID, title, and the number of closed vs total
child issues.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdConvoyList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdConvoyList is the CLI entry point for listing convoys.
func cmdConvoyList(stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStores(stderr, "gc convoy list")
	if stores == nil {
		return code
	}
	return doConvoyListAcrossStores(stores, stdout, stderr)
}

func convoyStoreCandidates(cfg *config.City, cityPath, beadID string) []string {
	if rawBeadsProvider(cityPath) == "file" {
		return []string{cityPath}
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
			add(rd)
		}
	}
	add(cityPath)
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			add(rig.Path)
		}
	}
	return candidates
}

type convoyStoreView struct {
	path  string
	store beads.Store
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
		stores = append(stores, convoyStoreView{path: dir, store: store})
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
	stores, err := openConvoyStores(cfg, cityPath, convoyID, openStore)
	if err != nil {
		return nil, err
	}
	var foundStore beads.Store
	foundDir := ""
	for _, candidate := range stores {
		store := candidate.store
		if _, err := store.Get(convoyID); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if foundStore != nil {
			return nil, fmt.Errorf("convoy %s exists in multiple stores (%s and %s); direct convoy commands require a uniquely resolvable convoy id", convoyID, foundDir, candidate.path)
		}
		foundStore = store
		foundDir = candidate.path
	}
	if foundStore != nil {
		return foundStore, nil
	}
	return nil, beads.ErrNotFound
}

func openAllConvoyStores(stderr io.Writer, cmdName string) ([]convoyStoreView, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	readDoltPort(cityPath)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
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

func collectOpenConvoys(stores []convoyStoreView) ([]convoyWithStore, error) {
	convoys := make([]convoyWithStore, 0)
	for _, candidate := range stores {
		all, err := candidate.store.List(beads.ListQuery{Type: "convoy"})
		if err != nil {
			return nil, err
		}
		for _, b := range all {
			convoys = append(convoys, convoyWithStore{store: candidate.store, bead: b})
		}
	}
	sort.SliceStable(convoys, func(i, j int) bool {
		if convoys[i].bead.ID == convoys[j].bead.ID {
			return convoys[i].bead.Title < convoys[j].bead.Title
		}
		return convoys[i].bead.ID < convoys[j].bead.ID
	})
	return convoys, nil
}

func openConvoyStoreByID(convoyID string, stderr io.Writer, cmdName string) (beads.Store, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	readDoltPort(cityPath)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
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
	return doConvoyListAcrossStores([]convoyStoreView{{store: store}}, stdout, stderr)
}

func listConvoyChildren(store beads.Store, parentID string, includeClosed bool) ([]beads.Bead, error) {
	return store.List(beads.ListQuery{
		ParentID:      parentID,
		IncludeClosed: includeClosed,
		Sort:          beads.SortCreatedAsc,
	})
}

func doConvoyListAcrossStores(stores []convoyStoreView, stdout, stderr io.Writer) int {
	convoys, err := collectOpenConvoys(stores)
	if err != nil {
		fmt.Fprintf(stderr, "gc convoy list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
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
		closed := 0
		for _, ch := range children {
			if ch.Status == "closed" {
				closed++
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%d closed\n", c.bead.ID, c.bead.Title, closed, len(children)) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status <id>",
		Short: "Show detailed convoy status",
		Long: `Show detailed status of a convoy and all its child issues.

Displays the convoy's ID, title, status, completion progress, and a
table of all child issues with their status and assignee.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdConvoyStatus(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdConvoyStatus is the CLI entry point for convoy status.
func cmdConvoyStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return doConvoyStatus(nil, args, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy status")
	if store == nil {
		return code
	}
	return doConvoyStatus(store, args, stdout, stderr)
}

// doConvoyStatus shows detailed status of a convoy and its children.
func doConvoyStatus(store beads.Store, args []string, stdout, stderr io.Writer) int {
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

	closed := 0
	for _, ch := range children {
		if ch.Status == "closed" {
			closed++
		}
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("Convoy:   %s", convoy.ID))
	w(fmt.Sprintf("Title:    %s", convoy.Title))
	w(fmt.Sprintf("Status:   %s", convoy.Status))
	w(fmt.Sprintf("Progress: %d/%d closed", closed, len(children)))
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

func newConvoyTargetCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "target <convoy-id> <branch>",
		Short: "Set the target branch on a convoy",
		Long: `Set the target branch metadata on a convoy.

Child work beads can inherit this target branch when slung with
feature-branch formulas such as mol-polecat-work.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdConvoyTarget(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdConvoyTarget(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		return doConvoyTarget(nil, args, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy target")
	if store == nil {
		return code
	}
	return doConvoyTarget(store, args, stdout, stderr)
}

func doConvoyTarget(store beads.Store, args []string, stdout, stderr io.Writer) int {
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

	fmt.Fprintf(stdout, "Set target of convoy %s to %s\n", id, target) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyAddCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "add <convoy-id> <issue-id>",
		Short: "Add an issue to a convoy",
		Long: `Link an existing issue bead to a convoy.

Sets the issue's parent to the convoy ID, making it appear in the
convoy's progress tracking.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdConvoyAdd(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdConvoyAdd is the CLI entry point for adding an issue to a convoy.
func cmdConvoyAdd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		return doConvoyAdd(nil, args, stdout, stderr)
	}
	convoyID := ""
	if len(args) > 0 {
		convoyID = args[0]
	}
	store, code := openConvoyStoreByID(convoyID, stderr, "gc convoy add")
	if store == nil {
		return code
	}
	return doConvoyAdd(store, args, stdout, stderr)
}

// doConvoyAdd adds an issue to a convoy by setting the issue's ParentID.
func doConvoyAdd(store beads.Store, args []string, stdout, stderr io.Writer) int {
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

	if err := store.Update(issueID, beads.UpdateOpts{ParentID: &convoyID}); err != nil {
		fmt.Fprintf(stderr, "gc convoy add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Added %s to convoy %s\n", issueID, convoyID) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "close <id>",
		Short: "Close a convoy",
		Long: `Close a convoy bead manually.

Marks the convoy as closed regardless of child issue status. Use
"gc convoy check" to auto-close convoys where all issues are resolved.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdConvoyClose(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdConvoyClose is the CLI entry point for closing a convoy.
func cmdConvoyClose(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return doConvoyClose(nil, events.Discard, args, stdout, stderr)
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
	return doConvoyClose(store, rec, args, stdout, stderr)
}

// doConvoyClose closes a convoy bead.
func doConvoyClose(store beads.Store, rec events.Recorder, args []string, stdout, stderr io.Writer) int {
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

	if err := store.Close(id); err != nil {
		fmt.Fprintf(stderr, "gc convoy close: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rec.Record(events.Event{
		Type:    events.ConvoyClosed,
		Actor:   eventActor(),
		Subject: id,
	})

	fmt.Fprintf(stdout, "Closed convoy %s\n", id) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Auto-close convoys where all issues are closed",
		Long: `Scan open convoys and auto-close any where all child issues are resolved.

Evaluates each open convoy's children. If all children have status
"closed", the convoy is automatically closed and an event is recorded.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdConvoyCheck(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdConvoyCheck is the CLI entry point for auto-closing completed convoys.
func cmdConvoyCheck(stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStores(stderr, "gc convoy check")
	if stores == nil {
		return code
	}
	rec := openCityRecorder(stderr)
	return doConvoyCheckAcrossStores(stores, rec, stdout, stderr)
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

// doConvoyCheck auto-closes convoys where all children are closed.
// Convoys with the "owned" label are skipped — their lifecycle is
// managed manually.
func doConvoyCheck(store beads.Store, rec events.Recorder, stdout, stderr io.Writer) int {
	return doConvoyCheckAcrossStores([]convoyStoreView{{store: store}}, rec, stdout, stderr)
}

func doConvoyCheckAcrossStores(stores []convoyStoreView, rec events.Recorder, stdout, stderr io.Writer) int {
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
			if ch.Status != "closed" {
				allClosed = false
				break
			}
		}
		if allClosed {
			if err := item.store.Close(item.bead.ID); err != nil {
				fmt.Fprintf(stderr, "gc convoy check: closing %s: %v\n", item.bead.ID, err) //nolint:errcheck // best-effort stderr
				return 1
			}
			rec.Record(events.Event{
				Type:    events.ConvoyClosed,
				Actor:   eventActor(),
				Subject: item.bead.ID,
			})
			fmt.Fprintf(stdout, "Auto-closed convoy %s %q\n", item.bead.ID, item.bead.Title) //nolint:errcheck // best-effort stdout
			closed++
		}
	}

	fmt.Fprintf(stdout, "%d convoy(s) auto-closed\n", closed) //nolint:errcheck // best-effort stdout
	return 0
}

func newConvoyStrandedCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "stranded",
		Short: "Find convoys with ready work but no workers",
		Long: `Find open issues in convoys that have no assignee.

Lists issues that are ready for work but not claimed by any agent.
Useful for identifying bottlenecks in convoy processing.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdConvoyStranded(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdConvoyStranded is the CLI entry point for finding stranded convoys.
func cmdConvoyStranded(stdout, stderr io.Writer) int {
	stores, code := openAllConvoyStores(stderr, "gc convoy stranded")
	if stores == nil {
		return code
	}
	return doConvoyStrandedAcrossStores(stores, stdout, stderr)
}

// doConvoyStranded finds open convoys with open children that have no assignee.
func doConvoyStranded(store beads.Store, stdout, stderr io.Writer) int {
	return doConvoyStrandedAcrossStores([]convoyStoreView{{store: store}}, stdout, stderr)
}

func doConvoyStrandedAcrossStores(stores []convoyStoreView, stdout, stderr io.Writer) int {
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
			if ch.Status != "closed" && ch.Assignee == "" {
				items = append(items, strandedItem{convoyID: item.bead.ID, issue: ch})
			}
		}
	}

	if len(items) == 0 {
		fmt.Fprintln(stdout, "No stranded work") //nolint:errcheck // best-effort stdout
		return 0
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
	var force, dryRun bool
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
			if cmdConvoyLand(args, opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "land even with open children")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview what would happen")
	return cmd
}

// landOpts controls the behavior of the land command.
type landOpts struct {
	Force  bool
	DryRun bool
}

// cmdConvoyLand is the CLI entry point for landing a convoy.
func cmdConvoyLand(args []string, opts landOpts, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return doConvoyLand(nil, events.Discard, args, opts, stdout, stderr)
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
	return doConvoyLand(store, rec, args, opts, stdout, stderr)
}

// doConvoyLand verifies an owned convoy's children are closed, optionally
// cleans up worktrees, closes the convoy bead, and records an event.
func doConvoyLand(store beads.Store, rec events.Recorder, args []string, opts landOpts, stdout, stderr io.Writer) int {
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
	if convoy.Status == "closed" {
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
		if ch.Status != "closed" {
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
		fmt.Fprintf(stdout, "Would land convoy %s %q\n", convoyID, convoy.Title)                 //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "  Children: %d total, %d open\n", len(children), len(openChildren)) //nolint:errcheck // best-effort stdout
		return 0
	}

	// Close the convoy.
	if err := store.Close(convoyID); err != nil {
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
	if fields.Notify != "" {
		fmt.Fprintf(stdout, "Landed convoy %s %q (notify: %s)\n", convoyID, convoy.Title, fields.Notify) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Landed convoy %s %q\n", convoyID, convoy.Title) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// --- gc convoy autoclose (hidden — called by bd on_close hook) ---

func newConvoyAutocloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:    "autoclose <bead-id>",
		Short:  "Auto-close parent convoy if all siblings are closed",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			doConvoyAutoclose(args[0], stdout, stderr)
			return nil // always succeed — best-effort infrastructure
		},
	}
}

// doConvoyAutoclose is the CLI entry point for convoy autoclose.
// It creates a cwd-rooted BdStore (matching the bd process that invoked
// the hook) and delegates to the testable core.
func doConvoyAutoclose(beadID string, stdout, stderr io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	store := bdStoreForDir(cwd)
	rec := openCityRecorder(stderr)
	doConvoyAutocloseWith(store, rec, beadID, stdout, stderr)
}

// doConvoyAutocloseWith checks whether the closed bead's parent is a
// convoy with all children closed, and if so closes it. All errors are
// silently swallowed — this is best-effort infrastructure called from
// a bd hook script.
func doConvoyAutocloseWith(store beads.Store, rec events.Recorder, beadID string, stdout, _ io.Writer) {
	bead, err := store.Get(beadID)
	if err != nil || bead.ParentID == "" {
		return
	}

	parent, err := store.Get(bead.ParentID)
	if err != nil || parent.Type != "convoy" || parent.Status == "closed" {
		return
	}
	if hasLabel(parent.Labels, "owned") {
		return
	}

	children, err := listConvoyChildren(store, parent.ID, true)
	if err != nil || len(children) == 0 {
		return
	}
	for _, ch := range children {
		if ch.Status != "closed" {
			return
		}
	}

	if err := store.Close(parent.ID); err != nil {
		return
	}

	rec.Record(events.Event{
		Type:    events.ConvoyClosed,
		Actor:   eventActor(),
		Subject: parent.ID,
	})

	fmt.Fprintf(stdout, "Auto-closed convoy %s %q\n", parent.ID, parent.Title) //nolint:errcheck // best-effort stdout
}
