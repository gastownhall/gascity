package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newGraphCmd(stdout, stderr io.Writer) *cobra.Command {
	var mermaid, tree bool
	cmd := &cobra.Command{
		Use:   "graph <bead-ids|convoy-id...>",
		Short: "Show dependency graph for beads",
		Long: `Show the dependency graph for a set of beads or a convoy.

Resolves dependencies via the bead store and prints each bead with its
status and what blocks it. Convoys are expanded to their children
automatically. Readiness is computed within the displayed set.

By default prints a table. Use --tree for a Unicode tree view or
--mermaid for a Mermaid.js flowchart you can paste into Markdown.`,
		Example: `  gc graph gc-42               # expand convoy children
  gc graph gc-1 gc-2 gc-3     # arbitrary beads
  gc graph gc-42 --tree        # dependency tree
  gc graph gc-42 --mermaid     # Mermaid.js diagram`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			opts := graphOpts{Mermaid: mermaid, Tree: tree}
			if cmdGraph(args, opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&mermaid, "mermaid", false, "output Mermaid.js flowchart")
	cmd.Flags().BoolVar(&tree, "tree", false, "output Unicode dependency tree")
	cmd.MarkFlagsMutuallyExclusive("mermaid", "tree")
	return cmd
}

// graphOpts controls graph output format.
type graphOpts struct {
	Mermaid bool
	Tree    bool
}

// cmdGraph is the CLI entry point.
func cmdGraph(args []string, opts graphOpts, stdout, stderr io.Writer) int {
	store, code := openRigAwareStore(args, stderr, "gc graph")
	if store == nil {
		return code
	}
	return doGraph(store, args, opts, stdout, stderr)
}

// openRigAwareStore opens a bead store, routing to the correct rig directory
// if the first bead arg has a rig prefix. Uses rig-level Dolt config when
// the rig has its own Dolt server.
func openRigAwareStore(args []string, stderr io.Writer, cmdName string) (beads.Store, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	readDoltPort(cityPath)

	// Try to resolve rig from the first bead arg's prefix.
	if len(args) > 0 {
		cfg, cfgErr := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if cfgErr == nil {
			if storeDir := slingDirForBead(cfg, cityPath, args[0]); storeDir != cityPath {
				store := bdStoreForRig(storeDir, cityPath, cfg)
				return store, 0
			}
		}
	}

	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

// graphNode holds a bead and its resolved dependency edges.
type graphNode struct {
	bead        beads.Bead
	blockedBy   []string // IDs of beads in the set that block this one (all edges)
	openBlocker []string // IDs of open beads in the set that block this one
}

// isBlockingDep reports whether a dependency type represents a blocking
// relationship for readiness computation. Non-blocking types like "tracks"
// or "relates-to" do not affect whether a bead is ready.
func isBlockingDep(depType string) bool {
	switch depType {
	case "blocks", "":
		return true
	default:
		return false
	}
}

// doGraph resolves beads and their dependencies, then prints the graph.
func doGraph(store beads.Store, args []string, opts graphOpts, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc graph: missing bead IDs") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Resolve input — expand containers, returning beads directly.
	resolved, err := resolveGraphInput(store, args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc graph: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(resolved) == 0 {
		fmt.Fprintln(stdout, "No beads to graph") //nolint:errcheck // best-effort stdout
		return 0
	}

	// Build set for filtering edges to within-set only.
	inSet := make(map[string]bool, len(resolved))
	for _, b := range resolved {
		inSet[b.ID] = true
	}

	// Fetch dependencies for each bead.
	nodes := make([]graphNode, 0, len(resolved))
	for _, b := range resolved {
		deps, err := store.DepList(b.ID, "down")
		if err != nil {
			fmt.Fprintf(stderr, "gc graph: listing deps for %s: %v\n", b.ID, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		var blockedBy []string
		for _, d := range deps {
			if inSet[d.DependsOnID] && isBlockingDep(d.Type) {
				blockedBy = append(blockedBy, d.DependsOnID)
			}
		}
		sort.Strings(blockedBy)
		nodes = append(nodes, graphNode{bead: b, blockedBy: blockedBy})
	}

	// Second pass: compute open blockers by cross-referencing status.
	closedIDs := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if n.bead.Status == "closed" {
			closedIDs[n.bead.ID] = true
		}
	}
	for i, n := range nodes {
		for _, dep := range n.blockedBy {
			if !closedIDs[dep] {
				nodes[i].openBlocker = append(nodes[i].openBlocker, dep)
			}
		}
	}

	switch {
	case opts.Mermaid:
		printMermaid(nodes, stdout)
	case opts.Tree:
		printTree(nodes, stdout)
	default:
		printTable(nodes, stdout)
	}
	return 0
}

// resolveGraphInput expands convoy inputs to their children.
// Non-containers are passed through. Multiple args are resolved individually.
// Duplicate IDs are removed. Returns the full Bead objects to avoid re-fetching.
func resolveGraphInput(store beads.Store, args []string, stderr io.Writer) ([]beads.Bead, error) {
	seen := make(map[string]bool)
	var result []beads.Bead
	add := func(b beads.Bead) {
		if !seen[b.ID] {
			seen[b.ID] = true
			result = append(result, b)
		}
	}
	for _, arg := range args {
		b, err := store.Get(arg)
		if err != nil {
			return nil, err
		}
		if b.Type == "epic" {
			fmt.Fprintf(stderr, "gc graph: epic %s is treated as an ordinary bead; convoy expansion is first-class\n", b.ID) //nolint:errcheck // best-effort stderr
		}
		if beads.IsContainerType(b.Type) {
			children, err := store.List(beads.ListQuery{
				ParentID: b.ID,
				Sort:     beads.SortCreatedAsc,
			})
			if err != nil {
				return nil, fmt.Errorf("expanding %s %s: %w", b.Type, b.ID, err)
			}
			for _, ch := range children {
				add(ch)
			}
		} else {
			add(b)
		}
	}
	return result, nil
}

// printTable prints the graph as a table with blocked-by and ready columns.
func printTable(nodes []graphNode, stdout io.Writer) {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BEAD\tTITLE\tSTATUS\tBLOCKED BY\tREADY") //nolint:errcheck // best-effort stdout

	ready := 0
	for _, n := range nodes {
		blockedBy := "-"
		if len(n.blockedBy) > 0 {
			blockedBy = strings.Join(n.blockedBy, ", ")
		}

		isReady := isBeadReady(n)
		var readyStr string
		switch {
		case n.bead.Status == "closed":
			readyStr = "done"
		case isReady:
			readyStr = "yes"
			ready++
		default:
			readyStr = "blocked"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck // best-effort stdout
			n.bead.ID, n.bead.Title, n.bead.Status, blockedBy, readyStr)
	}
	tw.Flush() //nolint:errcheck // best-effort stdout

	total := len(nodes)
	closed := 0
	for _, n := range nodes {
		if n.bead.Status == "closed" {
			closed++
		}
	}
	fmt.Fprintf(stdout, "\n%d bead(s): %d closed, %d ready, %d blocked\n", //nolint:errcheck // best-effort stdout
		total, closed, ready, total-closed-ready)
}

// printMermaid outputs a Mermaid.js flowchart.
func printMermaid(nodes []graphNode, stdout io.Writer) {
	fmt.Fprintln(stdout, "graph TD") //nolint:errcheck // best-effort stdout

	for _, n := range nodes {
		label := mermaidLabel(n)
		fmt.Fprintf(stdout, "  %s[\"%s\"]\n", n.bead.ID, label) //nolint:errcheck // best-effort stdout
	}

	// Print edges.
	for _, n := range nodes {
		for _, dep := range n.blockedBy {
			fmt.Fprintf(stdout, "  %s --> %s\n", dep, n.bead.ID) //nolint:errcheck // best-effort stdout
		}
	}

	// Style closed nodes.
	for _, n := range nodes {
		if n.bead.Status == "closed" {
			fmt.Fprintf(stdout, "  style %s fill:#90EE90\n", n.bead.ID) //nolint:errcheck // best-effort stdout
		} else if isBeadReady(n) {
			fmt.Fprintf(stdout, "  style %s fill:#FFD700\n", n.bead.ID) //nolint:errcheck // best-effort stdout
		}
	}
}

// mermaidLabel creates a display label for a mermaid node.
func mermaidLabel(n graphNode) string {
	status := ""
	switch n.bead.Status {
	case "closed":
		status = " done"
	case "in_progress":
		status = " ..."
	}
	// Escape quotes in titles for mermaid safety.
	title := strings.ReplaceAll(n.bead.Title, "\"", "'")
	return fmt.Sprintf("%s%s", title, status)
}

// isBeadReady reports whether a bead has no open blockers.
func isBeadReady(n graphNode) bool {
	return n.bead.Status != "closed" && len(n.openBlocker) == 0
}

// printTree renders the dependency graph as a Unicode tree.
//
// Root nodes (no blockers in the set) are top-level entries. Each node's
// dependents (nodes that it blocks) are rendered as children, producing a
// tree that reads top-down in execution order.
func printTree(nodes []graphNode, stdout io.Writer) {
	// Index nodes by ID for fast lookup.
	byID := make(map[string]graphNode, len(nodes))
	for _, n := range nodes {
		byID[n.bead.ID] = n
	}

	// Build forward edges: blocker → dependents (nodes it unblocks).
	dependents := make(map[string][]string)
	hasBlocker := make(map[string]bool)
	for _, n := range nodes {
		for _, dep := range n.blockedBy {
			dependents[dep] = append(dependents[dep], n.bead.ID)
			hasBlocker[n.bead.ID] = true
		}
	}
	// Sort dependents for deterministic output.
	for k := range dependents {
		sort.Strings(dependents[k])
	}

	// Roots: nodes with no blockers in the set.
	var roots []string
	for _, n := range nodes {
		if !hasBlocker[n.bead.ID] {
			roots = append(roots, n.bead.ID)
		}
	}

	// Track visited nodes to avoid printing duplicates in diamond deps.
	visited := make(map[string]bool)

	var walk func(id, prefix string, isLast bool)
	walk = func(id, prefix string, isLast bool) {
		n, ok := byID[id]
		if !ok {
			return
		}

		// Connector and continuation prefix.
		connector := "├── "
		childPrefix := prefix + "│   "
		if isLast {
			connector = "└── "
			childPrefix = prefix + "    "
		}

		icon := treeStatusIcon(n)
		dup := ""
		if visited[id] {
			dup = " (see above)"
		}
		fmt.Fprintf(stdout, "%s%s%s %s: %s%s\n", prefix, connector, icon, n.bead.ID, n.bead.Title, dup) //nolint:errcheck // best-effort stdout

		if visited[id] {
			return
		}
		visited[id] = true

		children := dependents[id]
		for i, childID := range children {
			walk(childID, childPrefix, i == len(children)-1)
		}
	}

	// Print each root as a top-level tree.
	for i, rootID := range roots {
		n := byID[rootID]
		icon := treeStatusIcon(n)
		fmt.Fprintf(stdout, "%s %s: %s\n", icon, n.bead.ID, n.bead.Title) //nolint:errcheck // best-effort stdout
		visited[rootID] = true

		children := dependents[rootID]
		for j, childID := range children {
			walk(childID, "", j == len(children)-1)
		}

		if i < len(roots)-1 {
			fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		}
	}

	// Summary line.
	total := len(nodes)
	closed, ready := 0, 0
	for _, n := range nodes {
		switch {
		case n.bead.Status == "closed":
			closed++
		case isBeadReady(n):
			ready++
		}
	}
	fmt.Fprintf(stdout, "\n%d bead(s): %d closed, %d ready, %d blocked\n", //nolint:errcheck // best-effort stdout
		total, closed, ready, total-closed-ready)
}

// treeStatusIcon returns a Unicode status icon for a graph node.
func treeStatusIcon(n graphNode) string {
	switch n.bead.Status {
	case "closed":
		return "✓"
	case "in_progress", "hooked":
		return "▶"
	default:
		return "○"
	}
}
