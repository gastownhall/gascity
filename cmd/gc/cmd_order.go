package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
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

var orderLogf = log.Printf

func newOrderCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "order",
		Short: "Manage orders (scheduled and event-driven dispatch)",
		Long: `Manage orders — scheduled or event-driven dispatch of formulas and scripts.

Orders live in flat orders/<name>.toml files. Each order pairs a trigger
condition (cooldown, cron, condition, event, or manual) with an action
(a formula or an exec script). The controller evaluates triggers on each
tick and dispatches work when a trigger opens.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc order: missing subcommand (list, show, run, check, history, sweep-tracking)") //nolint:errcheck // best-effort stderr
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
		newOrderSweepTrackingCmd(stdout, stderr),
	)
	return cmd
}

func newOrderListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available orders",
		Long: `List all available orders with their trigger type, schedule, and target.

Scans orders/ directories for flat .toml files defining trigger conditions,
scheduling parameters, and target pools.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdOrderListWithOptions(stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return cmd
}

func newOrderShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of an order",
		Long: `Display detailed information about a named order.

Shows the order name, description, formula reference, trigger type,
scheduling parameters, check command, target, and source file.
Use --rig to disambiguate same-name orders in different rigs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdOrderShowWithOptions(args[0], rig, stdout, stderr, jsonOutput) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeOrderNames,
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to disambiguate same-name orders")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	_ = cmd.RegisterFlagCompletionFunc("rig", completeRigFlagNames)
	return cmd
}

func newOrderRunCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Execute an order manually",
		Long: `Execute an order manually, bypassing its trigger conditions.

Instantiates a wisp from the order's formula and routes it to the
configured target (if any). Useful for testing orders or triggering
them outside their normal schedule.
Use --rig to disambiguate same-name orders in different rigs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdOrderRun(args[0], rig, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeOrderNames,
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to disambiguate same-name orders")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	_ = cmd.RegisterFlagCompletionFunc("rig", completeRigFlagNames)
	return cmd
}

func newOrderCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check which orders are due to run",
		Long: `Evaluate trigger conditions for all orders and show which are due.

Prints a table with each order's trigger, due status, and reason. Returns
exit code 0 if any order is due, 1 if none are due.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdOrderCheck(jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	return cmd
}

func newOrderHistoryCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	var jsonOutput bool
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
			if cmdOrderHistoryJSON(name, rig, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeOrderNames,
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to filter order history")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSONL summary")
	_ = cmd.RegisterFlagCompletionFunc("rig", completeRigFlagNames)
	return cmd
}

func newOrderSweepTrackingCmd(stdout, stderr io.Writer) *cobra.Command {
	staleAfter := defaultOrderTrackingSweepStaleAfter
	quiet := false
	cmd := &cobra.Command{
		Use:   "sweep-tracking",
		Short: "Close stale order-tracking beads",
		Long: `Close stale open order-tracking beads.

This is intended for maintenance exec orders. It only closes tracking beads
older than --stale-after so a fresh in-flight order is not interrupted.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdOrderSweepTracking(staleAfter, quiet, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&staleAfter, "stale-after", defaultOrderTrackingSweepStaleAfter, "minimum age for an open tracking bead to be closed")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress success output")
	return cmd
}

// loadOrders is the common preamble for active order commands: resolve city,
// load config, scan formula layers, apply overrides, and filter disabled orders.
func loadOrders(stderr io.Writer, cmdName string) ([]orders.Order, int) {
	return loadActiveOrders(stderr, cmdName)
}

func loadActiveOrders(stderr io.Writer, cmdName string) ([]orders.Order, int) {
	_, _, aa, code := loadActiveOrdersWithCity(stderr, cmdName)
	return aa, code
}

func loadOrdersWithCity(stderr io.Writer, cmdName string) (string, *config.City, []orders.Order, int) {
	return loadActiveOrdersWithCity(stderr, cmdName)
}

func loadActiveOrdersWithCity(stderr io.Writer, cmdName string) (string, *config.City, []orders.Order, int) {
	cityPath, cfg, allAA, code := loadAllOrdersWithCity(stderr, cmdName)
	if code != 0 {
		return cityPath, cfg, allAA, code
	}
	return cityPath, cfg, orders.FilterEnabled(allAA), 0
}

func loadAllOrdersWithCity(stderr io.Writer, cmdName string) (string, *config.City, []orders.Order, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return "", nil, nil, 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return "", nil, nil, 1
	}
	aa, code := loadAllOrders(cityPath, cfg, stderr, cmdName)
	return cityPath, cfg, aa, code
}

// loadAllOrders scans city layers + per-rig exclusive layers for all orders
// and applies configured overrides. Callers that execute or list active work
// should use loadActiveOrders instead. Rig orders get their Rig field stamped.
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

func loadActiveOrdersForCity(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) ([]orders.Order, int) {
	allAA, code := loadAllOrders(cityPath, cfg, stderr, cmdName)
	if code != 0 {
		return allAA, code
	}
	return orders.FilterEnabled(allAA), 0
}

func scanAllOrders(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) ([]orders.Order, error) {
	return scanAllOrdersWithOptions(cityPath, cfg, stderr, cmdName, orders.ScanOptions{})
}

func scanAllOrdersWithOptions(cityPath string, cfg *config.City, stderr io.Writer, cmdName string, opts orders.ScanOptions) ([]orders.Order, error) {
	return scanAllOrdersFSWithOptions(fsys.OSFS{}, cityPath, cfg, stderr, cmdName, opts)
}

func scanAllOrdersFSWithOptions(fs fsys.FS, cityPath string, cfg *config.City, stderr io.Writer, cmdName string, opts orders.ScanOptions) ([]orders.Order, error) {
	if cfg == nil {
		cfg = &config.City{}
	}
	// City-level orders.
	cRoots := cityOrderRoots(cityPath, cfg)
	cLayers := cityFormulaLayers(cityPath, cfg)
	cityAA, err := orders.ScanRootsWithOptions(fs, cRoots, cfg.Orders.Skip, opts)
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
		ra, err := orders.ScanRootsWithOptions(fs, rigOrderRoots(cityPath, cfg, exclusive), cfg.Orders.Skip, opts)
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

func cmdOrderListWithOptions(stdout, stderr io.Writer, jsonOutput bool) int {
	if jsonOutput {
		cityPath, cfg, aa, code := loadOrdersWithCity(stderr, "gc order list")
		if code != 0 {
			return code
		}
		return doOrderListJSON(cityPath, cfg, aa, stdout)
	}
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
		fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %-15s %s\n", "NAME", "TYPE", "TRIGGER", "INTERVAL/SCHED", "RIG", "TARGET") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %s\n", "NAME", "TYPE", "TRIGGER", "INTERVAL/SCHED", "TARGET") //nolint:errcheck
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
			fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %-15s %s\n", a.Name, typ, a.Trigger, timing, rig, pool) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %s\n", a.Name, typ, a.Trigger, timing, pool) //nolint:errcheck
		}
	}
	return 0
}

type orderListJSON struct {
	SchemaVersion string                `json:"schema_version"`
	CityPath      string                `json:"city_path,omitempty"`
	CityName      string                `json:"city_name,omitempty"`
	Orders        []orderJSON           `json:"orders"`
	Summary       orderListSummaryJSON  `json:"summary"`
	Warnings      []jsonContractWarning `json:"warnings,omitempty"`
}

type orderListSummaryJSON struct {
	Count int `json:"count"`
}

type orderShowJSON struct {
	SchemaVersion string                `json:"schema_version"`
	CityPath      string                `json:"city_path,omitempty"`
	CityName      string                `json:"city_name,omitempty"`
	Order         orderJSON             `json:"order"`
	Warnings      []jsonContractWarning `json:"warnings,omitempty"`
}

type orderJSON struct {
	Name         string `json:"name"`
	ScopedName   string `json:"scoped_name"`
	Rig          string `json:"rig,omitempty"`
	Description  string `json:"description,omitempty"`
	Type         string `json:"type"`
	Formula      string `json:"formula,omitempty"`
	Exec         string `json:"exec,omitempty"`
	Trigger      string `json:"trigger"`
	Interval     string `json:"interval,omitempty"`
	Schedule     string `json:"schedule,omitempty"`
	Check        string `json:"check,omitempty"`
	On           string `json:"on,omitempty"`
	Target       string `json:"target,omitempty"`
	Timeout      string `json:"timeout,omitempty"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source,omitempty"`
	FormulaLayer string `json:"formula_layer,omitempty"`
}

func doOrderListJSON(cityPath string, cfg *config.City, aa []orders.Order, stdout io.Writer) int {
	rows := make([]orderJSON, 0, len(aa))
	for _, a := range aa {
		rows = append(rows, orderToJSON(a))
	}
	payload := orderListJSON{
		SchemaVersion: "1",
		CityPath:      cityPath,
		CityName:      effectiveJSONCityName(cfg, cityPath),
		Orders:        rows,
		Summary:       orderListSummaryJSON{Count: len(rows)},
	}
	if err := writeCLIJSONLine(stdout, payload); err != nil {
		return 1
	}
	return 0
}

func doOrderShowJSON(cityPath string, cfg *config.City, aa []orders.Order, name, rig string, stdout, stderr io.Writer) int {
	a, ok := findOrder(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc order show: order %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}
	payload := orderShowJSON{
		SchemaVersion: "1",
		CityPath:      cityPath,
		CityName:      effectiveJSONCityName(cfg, cityPath),
		Order:         orderToJSON(a),
	}
	if err := writeCLIJSONLine(stdout, payload); err != nil {
		return 1
	}
	return 0
}

func effectiveJSONCityName(cfg *config.City, cityPath string) string {
	if cfg == nil {
		return ""
	}
	return config.EffectiveCityName(cfg, filepath.Base(cityPath))
}

func orderToJSON(a orders.Order) orderJSON {
	typ := "formula"
	if a.IsExec() {
		typ = "exec"
	}
	return orderJSON{
		Name:         a.Name,
		ScopedName:   a.ScopedName(),
		Rig:          a.Rig,
		Description:  a.Description,
		Type:         typ,
		Formula:      a.Formula,
		Exec:         a.Exec,
		Trigger:      a.Trigger,
		Interval:     a.Interval,
		Schedule:     a.Schedule,
		Check:        a.Check,
		On:           a.On,
		Target:       a.Pool,
		Timeout:      a.Timeout,
		Enabled:      a.IsEnabled(),
		Source:       a.Source,
		FormulaLayer: a.FormulaLayer,
	}
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
	return cmdOrderShowWithOptions(name, rig, stdout, stderr, false)
}

func cmdOrderShowWithOptions(name, rig string, stdout, stderr io.Writer, jsonOutput bool) int {
	cityPath, cfg, aa, code := loadAllOrdersWithCity(stderr, "gc order show")
	if code != 0 {
		return code
	}
	if jsonOutput {
		return doOrderShowJSON(cityPath, cfg, aa, name, rig, stdout, stderr)
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
	w(fmt.Sprintf("Trigger:     %s", a.Trigger))
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

func cmdOrderRun(name, rig string, jsonOutput bool, stdout, stderr io.Writer) int {
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
		if jsonOutput {
			fmt.Fprintln(stderr, "gc order run: --json is not supported for exec orders because the exec body may write arbitrary stdout") //nolint:errcheck // best-effort stderr
			return 1
		}
		if a.Trigger == "event" {
			store, storeCode := openOrderStoreForOrder(cityPath, cfg, a, stderr, "gc order run")
			if store == nil {
				return storeCode
			}
			ep, epCode := openCityEventsProvider(stderr, "gc order run")
			if ep == nil {
				return epCode
			}
			defer ep.Close() //nolint:errcheck // best-effort
			return doOrderRunExecTracked(a, cityPath, cfg, store, ep, stdout, stderr)
		}
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
	return doOrderRunWithJSON(aa, name, rig, cityPath, store, ep, jsonOutput, stdout, stderr)
}

// doOrderRun executes an order manually: instantiates a wisp from the
// order's formula (or runs exec script directly) and routes it to the
// configured target.
func doOrderRun(aa []orders.Order, name, rig, cityPath string, store beads.Store, ep events.Provider, stdout, stderr io.Writer) int {
	return doOrderRunWithJSON(aa, name, rig, cityPath, store, ep, false, stdout, stderr)
}

type orderRunJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Order         string `json:"order"`
	Rig           string `json:"rig,omitempty"`
	ScopedName    string `json:"scoped_name"`
	Action        string `json:"action"`
	WispID        string `json:"wisp_id"`
	RoutedTo      string `json:"routed_to,omitempty"`
	EventCursor   uint64 `json:"event_cursor,omitempty"`
}

func doOrderRunWithJSON(aa []orders.Order, name, rig, cityPath string, store beads.Store, ep events.Provider, jsonOutput bool, stdout, stderr io.Writer) int {
	a, ok := findOrder(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc order run: order %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Exec orders: run the script directly.
	if a.IsExec() {
		if jsonOutput {
			fmt.Fprintln(stderr, "gc order run: --json is not supported for exec orders because the exec body may write arbitrary stdout") //nolint:errcheck // best-effort stderr
			return 1
		}
		cfg, cfgErr := loadCityConfig(cityPath, stderr)
		if cfgErr != nil {
			fmt.Fprintf(stderr, "gc order run: %v\n", cfgErr) //nolint:errcheck // best-effort stderr
			return 1
		}
		return doOrderRunExecTracked(a, cityPath, cfg, store, ep, stdout, stderr)
	}

	// Capture event head before wisp creation (race-free cursor). Event runs
	// fail closed when the cursor cannot be read.
	var headSeq uint64
	if a.Trigger == "event" && ep != nil {
		var err error
		headSeq, err = ep.LatestSeq()
		if err != nil {
			fmt.Fprintf(stderr, "gc order run: reading event cursor for %s: %v\n", a.ScopedName(), err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	scoped := a.ScopedName()
	var cfg *config.City
	var cityName string
	if citylayout.HasCityConfig(cityPath) || citylayout.HasRuntimeRoot(cityPath) {
		var err error
		cfg, err = loadCityConfig(cityPath, stderr)
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
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), a.Formula, searchPaths, nil)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, molecule.Options{}); err != nil {
		fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	var pool string
	if a.Pool != "" {
		pool, err = qualifyOrderPool(a, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "gc order run: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	if a.Pool != "" && cfg != nil {
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
	if a.Trigger == "event" && ep != nil {
		update.Labels = append(update.Labels,
			"order:"+scoped,
			fmt.Sprintf("seq:%d", headSeq),
		)
	}
	if a.Pool != "" {
		update.Metadata = map[string]string{"gc.routed_to": pool}
	}
	if err := store.Update(rootID, update); err != nil {
		fmt.Fprintf(stderr, "gc order run: labeling wisp: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOutput {
		_ = writeCLIJSONLine(stdout, orderRunJSON{
			SchemaVersion: "1",
			OK:            true,
			Order:         a.Name,
			Rig:           a.Rig,
			ScopedName:    scoped,
			Action:        "formula",
			WispID:        rootID,
			RoutedTo:      pool,
			EventCursor:   headSeq,
		}) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "Order %q executed: wisp %s", name, rootID) //nolint:errcheck
	if a.Pool != "" {
		fmt.Fprintf(stdout, " → gc.routed_to=%s", pool) //nolint:errcheck
	}
	fmt.Fprintln(stdout) //nolint:errcheck
	return 0
}

func doOrderRunExecTracked(a orders.Order, cityPath string, cfg *config.City, store beads.Store, ep events.Provider, stdout, stderr io.Writer) int {
	if a.Trigger != "event" || ep == nil {
		return doOrderRunExec(a, cityPath, cfg, stdout, stderr)
	}

	scoped := a.ScopedName()
	headSeq, err := ep.LatestSeq()
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: reading event cursor for %s: %v\n", scoped, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	tracking, err := store.Create(beads.Bead{
		Title:     "order:" + scoped,
		Labels:    []string{"order-run:" + scoped, labelOrderTracking},
		Ephemeral: true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: creating exec tracking bead for %s: %v\n", scoped, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer store.Close(tracking.ID) //nolint:errcheck // best-effort close

	// Persist the event cursor before running the command so manual event execs
	// do not leave the controller cursor stale after the side effect.
	if err := store.Update(tracking.ID, beads.UpdateOpts{Labels: eventCursorLabels(scoped, headSeq)}); err != nil {
		fmt.Fprintf(stderr, "gc order run: labeling exec event cursor for %s: %v\n", scoped, err) //nolint:errcheck // best-effort stderr
		return 1
	}

	result := doOrderRunExecResult(a, cityPath, cfg, stdout, stderr)
	labels := []string{"exec"}
	if result.code != 0 {
		failureLabel := result.failureLabel
		if failureLabel == "" {
			failureLabel = "exec-failed"
		}
		labels = []string{failureLabel}
	}
	if err := store.Update(tracking.ID, beads.UpdateOpts{Labels: labels}); err != nil {
		fmt.Fprintf(stderr, "gc order run: labeling exec tracking bead for %s: %v\n", scoped, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return result.code
}

// doOrderRunExec runs an exec order directly via shell.
func doOrderRunExec(a orders.Order, cityPath string, cfg *config.City, stdout, stderr io.Writer) int {
	return doOrderRunExecResult(a, cityPath, cfg, stdout, stderr).code
}

type orderRunExecResult struct {
	code         int
	failureLabel string
}

func doOrderRunExecResult(a orders.Order, cityPath string, cfg *config.City, stdout, stderr io.Writer) orderRunExecResult {
	var maxTimeout time.Duration
	if cfg != nil {
		maxTimeout = cfg.Orders.MaxTimeoutDuration()
	}
	timeout := effectiveTimeout(a, maxTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	target, err := resolveOrderExecTarget(cityPath, cfg, a)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: %s\n", redactOrderEnvError(err, os.Environ())) //nolint:errcheck // best-effort stderr
		return orderRunExecResult{code: 1, failureLabel: "exec-failed"}
	}
	env, err := orderExecEnvWithError(cityPath, cfg, target, a)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: %s\n", redactOrderEnvError(err, os.Environ())) //nolint:errcheck // best-effort stderr
		return orderRunExecResult{code: 1, failureLabel: "exec-env-failed"}
	}

	output, err := shellExecRunner(ctx, a.Exec, target.ScopeRoot, env)
	if err != nil {
		fmt.Fprintf(stderr, "gc order run: exec failed: %v\n", err) //nolint:errcheck
		if len(output) > 0 {
			fmt.Fprintf(stderr, "%s", output) //nolint:errcheck
		}
		return orderRunExecResult{code: 1, failureLabel: "exec-failed"}
	}
	if len(output) > 0 {
		fmt.Fprintf(stdout, "%s", output) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Order %q executed (exec)\n", a.Name) //nolint:errcheck
	return orderRunExecResult{code: 0}
}

// --- gc order check ---

func cmdOrderCheck(jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, cfg, aa, code := loadOrdersWithCity(stderr, "gc order check")
	if code != 0 {
		return code
	}

	ep, epCode := openCityEventsProvider(stderr, "gc order check")
	if ep == nil {
		return epCode
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doOrderCheckWithStoresResolverScopedJSON(cityPath, cfg, aa, time.Now(), ep, cachedOrderStoresResolver(cityPath, cfg), jsonOutput, stdout, stderr)
}

// orderLastRunFn returns a LastRunFunc that queries BdStore for the most
// recent bead labeled order-run:<name>. Returns zero time if never run.
func orderLastRunFn(store beads.Store) orders.LastRunFunc {
	return orders.LastRunFuncForStore(store)
}

// doOrderCheck evaluates triggers for all orders and prints a table.
// Returns 0 if any are due, 1 if none are due.
func doOrderCheck(aa []orders.Order, now time.Time, lastRunFn orders.LastRunFunc, stdout io.Writer) int {
	return doOrderCheckJSON(aa, now, lastRunFn, false, stdout)
}

type orderCheckJSON struct {
	SchemaVersion string              `json:"schema_version"`
	OK            bool                `json:"ok"`
	AnyDue        bool                `json:"any_due"`
	OrdersTotal   int                 `json:"orders_total"`
	DueTotal      int                 `json:"due_total"`
	Orders        []orderCheckJSONRow `json:"orders"`
}

type orderCheckJSONRow struct {
	Name       string `json:"name"`
	Rig        string `json:"rig,omitempty"`
	ScopedName string `json:"scoped_name"`
	Trigger    string `json:"trigger"`
	Due        bool   `json:"due"`
	Reason     string `json:"reason"`
}

func doOrderCheckJSON(aa []orders.Order, now time.Time, lastRunFn orders.LastRunFunc, jsonOutput bool, stdout io.Writer) int {
	if len(aa) == 0 {
		if jsonOutput {
			_ = writeCLIJSONLine(stdout, orderCheckJSON{
				SchemaVersion: "1",
				OK:            true,
				AnyDue:        false,
				OrdersTotal:   0,
				DueTotal:      0,
				Orders:        []orderCheckJSONRow{},
			}) //nolint:errcheck // best-effort stdout
			return 1
		}
		fmt.Fprintln(stdout, "No orders found.") //nolint:errcheck // best-effort stdout
		return 1
	}

	if jsonOutput {
		result := orderCheckJSON{
			SchemaVersion: "1",
			OK:            true,
			OrdersTotal:   len(aa),
			Orders:        make([]orderCheckJSONRow, 0, len(aa)),
		}
		for _, a := range aa {
			check := orders.CheckTrigger(a, now, lastRunFn, nil, nil)
			if check.Due {
				result.AnyDue = true
				result.DueTotal++
			}
			result.Orders = append(result.Orders, orderCheckJSONRow{
				Name:       a.Name,
				Rig:        a.Rig,
				ScopedName: a.ScopedName(),
				Trigger:    a.Trigger,
				Due:        check.Due,
				Reason:     check.Reason,
			})
		}
		_ = writeCLIJSONLine(stdout, result) //nolint:errcheck // best-effort stdout
		if result.AnyDue {
			return 0
		}
		return 1
	}

	hasRig := anyOrderHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", "NAME", "TRIGGER", "RIG", "DUE", "REASON") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", "NAME", "TRIGGER", "DUE", "REASON") //nolint:errcheck
	}
	anyDue := false
	for _, a := range aa {
		result := orders.CheckTrigger(a, now, lastRunFn, nil, nil)
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
			fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", a.Name, a.Trigger, rig, due, result.Reason) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", a.Name, a.Trigger, due, result.Reason) //nolint:errcheck
		}
	}

	if anyDue {
		return 0
	}
	return 1
}

func doOrderCheckWithStoresResolver(aa []orders.Order, now time.Time, ep events.Provider, resolveStores orderStoresResolver, stdout, stderr io.Writer) int {
	return doOrderCheckWithStoresResolverScoped("", nil, aa, now, ep, resolveStores, stdout, stderr)
}

func doOrderCheckWithStoresResolverScoped(cityPath string, cfg *config.City, aa []orders.Order, now time.Time, ep events.Provider, resolveStores orderStoresResolver, stdout, stderr io.Writer) int {
	return doOrderCheckWithStoresResolverScopedJSON(cityPath, cfg, aa, now, ep, resolveStores, false, stdout, stderr)
}

func doOrderCheckWithStoresResolverScopedJSON(cityPath string, cfg *config.City, aa []orders.Order, now time.Time, ep events.Provider, resolveStores orderStoresResolver, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(aa) == 0 {
		if jsonOutput {
			_ = writeCLIJSONLine(stdout, orderCheckJSON{
				SchemaVersion: "1",
				OK:            true,
				AnyDue:        false,
				OrdersTotal:   0,
				DueTotal:      0,
				Orders:        []orderCheckJSONRow{},
			}) //nolint:errcheck // best-effort stdout
			return 1
		}
		fmt.Fprintln(stdout, "No orders found.") //nolint:errcheck // best-effort stdout
		return 1
	}

	if jsonOutput {
		result := orderCheckJSON{
			SchemaVersion: "1",
			OK:            true,
			OrdersTotal:   len(aa),
			Orders:        make([]orderCheckJSONRow, 0, len(aa)),
		}
		for _, a := range aa {
			stores, err := resolveStores(a)
			if err != nil {
				fmt.Fprintf(stderr, "gc order check: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			baseLastRunFn := orders.LastRunAcrossStores(stores...)
			var lastRunErr error
			lastRunFn := func(orderName string) (time.Time, error) {
				last, err := baseLastRunFn(orderName)
				if err != nil {
					lastRunErr = err
				}
				return last, err
			}
			cursorFn := orders.CursorAcrossStores(stores...)
			if a.Trigger == "event" {
				cursor, err := bdCursorAcrossStores(a.ScopedName(), stores...)
				if err != nil {
					fmt.Fprintf(stderr, "gc order check: reading event cursor for %s: %v\n", a.ScopedName(), err) //nolint:errcheck // best-effort stderr
					return 1
				}
				cursorFn = func(string) uint64 {
					return cursor
				}
			}
			triggerOpts, err := orderTriggerOptions(cityPath, cfg, a)
			if err != nil {
				fmt.Fprintf(stderr, "gc order check: %v\n", err) //nolint:errcheck // best-effort stderr
				return 1
			}
			check := orders.CheckTriggerWithOptions(a, now, lastRunFn, ep, cursorFn, triggerOpts)
			if lastRunErr != nil {
				fmt.Fprintf(stderr, "gc order check: reading last run for %s: %v\n", a.ScopedName(), lastRunErr) //nolint:errcheck // best-effort stderr
				return 1
			}
			if check.Due {
				result.AnyDue = true
				result.DueTotal++
			}
			result.Orders = append(result.Orders, orderCheckJSONRow{
				Name:       a.Name,
				Rig:        a.Rig,
				ScopedName: a.ScopedName(),
				Trigger:    a.Trigger,
				Due:        check.Due,
				Reason:     check.Reason,
			})
		}
		_ = writeCLIJSONLine(stdout, result) //nolint:errcheck // best-effort stdout
		if result.AnyDue {
			return 0
		}
		return 1
	}

	hasRig := anyOrderHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", "NAME", "TRIGGER", "RIG", "DUE", "REASON") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", "NAME", "TRIGGER", "DUE", "REASON") //nolint:errcheck
	}
	anyDue := false
	for _, a := range aa {
		stores, err := resolveStores(a)
		if err != nil {
			fmt.Fprintf(stderr, "gc order check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		baseLastRunFn := orders.LastRunAcrossStores(stores...)
		var lastRunErr error
		lastRunFn := func(orderName string) (time.Time, error) {
			last, err := baseLastRunFn(orderName)
			if err != nil {
				lastRunErr = err
			}
			return last, err
		}
		cursorFn := orders.CursorAcrossStores(stores...)
		if a.Trigger == "event" {
			cursor, err := bdCursorAcrossStores(a.ScopedName(), stores...)
			if err != nil {
				fmt.Fprintf(stderr, "gc order check: reading event cursor for %s: %v\n", a.ScopedName(), err) //nolint:errcheck // best-effort stderr
				return 1
			}
			cursorFn = func(string) uint64 {
				return cursor
			}
		}
		triggerOpts, err := orderTriggerOptions(cityPath, cfg, a)
		if err != nil {
			fmt.Fprintf(stderr, "gc order check: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		result := orders.CheckTriggerWithOptions(a, now, lastRunFn, ep, cursorFn, triggerOpts)
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
			fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", a.Name, a.Trigger, rig, due, result.Reason) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", a.Name, a.Trigger, due, result.Reason) //nolint:errcheck
		}
	}

	if anyDue {
		return 0
	}
	return 1
}

// --- gc order history ---

func cmdOrderHistory(name, rig string, stdout, stderr io.Writer) int {
	return cmdOrderHistoryJSON(name, rig, false, stdout, stderr)
}

func cmdOrderHistoryJSON(name, rig string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, cfg, aa, code := loadAllOrdersWithCity(stderr, "gc order history")
	if code != 0 {
		return code
	}
	return doOrderHistoryWithStoresResolverJSON(name, rig, aa, cachedOrderHistoryStoresResolver(cityPath, cfg, stderr), jsonOutput, stdout, stderr)
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
	return doOrderHistoryWithStoresResolverJSON(name, rig, aa, resolveStores, false, stdout, stderr)
}

func doOrderHistoryWithStoresResolverJSON(name, rig string, aa []orders.Order, resolveStores orderStoresResolver, jsonOutput bool, stdout, stderr io.Writer) int {
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
				TierMode:      beads.TierBoth,
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc order history: %v\n", err) //nolint:errcheck // best-effort stderr
				if i == 0 && len(results) == 0 {
					return 1
				}
				if len(results) == 0 {
					continue
				}
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
		if jsonOutput {
			_ = writeCLIJSONLine(stdout, orderHistoryJSONResult{
				SchemaVersion: "1",
				OK:            true,
				Name:          name,
				Rig:           rig,
				Entries:       []orderHistoryJSONEntry{},
				Summary:       orderHistoryJSONSummary{Total: 0},
			})
			return 0
		}
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

	if jsonOutput {
		payload := orderHistoryJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Name:          name,
			Rig:           rig,
			Entries:       make([]orderHistoryJSONEntry, 0, len(entries)),
			Summary:       orderHistoryJSONSummary{Total: len(entries)},
		}
		for _, e := range entries {
			payload.Entries = append(payload.Entries, orderHistoryJSONEntry{
				Order:     e.order,
				Rig:       e.rig,
				BeadID:    e.id,
				Executed:  e.createdAt.Format(time.RFC3339),
				CreatedAt: e.createdAt,
			})
		}
		_ = writeCLIJSONLine(stdout, payload)
		return 0
	}

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

type orderHistoryJSONResult struct {
	SchemaVersion string                  `json:"schema_version"`
	OK            bool                    `json:"ok"`
	Name          string                  `json:"name,omitempty"`
	Rig           string                  `json:"rig,omitempty"`
	Entries       []orderHistoryJSONEntry `json:"entries"`
	Summary       orderHistoryJSONSummary `json:"summary"`
}

type orderHistoryJSONEntry struct {
	Order     string    `json:"order"`
	Rig       string    `json:"rig,omitempty"`
	BeadID    string    `json:"bead_id"`
	Executed  string    `json:"executed"`
	CreatedAt time.Time `json:"created_at"`
}

type orderHistoryJSONSummary struct {
	Total int `json:"total"`
}

// --- gc order sweep-tracking ---

func cmdOrderSweepTracking(staleAfter time.Duration, quiet bool, stdout, stderr io.Writer) int {
	if staleAfter <= 0 {
		fmt.Fprintln(stderr, "gc order sweep-tracking: --stale-after must be positive") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc order sweep-tracking: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc order sweep-tracking: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	closed, err := sweepStaleOrderTracking(store, time.Now(), staleAfter, nil, orderTrackingSweepMetadataInitiator)
	if err != nil {
		fmt.Fprintf(stderr, "gc order sweep-tracking: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !quiet {
		fmt.Fprintf(stdout, "closed %d stale order-tracking bead(s)\n", closed) //nolint:errcheck // best-effort stdout
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

func bdCursor(store beads.Store, orderName string) (uint64, error) {
	beadList, err := store.List(beads.ListQuery{
		Label:         "order:" + orderName,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		if len(beadList) == 0 {
			return 0, fmt.Errorf("listing event cursor beads for order %q: %w", orderName, err)
		}
		orderLogf("gc order: event cursor lookup partially failed for %s: %v", orderName, err)
	}
	labelSets := make([][]string, len(beadList))
	for i, b := range beadList {
		labelSets[i] = b.Labels
	}
	return orders.MaxSeqFromLabels(labelSets), nil
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
