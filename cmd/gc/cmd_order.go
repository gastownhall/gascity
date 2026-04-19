package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/spf13/cobra"
)

func newOrderCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "order",
		Short: "Manage orders (scheduled and event-driven dispatch)",
		Long: `Manage orders — scheduled or event-driven dispatch of formulas and scripts.

Orders live in flat orders/*.toml files. Each order pairs a gate
condition (cooldown, cron, condition, event, or manual) with an action
(a formula or an exec script). The controller evaluates gates on each
tick and dispatches work when a gate opens.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc order: missing subcommand (list, show, run, check, history)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc order: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newOrderListCmd(stdout, stderr),
		newOrderShowCmd(stdout, stderr),
		newOrderRunCmd(stdout, stderr),
		newOrderCheckCmd(stdout, stderr),
		newOrderHistoryCmd(stdout, stderr),
	)
	return cmd
}

func newOrderListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available orders",
		Long: `List all available orders with their gate type, schedule, and target.

Scans orders/ directories for flat .toml files defining gate conditions,
scheduling parameters, and target pools.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdOrderList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newOrderShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of an order",
		Long: `Display detailed information about a named order.

Shows the order name, description, formula reference, gate type,
scheduling parameters, check command, target, and source file.
Use --rig to disambiguate same-name orders in different rigs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdOrderShow(args[0], rig, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to disambiguate same-name orders")
	return cmd
}

func newOrderRunCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Execute an order manually",
		Long: `Execute an order manually, bypassing its gate conditions.

Instantiates a wisp from the order's formula and routes it to the
configured target (if any). Useful for testing orders or triggering
them outside their normal schedule.
Use --rig to disambiguate same-name orders in different rigs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdOrderRun(args[0], rig, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to disambiguate same-name orders")
	return cmd
}

func newOrderCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check which orders are due to run",
		Long: `Evaluate gate conditions for all orders and show which are due.

Prints a table with each order's gate, due status, and reason. Returns
exit code 0 if any order is due, 1 if none are due.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdOrderCheck(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newOrderHistoryCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	cmd := &cobra.Command{
		Use:   "history [name]",
		Short: "Show order execution history",
		Long: `Show execution history for orders.

Queries bead history for past order runs. Optionally filter by order
name. Use --rig to filter by rig.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if cmdOrderHistory(name, rig, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to filter order history")
	return cmd
}

// loadOrders is the common preamble for order commands: resolve city,
// load config, scan formula layers for all orders (city + rig).
func loadOrders(stderr io.Writer, cmdName string) ([]orders.Order, int) {
	_, _, aa, code := loadOrdersWithCity(stderr, cmdName)
	return aa, code
}

func loadOrdersWithCity(stderr io.Writer, cmdName string) (string, *config.City, []orders.Order, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return "", nil, nil, 1
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return "", nil, nil, 1
	}
	aa, code := loadAllOrders(cityPath, cfg, stderr, cmdName)
	return cityPath, cfg, aa, code
}

// loadAllOrders scans city layers + per-rig exclusive layers for orders.
// Rig orders get their Rig field stamped.
func loadAllOrders(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) ([]orders.Order, int) {
	allAA, err := scanAllOrders(cityPath, cfg, stderr, cmdName)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}

	// Apply order overrides from city config.
	if len(cfg.Orders.Overrides) > 0 {
		if err := orders.ApplyOverrides(allAA, convertOverrides(cfg.Orders.Overrides)); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
			return nil, 1
		}
	}

	return allAA, 0
}

func scanAllOrders(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) ([]orders.Order, error) {
	// City-level orders.
	cRoots := cityOrderRoots(cityPath, cfg)
	cLayers := cityFormulaLayers(cityPath, cfg)
	cityAA, err := orders.ScanRoots(fsys.OSFS{}, cRoots, cfg.Orders.Skip)
	if err != nil {
		return nil, err
	}

	// Per-rig orders from rig-exclusive layers.
	var rigAA []orders.Order
	for rigName, layers := range cfg.FormulaLayers.Rigs {
		exclusive := rigExclusiveLayers(layers, cLayers)
		if len(exclusive) == 0 {
			continue
		}
		ra, err := orders.ScanRoots(fsys.OSFS{}, rigOrderRoots(cityPath, cfg, exclusive), cfg.Orders.Skip)
		if err != nil {
			fmt.Fprintf(stderr, "%s: rig %s: %v\n", cmdName, rigName, err) //nolint:errcheck // best-effort stderr
			continue
		}
		for i := range ra {
			ra[i].Rig = rigName
		}
		rigAA = append(rigAA, ra...)
	}

	allAA := make([]orders.Order, 0, len(cityAA)+len(rigAA))
	allAA = append(allAA, cityAA...)
	allAA = append(allAA, rigAA...)
	return allAA, nil
}

// cityFormulaLayers returns the formula directory layers for city-level order
// scanning. Uses FormulaLayers.City if populated (from LoadWithIncludes),
// otherwise falls back to the single formulas dir.
func cityFormulaLayers(cityPath string, cfg *config.City) []string {
	if len(cfg.FormulaLayers.City) > 0 {
		return cfg.FormulaLayers.City
	}
	return []string{citylayout.ResolveFormulasDir(cityPath, cfg.FormulasDir())}
}

func cityOrderRoots(cityPath string, cfg *config.City) []orders.ScanRoot {
	formulaLayers := cityFormulaLayers(cityPath, cfg)
	localFormulas := citylayout.ResolveFormulasDir(cityPath, cfg.FormulasDir())
	roots := make([]orders.ScanRoot, 0, len(formulaLayers)+len(cfg.PackDirs)+2)
	seen := make(map[string]bool, len(formulaLayers)+len(cfg.PackDirs)+2)
	appendRoot := func(root orders.ScanRoot) {
		key := filepath.Clean(root.Dir) + "\n" + filepath.Clean(root.FormulaLayer)
		if seen[key] {
			return
		}
		seen[key] = true
		roots = append(roots, root)
	}

	// Formula layers include system packs (via LoadWithIncludes extraIncludes)
	// and user packs (via workspace.includes). City-local formulas are highest
	// priority and override pack formulas when order names collide.
	for _, layer := range formulaLayers {
		if layer == localFormulas {
			for _, root := range []string{citylayout.OrdersPath(cityPath)} {
				appendRoot(orders.ScanRoot{
					Dir:          root,
					FormulaLayer: localFormulas,
				})
			}
			continue
		}
		appendRoot(orders.ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), "orders"),
			FormulaLayer: layer,
		})
	}

	return roots
}

func rigOrderRoots(_ string, _ *config.City, formulaLayers []string) []orders.ScanRoot {
	roots := make([]orders.ScanRoot, 0, len(formulaLayers))
	for _, layer := range formulaLayers {
		roots = append(roots, orders.ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), "orders"),
			FormulaLayer: layer,
		})
	}
	return roots
}

// --- gc order list ---

func cmdOrderList(stdout, stderr io.Writer) int {
	aa, code := loadOrders(stderr, "gc order list")
	if code != 0 {
		return code
	}
	return doOrderList(aa, stdout)
}

// doOrderList prints a table of orders. Accepts pre-scanned orders for testability.
func doOrderList(aa []orders.Order, stdout io.Writer) int {
	if len(aa) == 0 {
		fmt.Fprintln(stdout, "No orders found.") //nolint:errcheck // best-effort stdout
		return 0
	}

	hasRig := anyOrderHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %-15s %s\n", "NAME", "TYPE", "GATE", "INTERVAL/SCHED", "RIG", "TARGET") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %s\n", "NAME", "TYPE", "GATE", "INTERVAL/SCHED", "TARGET") //nolint:errcheck
	}
	for _, a := range aa {
		typ := "formula"
		if a.IsExec() {
			typ = "exec"
		}
		timing := a.Interval
		if timing == "" {
			timing = a.Schedule
		}
		if timing == "" {
			timing = a.On
		}
		if timing == "" {
			timing = "-"
		}
		pool := a.Pool
		if pool == "" {
			pool = "-"
		}
		rig := a.Rig
		if rig == "" {
			rig = "-"
		}
		if hasRig {
			fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %-15s %s\n", a.Name, typ, a.Gate, timing, rig, pool) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %s\n", a.Name, typ, a.Gate, timing, pool) //nolint:errcheck
		}
	}
	return 0
}

// anyOrderHasRig returns true if any order in the list has a non-empty Rig.
func anyOrderHasRig(aa []orders.Order) bool {
	for _, a := range aa {
		if a.Rig != "" {
			return true
		}
	}
	return false
}

// --- gc order show ---

func cmdOrderShow(name, rig string, stdout, stderr io.Writer) int {
	aa, code := loadOrders(stderr, "gc order show")
	if code != 0 {
		return code
	}
	return doOrderShow(aa, name, rig, stdout, stderr)
}

// doOrderShow prints details of a named order.
func doOrderShow(aa []orders.Order, name, rig string, stdout, stderr io.Writer) int {
	a, ok := findOrder(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc order show: order %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("Order:  %s", a.Name))
	if a.Rig != "" {
		w(fmt.Sprintf("Rig:         %s", a.Rig))
	}
	if a.Description != "" {
		w(fmt.Sprintf("Description: %s", a.Description))
	}
	if a.IsExec() {
		w(fmt.Sprintf("Exec:        %s", a.Exec))
	} else {
		w(fmt.Sprintf("Formula:     %s", a.Formula))
	}
	w(fmt.Sprintf("Gate:        %s", a.Gate))
	if a.Interval != "" {
		w(fmt.Sprintf("Interval:    %s", a.Interval))
	}
	if a.Schedule != "" {
		w(fmt.Sprintf("Schedule:    %s", a.Schedule))
	}
	if a.Check != "" {
		w(fmt.Sprintf("Check:       %s", a.Check))
	}
	if a.On != "" {
		w(fmt.Sprintf("On:          %s", a.On))
	}
	if a.Pool != "" {
		w(fmt.Sprintf("Target:      %s", a.Pool))
	}
	w(fmt.Sprintf("Source:      %s", a.Source))
	return 0
}

// --- gc order run ---

func openCityOrderStore(stderr io.Writer, cmdName string) (beads.Store, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

func openOrderStoreForOrder(cityPath string, cfg *config.City, a orders.Order, stderr io.Writer, cmdName string) (beads.Store, int) {
	target, err := resolveOrderStoreTarget(cityPath, cfg, a)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

func cmdOrderRun(name, rig string, stdout, stderr io.Writer) int {
	cityPath, cfg, aa, code := loadOrdersWithCity(stderr, "gc order run")
	if code != 0 {
		return code
	}
	a, ok := findOrder(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc order run: order %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}
	if a.IsExec() {
		return doOrderRunExec(a, cityPath, cfg, stdout, stderr)
	}
	store, storeCode := openOrderStoreForOrder(cityPath, cfg, a, stderr, "gc order run")
	if store == nil {
		return storeCode
	}

	ep, epCode := openCityEventsProvider(stderr, "gc order run")
	if ep == nil {
		return epCode
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doOrderRun(aa, name, rig, cityPath, store, ep, stdout, stderr)
}

// doOrderRun executes an order manually: instantiates a wisp from the
// order's formula (or runs exec script directly) and routes it to the
// configured target.
func doOrderRun(aa []orders.Order, name, rig, cityPath string, store beads.Store, ep events.Provider, stdout, stderr io.Writer) int {
	a, ok := findOrder(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc order run: order %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Exec orders: run the script directly.
	if a.IsExec() {
		cfg, cfgErr := loadCityConfig(cityPath)
		if cfgErr != nil {
			fmt.Fprintf(stderr, "gc order run: %v\n", cfgErr) //nolint:errcheck // best-effort stderr
			return 1
		}
		return doOrderRunExec(a, cityPath, cfg, stdout, stderr)
	}

	// Capture event head before wisp creation (race-free cursor).
	var headSeq uint64
	if a.Gate == "event" && ep != nil {
		headSeq, _ = ep.LatestSeq()
	}

	scoped := a.ScopedName()
	var cfg *config.City
	var cityName string
	if citylayout.HasCityConfig(cityPath) || citylayout.HasRuntimeRoot(cityPath) {
		var err error
		cfg, err = loadCityConfig(cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		cityName = config.EffectiveCityName(cfg, filepath.Base(cityPath))
	}

	// Compile wisp from formula so graph workflows can be decorated with
	// routing metadata before instantiation.
	var searchPaths []string
	if a.FormulaLayer != "" {
		searchPaths = []string{a.FormulaLayer}
	}
	recipe, err := formula.Compile(context.Background(), a.Formula, searchPaths, nil)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if a.Pool != "" && cfg != nil {
		pool := qualifyPool(a.Pool, a.Rig)
		if err := applyGraphRouting(recipe, nil, pool, nil, "", "", "", "", store, cityName, cityPath, cfg); err != nil {
			fmt.Fprintf(stderr, "gc order run: routing decoration failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	cookResult, err := molecule.Instantiate(context.Background(), store, recipe, molecule.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rootID := cookResult.RootID

	// Track the spawned root in the same store that created it so manual runs
	// stay provider-aware and do not fall back to ambient bd CLI state.
	update := beads.UpdateOpts{
		Labels: []string{"order-run:" + scoped},
	}
	if a.Gate == "event" && ep != nil {
		update.Labels = append(update.Labels,
			"order:"+scoped,
			fmt.Sprintf("seq:%d", headSeq),
		)
	}
	if a.Pool != "" {
		update.Metadata = map[string]string{
			"gc.routed_to": qualifyPool(a.Pool, a.Rig),
		}
	}
	if err := store.Update(rootID, update); err != nil {
		fmt.Fprintf(stderr, "gc order run: labeling wisp: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Order %q executed: wisp %s", name, rootID) //nolint:errcheck
	if a.Pool != "" {
		fmt.Fprintf(stdout, " → gc.routed_to=%s", qualifyPool(a.Pool, a.Rig)) //nolint:errcheck
	}
	fmt.Fprintln(stdout) //nolint:errcheck
	return 0
}

// doOrderRunExec runs an exec order directly via shell.
func doOrderRunExec(a orders.Order, cityPath string, cfg *config.City, stdout, stderr io.Writer) int {
	timeout := a.TimeoutOrDefault()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	target, err := resolveOrderExecTarget(cityPath, cfg, a)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	env := orderExecEnv(cityPath, cfg, target, a)

	output, err := shellExecRunner(ctx, a.Exec, target.ScopeRoot, env)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: exec failed: %v\n", err) //nolint:errcheck
		if len(output) > 0 {
			fmt.Fprintf(stderr, "%s", output) //nolint:errcheck
		}
		return 1
	}
	if len(output) > 0 {
		fmt.Fprintf(stdout, "%s", output) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Order %q executed (exec)\n", a.Name) //nolint:errcheck
	return 0
}

// --- gc order check ---

func cmdOrderCheck(stdout, stderr io.Writer) int {
	cityPath, cfg, aa, code := loadOrdersWithCity(stderr, "gc order check")
	if code != 0 {
		return code
	}

	ep, epCode := openCityEventsProvider(stderr, "gc order check")
	if ep == nil {
		return epCode
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doOrderCheckWithStoresResolver(aa, time.Now(), ep, cachedOrderStoresResolver(cityPath, cfg), stdout, stderr)
}

// orderLastRunFn returns a LastRunFunc that queries BdStore for the most
// recent bead labeled order-run:<name>. Returns zero time if never run.
func orderLastRunFn(store beads.Store) orders.LastRunFunc {
	return func(name string) (time.Time, error) {
		label := "order-run:" + name
		results, err := store.List(beads.ListQuery{
			Label:         label,
			Limit:         1,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
		})
		if err != nil {
			return time.Time{}, err
		}
		if len(results) == 0 {
			return time.Time{}, nil
		}
		return results[0].CreatedAt, nil
	}
}

func orderLastRunFnAcrossStores(stores ...beads.Store) orders.LastRunFunc {
	fns := make([]orders.LastRunFunc, 0, len(stores))
	for _, store := range stores {
		if store != nil {
			fns = append(fns, orderLastRunFn(store))
		}
	}
	return func(name string) (time.Time, error) {
		var latest time.Time
		for _, fn := range fns {
			t, err := fn(name)
			if err != nil {
				return time.Time{}, err
			}
			if t.After(latest) {
				latest = t
			}
		}
		return latest, nil
	}
}

// doOrderCheck evaluates gates for all orders and prints a table.
// Returns 0 if any are due, 1 if none are due.
func doOrderCheck(aa []orders.Order, now time.Time, lastRunFn orders.LastRunFunc, stdout io.Writer) int {
	if len(aa) == 0 {
		fmt.Fprintln(stdout, "No orders found.") //nolint:errcheck // best-effort stdout
		return 1
	}

	hasRig := anyOrderHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", "NAME", "GATE", "RIG", "DUE", "REASON") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", "NAME", "GATE", "DUE", "REASON") //nolint:errcheck
	}
	anyDue := false
	for _, a := range aa {
		result := orders.CheckGate(a, now, lastRunFn, nil, nil)
		due := "no"
		if result.Due {
			due = "yes"
			anyDue = true
		}
		if hasRig {
			rig := a.Rig
			if rig == "" {
				rig = "-"
			}
			fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", a.Name, a.Gate, rig, due, result.Reason) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", a.Name, a.Gate, due, result.Reason) //nolint:errcheck
		}
	}

	if anyDue {
		return 0
	}
	return 1
}

type (
	orderStoreResolver  func(orders.Order) (beads.Store, error)
	orderStoresResolver func(orders.Order) ([]beads.Store, error)
)

func cachedOrderStoresResolver(cityPath string, cfg *config.City) orderStoresResolver {
	stores := make(map[string]beads.Store)
	openCached := func(target execStoreTarget) (beads.Store, error) {
		key := orderStoreTargetKey(target)
		if store, ok := stores[key]; ok {
			return store, nil
		}
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		stores[key] = store
		return store, nil
	}
	return func(a orders.Order) ([]beads.Store, error) {
		target, err := resolveOrderStoreTarget(cityPath, cfg, a)
		if err != nil {
			return nil, err
		}
		primary, err := openCached(target)
		if err != nil {
			return nil, err
		}
		out := []beads.Store{primary}
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			legacy, err := openCached(legacyOrderCityTarget(cityPath, cfg))
			if err != nil {
				return nil, err
			}
			out = append(out, legacy)
		}
		return out, nil
	}
}

func cachedOrderHistoryStoresResolver(cityPath string, cfg *config.City, stderr io.Writer) orderStoresResolver {
	stores := make(map[string]beads.Store)
	openCached := func(target execStoreTarget) (beads.Store, error) {
		key := orderStoreTargetKey(target)
		if store, ok := stores[key]; ok {
			return store, nil
		}
		store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
		if err != nil {
			return nil, err
		}
		stores[key] = store
		return store, nil
	}
	return func(a orders.Order) ([]beads.Store, error) {
		target, err := resolveOrderStoreTarget(cityPath, cfg, a)
		if err != nil {
			return nil, err
		}
		primary, err := openCached(target)
		if err != nil {
			return nil, err
		}
		out := []beads.Store{primary}
		if legacyOrderCityFallbackNeeded(cityPath, target) {
			legacy, err := openCached(legacyOrderCityTarget(cityPath, cfg))
			if err != nil {
				fmt.Fprintf(stderr, "gc order history: legacy city fallback unavailable for %s: %v\n", a.ScopedName(), err) //nolint:errcheck
				return out, nil
			}
			out = append(out, legacy)
		}
		return out, nil
	}
}

func legacyOrderCityFallbackNeeded(cityPath string, target execStoreTarget) bool {
	return target.ScopeKind == "rig" && filepath.Clean(target.ScopeRoot) != filepath.Clean(cityPath)
}

func legacyOrderCityTarget(cityPath string, cfg *config.City) execStoreTarget {
	prefix := ""
	if cfg != nil {
		prefix = config.EffectiveHQPrefix(cfg)
	}
	return execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city", Prefix: prefix}
}

func doOrderCheckWithStoresResolver(aa []orders.Order, now time.Time, ep events.Provider, resolveStores orderStoresResolver, stdout, stderr io.Writer) int {
	if len(aa) == 0 {
		fmt.Fprintln(stdout, "No orders found.") //nolint:errcheck // best-effort stdout
		return 1
	}

	hasRig := anyOrderHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", "NAME", "GATE", "RIG", "DUE", "REASON") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", "NAME", "GATE", "DUE", "REASON") //nolint:errcheck
	}
	anyDue := false
	for _, a := range aa {
		stores, err := resolveStores(a)
		if err != nil {
			fmt.Fprintf(stderr, "gc order check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		baseLastRunFn := orderLastRunFnAcrossStores(stores...)
		var lastRunErr error
		lastRunFn := func(orderName string) (time.Time, error) {
			last, err := baseLastRunFn(orderName)
			if err != nil {
				lastRunErr = err
			}
			return last, err
		}
		cursorFn := bdCursorFuncAcrossStores(stores...)
		if a.Gate == "event" {
			cursor, err := bdCursorAcrossStores(a.ScopedName(), stores...)
			if err != nil {
				fmt.Fprintf(stderr, "gc order check: reading event cursor for %s: %v\n", a.ScopedName(), err) //nolint:errcheck // best-effort stderr
				return 1
			}
			cursorFn = func(string) uint64 {
				return cursor
			}
		}
		result := orders.CheckGate(a, now, lastRunFn, ep, cursorFn)
		if lastRunErr != nil {
			fmt.Fprintf(stderr, "gc order check: reading last run for %s: %v\n", a.ScopedName(), lastRunErr) //nolint:errcheck // best-effort stderr
			return 1
		}
		due := "no"
		if result.Due {
			due = "yes"
			anyDue = true
		}
		if hasRig {
			rig := a.Rig
			if rig == "" {
				rig = "-"
			}
			fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", a.Name, a.Gate, rig, due, result.Reason) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", a.Name, a.Gate, due, result.Reason) //nolint:errcheck
		}
	}

	if anyDue {
		return 0
	}
	return 1
}

// --- gc order history ---

func cmdOrderHistory(name, rig string, stdout, stderr io.Writer) int {
	cityPath, cfg, aa, code := loadOrdersWithCity(stderr, "gc order history")
	if code != 0 {
		return code
	}
	return doOrderHistoryWithStoresResolver(name, rig, aa, cachedOrderHistoryStoresResolver(cityPath, cfg, stderr), stdout, stderr)
}

// doOrderHistory queries bead history for order runs and prints a table.
// When name is empty, shows history for all orders. When name is given,
// filters to that order only. When rig is non-empty, also filters by rig.
func doOrderHistory(name, rig string, aa []orders.Order, store beads.Store, stdout io.Writer) int {
	return doOrderHistoryWithStoreResolver(name, rig, aa, func(orders.Order) (beads.Store, error) {
		return store, nil
	}, stdout, io.Discard)
}

func doOrderHistoryWithStoreResolver(name, rig string, aa []orders.Order, resolveStore orderStoreResolver, stdout, stderr io.Writer) int {
	return doOrderHistoryWithStoresResolver(name, rig, aa, func(a orders.Order) ([]beads.Store, error) {
		store, err := resolveStore(a)
		if err != nil {
			return nil, err
		}
		return []beads.Store{store}, nil
	}, stdout, stderr)
}

func doOrderHistoryWithStoresResolver(name, rig string, aa []orders.Order, resolveStores orderStoresResolver, stdout, stderr io.Writer) int {
	// Filter orders if name or rig specified.
	targets := aa
	if name != "" || rig != "" {
		targets = nil
		for _, a := range aa {
			if name != "" && a.Name != name {
				continue
			}
			if rig != "" && a.Rig != rig {
				continue
			}
			targets = append(targets, a)
		}
	}

	type historyEntry struct {
		order     string
		rig       string
		id        string
		createdAt time.Time
	}
	var entries []historyEntry
	seenEntries := make(map[string]bool)

	for _, a := range targets {
		stores, err := resolveStores(a)
		if err != nil {
			fmt.Fprintf(stderr, "gc order history: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		label := "order-run:" + a.ScopedName()
		for i, store := range stores {
			if store == nil {
				continue
			}
			results, err := store.List(beads.ListQuery{
				Label:         label,
				IncludeClosed: true,
				Sort:          beads.SortCreatedDesc,
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc order history: %v\n", err) //nolint:errcheck // best-effort stderr
				if i == 0 {
					return 1
				}
				continue
			}
			for _, b := range results {
				key := a.ScopedName() + "\x00" + b.ID + "\x00" + b.CreatedAt.Format(time.RFC3339Nano) + "\x00" + b.Title
				if seenEntries[key] {
					continue
				}
				seenEntries[key] = true
				entries = append(entries, historyEntry{
					order:     a.Name,
					rig:       a.Rig,
					id:        b.ID,
					createdAt: b.CreatedAt,
				})
			}
		}
	}

	if len(entries) == 0 {
		if name != "" {
			fmt.Fprintf(stdout, "No order history for %q.\n", name) //nolint:errcheck
		} else {
			fmt.Fprintln(stdout, "No order history.") //nolint:errcheck
		}
		return 0
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].createdAt.After(entries[j].createdAt)
	})

	hasRig := false
	for _, e := range entries {
		if e.rig != "" {
			hasRig = true
			break
		}
	}

	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-15s %-15s %s\n", "ORDER", "RIG", "BEAD", "EXECUTED") //nolint:errcheck
		for _, e := range entries {
			rig := e.rig
			if rig == "" {
				rig = "-"
			}
			fmt.Fprintf(stdout, "%-20s %-15s %-15s %s\n", e.order, rig, e.id, e.createdAt.Format(time.RFC3339)) //nolint:errcheck
		}
	} else {
		fmt.Fprintf(stdout, "%-20s %-15s %s\n", "ORDER", "BEAD", "EXECUTED") //nolint:errcheck
		for _, e := range entries {
			fmt.Fprintf(stdout, "%-20s %-15s %s\n", e.order, e.id, e.createdAt.Format(time.RFC3339)) //nolint:errcheck
		}
	}
	return 0
}

// findOrder looks up an order by name and optional rig.
// When rig is empty, returns the first match by name (prefers city-level).
// When rig is non-empty, matches exact rig.
func findOrder(aa []orders.Order, name, rig string) (orders.Order, bool) {
	for _, a := range aa {
		if a.Name == name && (rig == "" || a.Rig == rig) {
			return a, true
		}
	}
	return orders.Order{}, false
}

// bdCursorFunc returns a CursorFunc that queries BdStore for the max seq
// label on wisps labeled order:<name>.
func bdCursorFunc(store beads.Store) orders.CursorFunc {
	return func(orderName string) uint64 {
		seq, err := bdCursor(store, orderName)
		if err != nil {
			return 0
		}
		return seq
	}
}

func bdCursor(store beads.Store, orderName string) (uint64, error) {
	beadList, err := store.List(beads.ListQuery{
		Label:         "order:" + orderName,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
	})
	if err != nil {
		return 0, fmt.Errorf("listing event cursor beads for order %q: %w", orderName, err)
	}
	labelSets := make([][]string, len(beadList))
	for i, b := range beadList {
		labelSets[i] = b.Labels
	}
	return orders.MaxSeqFromLabels(labelSets), nil
}

func bdCursorFuncAcrossStores(stores ...beads.Store) orders.CursorFunc {
	fns := make([]orders.CursorFunc, 0, len(stores))
	for _, store := range stores {
		if store != nil {
			fns = append(fns, bdCursorFunc(store))
		}
	}
	return func(orderName string) uint64 {
		var maxSeq uint64
		for _, fn := range fns {
			if seq := fn(orderName); seq > maxSeq {
				maxSeq = seq
			}
		}
		return maxSeq
	}
}

func bdCursorAcrossStores(orderName string, stores ...beads.Store) (uint64, error) {
	var maxSeq uint64
	for i, store := range stores {
		if store == nil {
			continue
		}
		seq, err := bdCursor(store, orderName)
		if err != nil {
			return 0, fmt.Errorf("store %d: %w", i, err)
		}
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq, nil
}
