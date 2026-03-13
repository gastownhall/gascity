package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/automations"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

func newAutomationCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "automation",
		Short: "Manage automations (periodic formula dispatch)",
		Long: `Manage automations — formulas with gate conditions for periodic dispatch.

Automations are formulas annotated with scheduling gates (interval, cron
schedule, or shell check commands). The controller evaluates gates
periodically and dispatches automation formulas when they are due.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc automation: missing subcommand (list, show, run, check, history)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc automation: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newAutomationListCmd(stdout, stderr),
		newAutomationShowCmd(stdout, stderr),
		newAutomationRunCmd(stdout, stderr),
		newAutomationCheckCmd(stdout, stderr),
		newAutomationHistoryCmd(stdout, stderr),
	)
	return cmd
}

func newAutomationListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available automations",
		Long: `List all available automations with their gate type, schedule, and target pool.

Scans formula layers for formulas that have automation metadata
(gate, interval, schedule, check, pool).`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdAutomationList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newAutomationShowCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of an automation",
		Long: `Display detailed information about a named automation.

Shows the automation name, description, formula reference, gate type,
scheduling parameters, check command, target pool, and source file.
Use --rig to disambiguate same-name automations in different rigs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAutomationShow(args[0], rig, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to disambiguate same-name automations")
	return cmd
}

func newAutomationRunCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Execute an automation manually",
		Long: `Execute an automation manually, bypassing its gate conditions.

Instantiates a wisp from the automation's formula and routes it to the
target pool (if configured). Useful for testing automations or triggering
them outside their normal schedule.
Use --rig to disambiguate same-name automations in different rigs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAutomationRun(args[0], rig, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to disambiguate same-name automations")
	return cmd
}

func newAutomationCheckCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check which automations are due to run",
		Long: `Evaluate gate conditions for all automations and show which are due.

Prints a table with each automation's gate, due status, and reason. Returns
exit code 0 if any automation is due, 1 if none are due.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdAutomationCheck(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newAutomationHistoryCmd(stdout, stderr io.Writer) *cobra.Command {
	var rig string
	cmd := &cobra.Command{
		Use:   "history [name]",
		Short: "Show automation execution history",
		Long: `Show execution history for automations.

Queries bead history for past automation runs. Optionally filter by automation
name. Use --rig to filter by rig.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			if cmdAutomationHistory(name, rig, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&rig, "rig", "", "rig name to filter automation history")
	return cmd
}

// loadAutomations is the common preamble for automation commands: resolve city,
// load config, scan formula layers for all automations (city + rig).
func loadAutomations(stderr io.Writer, cmdName string) ([]automations.Automation, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return loadAllAutomations(cityPath, cfg, stderr, cmdName)
}

// loadAllAutomations scans city layers + per-rig exclusive layers for automations.
// Rig automations get their Rig field stamped.
func loadAllAutomations(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) ([]automations.Automation, int) {
	allAA, err := scanAllAutomations(cityPath, cfg, stderr, cmdName)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}

	// Apply automation overrides from city config.
	if len(cfg.Automations.Overrides) > 0 {
		if err := automations.ApplyOverrides(allAA, convertOverrides(cfg.Automations.Overrides)); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
			return nil, 1
		}
	}

	return allAA, 0
}

func scanAllAutomations(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) ([]automations.Automation, error) {
	// City-level automations.
	cRoots := cityAutomationRoots(cityPath, cfg)
	cLayers := cityFormulaLayers(cityPath, cfg)
	cityAA, err := automations.ScanRoots(fsys.OSFS{}, cRoots, cfg.Automations.Skip)
	if err != nil {
		return nil, err
	}

	// Per-rig automations from rig-exclusive layers.
	var rigAA []automations.Automation
	for rigName, layers := range cfg.FormulaLayers.Rigs {
		exclusive := rigExclusiveLayers(layers, cLayers)
		if len(exclusive) == 0 {
			continue
		}
		ra, err := automations.ScanRoots(fsys.OSFS{}, rigAutomationRoots(cityPath, cfg, exclusive), cfg.Automations.Skip)
		if err != nil {
			fmt.Fprintf(stderr, "%s: rig %s: %v\n", cmdName, rigName, err) //nolint:errcheck // best-effort stderr
			continue
		}
		for i := range ra {
			ra[i].Rig = rigName
		}
		rigAA = append(rigAA, ra...)
	}

	allAA := make([]automations.Automation, 0, len(cityAA)+len(rigAA))
	allAA = append(allAA, cityAA...)
	allAA = append(allAA, rigAA...)
	return allAA, nil
}

// cityFormulaLayers returns the formula directory layers for city-level automation
// scanning. Uses FormulaLayers.City if populated (from LoadWithIncludes),
// otherwise falls back to the single formulas dir.
func cityFormulaLayers(cityPath string, cfg *config.City) []string {
	if len(cfg.FormulaLayers.City) > 0 {
		return cfg.FormulaLayers.City
	}
	return []string{citylayout.ResolveCityFormulasDir(fsys.OSFS{}, cityPath, cfg.FormulasDir())}
}

func cityAutomationRoots(cityPath string, cfg *config.City) []automations.ScanRoot {
	formulaLayers := cityFormulaLayers(cityPath, cfg)
	localFormulas := citylayout.ResolveCityFormulasDir(fsys.OSFS{}, cityPath, cfg.FormulasDir())
	roots := make([]automations.ScanRoot, 0, len(formulaLayers)+2)
	seen := make(map[string]bool, len(formulaLayers)+2)
	appendRoot := func(root automations.ScanRoot) {
		dir := filepath.Clean(root.Dir)
		if seen[dir] {
			return
		}
		seen[dir] = true
		roots = append(roots, root)
	}
	for _, layer := range formulaLayers {
		formulaRoot := filepath.Join(layer, "automations")
		if layer == localFormulas {
			appendRoot(automations.ScanRoot{
				Dir:          formulaRoot,
				FormulaLayer: localFormulas,
			})
			for _, root := range citylayout.ResolveCityAutomationRoots(fsys.OSFS{}, cityPath) {
				appendRoot(automations.ScanRoot{
					Dir:          root,
					FormulaLayer: localFormulas,
				})
			}
			continue
		}
		appendRoot(automations.ScanRoot{
			Dir:          formulaRoot,
			FormulaLayer: layer,
		})
	}
	return roots
}

func rigAutomationRoots(_ string, _ *config.City, formulaLayers []string) []automations.ScanRoot {
	roots := make([]automations.ScanRoot, 0, len(formulaLayers))
	for _, layer := range formulaLayers {
		roots = append(roots, automations.ScanRoot{
			Dir:          filepath.Join(layer, "automations"),
			FormulaLayer: layer,
		})
	}
	return roots
}

// --- gc automation list ---

func cmdAutomationList(stdout, stderr io.Writer) int {
	aa, code := loadAutomations(stderr, "gc automation list")
	if code != 0 {
		return code
	}
	return doAutomationList(aa, stdout)
}

// doAutomationList prints a table of automations. Accepts pre-scanned automations for testability.
func doAutomationList(aa []automations.Automation, stdout io.Writer) int {
	if len(aa) == 0 {
		fmt.Fprintln(stdout, "No automations found.") //nolint:errcheck // best-effort stdout
		return 0
	}

	hasRig := anyAutomationHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %-15s %s\n", "NAME", "TYPE", "GATE", "INTERVAL/SCHED", "RIG", "POOL") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-8s %-12s %-15s %s\n", "NAME", "TYPE", "GATE", "INTERVAL/SCHED", "POOL") //nolint:errcheck
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

// anyAutomationHasRig returns true if any automation in the list has a non-empty Rig.
func anyAutomationHasRig(aa []automations.Automation) bool {
	for _, a := range aa {
		if a.Rig != "" {
			return true
		}
	}
	return false
}

// --- gc automation show ---

func cmdAutomationShow(name, rig string, stdout, stderr io.Writer) int {
	aa, code := loadAutomations(stderr, "gc automation show")
	if code != 0 {
		return code
	}
	return doAutomationShow(aa, name, rig, stdout, stderr)
}

// doAutomationShow prints details of a named automation.
func doAutomationShow(aa []automations.Automation, name, rig string, stdout, stderr io.Writer) int {
	a, ok := findAutomation(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc automation show: automation %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w(fmt.Sprintf("Automation:  %s", a.Name))
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
		w(fmt.Sprintf("Pool:        %s", a.Pool))
	}
	w(fmt.Sprintf("Source:      %s", a.Source))
	return 0
}

// --- gc automation run ---

func cmdAutomationRun(name, rig string, stdout, stderr io.Writer) int {
	aa, code := loadAutomations(stderr, "gc automation run")
	if code != 0 {
		return code
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc automation run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store := beads.NewBdStore(cityPath, beads.ExecCommandRunner())

	ep, epCode := openCityEventsProvider(stderr, "gc automation run")
	if ep == nil {
		return epCode
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doAutomationRun(aa, name, rig, shellSlingRunner, store, ep, stdout, stderr)
}

// doAutomationRun executes an automation manually: instantiates a wisp from the
// automation's formula (or runs exec script directly) and routes it to the
// target pool.
func doAutomationRun(aa []automations.Automation, name, rig string, runner SlingRunner, store beads.Store, ep events.Provider, stdout, stderr io.Writer) int {
	a, ok := findAutomation(aa, name, rig)
	if !ok {
		fmt.Fprintf(stderr, "gc automation run: automation %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Exec automations: run the script directly.
	if a.IsExec() {
		return doAutomationRunExec(a, stdout, stderr)
	}

	// Capture event head before wisp creation (race-free cursor).
	var headSeq uint64
	if a.Gate == "event" && ep != nil {
		headSeq, _ = ep.LatestSeq()
	}

	// Instantiate wisp from formula.
	rootID, err := store.MolCook(a.Formula, "", nil)
	if err != nil {
		fmt.Fprintf(stderr, "gc automation run: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Label with automation-run:<scopedName> for tracking, plus pool routing if specified.
	// For event gates, also add automation:<scopedName> and seq:<headSeq> for cursor tracking.
	scoped := a.ScopedName()
	routeCmd := fmt.Sprintf("bd update %s --add-label=automation-run:%s", rootID, scoped)
	if a.Gate == "event" && ep != nil {
		routeCmd += fmt.Sprintf(" --add-label=automation:%s --add-label=seq:%d", scoped, headSeq)
	}
	if a.Pool != "" {
		pool := qualifyPool(a.Pool, a.Rig)
		routeCmd += fmt.Sprintf(" --add-label=pool:%s", pool)
	}
	if _, err := runner("", routeCmd, nil); err != nil {
		fmt.Fprintf(stderr, "gc automation run: labeling wisp: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Automation %q executed: wisp %s", name, rootID) //nolint:errcheck
	if a.Pool != "" {
		fmt.Fprintf(stdout, " → pool:%s", a.Pool) //nolint:errcheck
	}
	fmt.Fprintln(stdout) //nolint:errcheck
	return 0
}

// doAutomationRunExec runs an exec automation directly via shell.
func doAutomationRunExec(a automations.Automation, stdout, stderr io.Writer) int {
	timeout := a.TimeoutOrDefault()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var env []string
	if a.Source != "" {
		env = append(env, "AUTOMATION_DIR="+filepath.Dir(a.Source))
	}

	output, err := shellExecRunner(ctx, a.Exec, ".", env)
	if err != nil {
		fmt.Fprintf(stderr, "gc automation run: exec failed: %v\n", err) //nolint:errcheck
		if len(output) > 0 {
			fmt.Fprintf(stderr, "%s", output) //nolint:errcheck
		}
		return 1
	}
	if len(output) > 0 {
		fmt.Fprintf(stdout, "%s", output) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Automation %q executed (exec)\n", a.Name) //nolint:errcheck
	return 0
}

// --- gc automation check ---

func cmdAutomationCheck(stdout, stderr io.Writer) int {
	aa, code := loadAutomations(stderr, "gc automation check")
	if code != 0 {
		return code
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc automation check: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store := beads.NewBdStore(cityPath, beads.ExecCommandRunner())
	lastRunFn := automationLastRunFn(store)
	cursorFn := bdCursorFunc(store)

	ep, epCode := openCityEventsProvider(stderr, "gc automation check")
	if ep == nil {
		return epCode
	}
	defer ep.Close() //nolint:errcheck // best-effort
	return doAutomationCheck(aa, time.Now(), lastRunFn, ep, cursorFn, stdout)
}

// automationLastRunFn returns a LastRunFunc that queries BdStore for the most
// recent bead labeled automation-run:<name>. Returns zero time if never run.
func automationLastRunFn(store beads.Store) automations.LastRunFunc {
	return func(name string) (time.Time, error) {
		label := "automation-run:" + name
		results, err := store.ListByLabel(label, 1)
		if err != nil {
			return time.Time{}, err
		}
		if len(results) == 0 {
			return time.Time{}, nil
		}
		return results[0].CreatedAt, nil
	}
}

// doAutomationCheck evaluates gates for all automations and prints a table.
// Returns 0 if any are due, 1 if none are due.
func doAutomationCheck(aa []automations.Automation, now time.Time, lastRunFn automations.LastRunFunc, ep events.Provider, cursorFn automations.CursorFunc, stdout io.Writer) int {
	if len(aa) == 0 {
		fmt.Fprintln(stdout, "No automations found.") //nolint:errcheck // best-effort stdout
		return 1
	}

	hasRig := anyAutomationHasRig(aa)
	if hasRig {
		fmt.Fprintf(stdout, "%-20s %-12s %-15s %-5s %s\n", "NAME", "GATE", "RIG", "DUE", "REASON") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "%-20s %-12s %-5s %s\n", "NAME", "GATE", "DUE", "REASON") //nolint:errcheck
	}
	anyDue := false
	for _, a := range aa {
		result := automations.CheckGate(a, now, lastRunFn, ep, cursorFn)
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

// --- gc automation history ---

func cmdAutomationHistory(name, rig string, stdout, stderr io.Writer) int {
	aa, code := loadAutomations(stderr, "gc automation history")
	if code != 0 {
		return code
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc automation history: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	store := beads.NewBdStore(cityPath, beads.ExecCommandRunner())
	return doAutomationHistory(name, rig, aa, store, stdout)
}

// doAutomationHistory queries bead history for automation runs and prints a table.
// When name is empty, shows history for all automations. When name is given,
// filters to that automation only. When rig is non-empty, also filters by rig.
func doAutomationHistory(name, rig string, aa []automations.Automation, store beads.Store, stdout io.Writer) int {
	// Filter automations if name or rig specified.
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
		automation string
		rig        string
		id         string
		time       string
	}
	var entries []historyEntry

	for _, a := range targets {
		label := "automation-run:" + a.ScopedName()
		results, err := store.ListByLabel(label, 0)
		if err != nil {
			continue
		}
		for _, b := range results {
			entries = append(entries, historyEntry{
				automation: a.Name,
				rig:        a.Rig,
				id:         b.ID,
				time:       b.CreatedAt.Format(time.RFC3339),
			})
		}
	}

	if len(entries) == 0 {
		if name != "" {
			fmt.Fprintf(stdout, "No automation history for %q.\n", name) //nolint:errcheck
		} else {
			fmt.Fprintln(stdout, "No automation history.") //nolint:errcheck
		}
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
		fmt.Fprintf(stdout, "%-20s %-15s %-15s %s\n", "AUTOMATION", "RIG", "BEAD", "EXECUTED") //nolint:errcheck
		for _, e := range entries {
			rig := e.rig
			if rig == "" {
				rig = "-"
			}
			fmt.Fprintf(stdout, "%-20s %-15s %-15s %s\n", e.automation, rig, e.id, e.time) //nolint:errcheck
		}
	} else {
		fmt.Fprintf(stdout, "%-20s %-15s %s\n", "AUTOMATION", "BEAD", "EXECUTED") //nolint:errcheck
		for _, e := range entries {
			fmt.Fprintf(stdout, "%-20s %-15s %s\n", e.automation, e.id, e.time) //nolint:errcheck
		}
	}
	return 0
}

// findAutomation looks up an automation by name and optional rig.
// When rig is empty, returns the first match by name (prefers city-level).
// When rig is non-empty, matches exact rig.
func findAutomation(aa []automations.Automation, name, rig string) (automations.Automation, bool) {
	for _, a := range aa {
		if a.Name == name && (rig == "" || a.Rig == rig) {
			return a, true
		}
	}
	return automations.Automation{}, false
}

// bdCursorFunc returns a CursorFunc that queries BdStore for the max seq
// label on wisps labeled automation:<name>.
func bdCursorFunc(store beads.Store) automations.CursorFunc {
	return func(automationName string) uint64 {
		beadList, err := store.ListByLabel("automation:"+automationName, 0)
		if err != nil {
			return 0
		}
		labelSets := make([][]string, len(beadList))
		for i, b := range beadList {
			labelSets[i] = b.Labels
		}
		return automations.MaxSeqFromLabels(labelSets)
	}
}
