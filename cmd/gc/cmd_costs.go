package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/gastownhall/gascity/internal/usage"
	"github.com/spf13/cobra"
)

func newCostsCmd(stdout, stderr io.Writer) *cobra.Command {
	var byFormula bool
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Show per-run usage and estimated cost for this city",
		Long: `Aggregate recorded usage facts (model tokens and compute wall-seconds)
by run for local cost insight.

Reads .gc/usage.jsonl (the local usage sink) and groups facts by run id. This
reflects facts only under the default "local" usage provider; with an "exec:"
or "discard" provider the facts are forwarded out of process or dropped, so
gc costs shows nothing local.

Cost is a list-price estimate for decision support, not an authoritative
charge; invocations with no pricing are flagged "unpriced" and excluded from
the cost total.

With --by-formula, model usage is grouped by the formula the acting bead's run
was compiled from instead of by run; usage with no formula appears as "-".
Compute wall-seconds are not attributed to a formula, so that table has no
WALL_S column.`,
		Example: "  gc costs\n  gc costs --by-formula",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doCosts(stdout, stderr, byFormula) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&byFormula, "by-formula", false, "group model usage by the formula each fact is attributed to instead of by run")
	return cmd
}

type runCost struct {
	RunID               string  `json:"run_id"`
	Invocations         int     `json:"invocations"`
	ComputeFacts        int     `json:"compute_facts"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	WallSeconds         float64 `json:"wall_seconds"`
	CostUSDEstimate     float64 `json:"cost_usd_estimate"`
	Unpriced            int     `json:"unpriced_invocations"`
}

// aggregateRunCosts groups usage facts by run id. Exposed (unexported) for tests.
func aggregateRunCosts(facts []usage.Fact) []runCost {
	byRun := map[string]*runCost{}
	var order []string
	for _, f := range facts {
		rc := byRun[f.RunID]
		if rc == nil {
			rc = &runCost{RunID: f.RunID}
			byRun[f.RunID] = rc
			order = append(order, f.RunID)
		}
		switch f.Kind {
		case usage.KindModel:
			addModelFact(&rc.Invocations, &rc.InputTokens, &rc.OutputTokens, &rc.CacheReadTokens, &rc.CacheCreationTokens, &rc.CostUSDEstimate, &rc.Unpriced, f)
		case usage.KindCompute:
			rc.ComputeFacts++
			rc.WallSeconds += f.WallSeconds
		}
	}
	sort.Strings(order)
	rows := make([]runCost, 0, len(order))
	for _, id := range order {
		rows = append(rows, *byRun[id])
	}
	return rows
}

// addModelFact folds one model fact into a row's counters. Both tables use it,
// so the per-formula buckets conserve the default table's model columns.
func addModelFact(invocations, in, out, cacheRead, cacheCreate *int, cost *float64, unpriced *int, f usage.Fact) {
	*invocations++
	*in += f.InputTokens
	*out += f.OutputTokens
	*cacheRead += f.CacheReadTokens
	*cacheCreate += f.CacheCreationTokens
	if f.Unpriced {
		*unpriced++
	} else {
		*cost += f.CostUSDEstimate
	}
}

// formulaCost is one row of the --by-formula table: the model facts that carry
// one formula name. The empty name is the unattributed bucket.
type formulaCost struct {
	FormulaName         string
	Invocations         int
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
	CostUSDEstimate     float64
	Unpriced            int
}

// aggregateFormulaCosts buckets model facts by their own formula name, sorted
// by name. Compute facts carry no formula and are not counted.
func aggregateFormulaCosts(facts []usage.Fact) []formulaCost {
	byName := map[string]*formulaCost{}
	for _, f := range facts {
		if f.Kind != usage.KindModel {
			continue
		}
		fc := byName[f.FormulaName]
		if fc == nil {
			fc = &formulaCost{FormulaName: f.FormulaName}
			byName[f.FormulaName] = fc
		}
		addModelFact(&fc.Invocations, &fc.InputTokens, &fc.OutputTokens, &fc.CacheReadTokens, &fc.CacheCreationTokens, &fc.CostUSDEstimate, &fc.Unpriced, f)
	}
	rows := make([]formulaCost, 0, len(byName))
	for _, fc := range byName {
		rows = append(rows, *fc)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].FormulaName < rows[j].FormulaName })
	return rows
}

func doCosts(stdout, stderr io.Writer, byFormula bool) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc costs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	usagePath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	facts, warnings, err := usage.ReadFacts(usagePath)
	if err != nil {
		fmt.Fprintf(stderr, "gc costs: reading %s: %v\n", usagePath, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Surface skipped malformed lines so a partially corrupt log never silently
	// undercounts without a trace (the read itself stays non-fatal).
	for _, w := range warnings {
		fmt.Fprintf(stderr, "gc costs: %s\n", w) //nolint:errcheck // best-effort stderr
	}
	return renderCosts(stdout, facts, byFormula, usagePath)
}

// renderCosts writes the costs table for facts to stdout. usagePath only names
// the log in the empty-result message.
func renderCosts(stdout io.Writer, facts []usage.Fact, byFormula bool, usagePath string) int {
	if byFormula {
		return renderFormulaCosts(stdout, facts)
	}
	rows := aggregateRunCosts(facts)

	if len(rows) == 0 {
		fmt.Fprintf(stdout, "No usage facts recorded yet (%s).\n", usagePath) //nolint:errcheck
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tINVOCATIONS\tIN\tOUT\tCACHE_R\tCACHE_C\tWALL_S\tEST_USD\tUNPRICED") //nolint:errcheck
	var tot runCost
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%.1f\t%.4f\t%d\n", //nolint:errcheck
			truncRunID(r.RunID), r.Invocations, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.WallSeconds, r.CostUSDEstimate, r.Unpriced)
		tot.Invocations += r.Invocations
		tot.InputTokens += r.InputTokens
		tot.OutputTokens += r.OutputTokens
		tot.CacheReadTokens += r.CacheReadTokens
		tot.CacheCreationTokens += r.CacheCreationTokens
		tot.WallSeconds += r.WallSeconds
		tot.CostUSDEstimate += r.CostUSDEstimate
		tot.Unpriced += r.Unpriced
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\t%.1f\t%.4f\t%d\n", //nolint:errcheck
		tot.Invocations, tot.InputTokens, tot.OutputTokens, tot.CacheReadTokens, tot.CacheCreationTokens, tot.WallSeconds, tot.CostUSDEstimate, tot.Unpriced)
	tw.Flush() //nolint:errcheck
	if tot.Unpriced > 0 {
		fmt.Fprintf(stdout, "\nNote: %d invocation(s) had no pricing and are excluded from EST_USD.\n", tot.Unpriced) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Estimates are list-price decision-support, not authoritative charges.\n") //nolint:errcheck
	return 0
}

// renderFormulaCosts writes the --by-formula table. It has no wall-clock
// columns: compute facts carry no formula, so any wall figure here would be a
// number the table cannot attribute.
func renderFormulaCosts(stdout io.Writer, facts []usage.Fact) int {
	rows := aggregateFormulaCosts(facts)
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No model usage recorded yet to group by formula.") //nolint:errcheck
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "FORMULA\tINVOCATIONS\tIN\tOUT\tCACHE_R\tCACHE_C\tEST_USD\tUNPRICED") //nolint:errcheck
	var tot formulaCost
	for _, r := range rows {
		name := printableFormulaName(r.FormulaName)
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%.4f\t%d\n", //nolint:errcheck
			name, r.Invocations, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, r.CostUSDEstimate, r.Unpriced)
		tot.Invocations += r.Invocations
		tot.InputTokens += r.InputTokens
		tot.OutputTokens += r.OutputTokens
		tot.CacheReadTokens += r.CacheReadTokens
		tot.CacheCreationTokens += r.CacheCreationTokens
		tot.CostUSDEstimate += r.CostUSDEstimate
		tot.Unpriced += r.Unpriced
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\t%.4f\t%d\n", //nolint:errcheck
		tot.Invocations, tot.InputTokens, tot.OutputTokens, tot.CacheReadTokens, tot.CacheCreationTokens, tot.CostUSDEstimate, tot.Unpriced)
	tw.Flush() //nolint:errcheck
	if tot.Unpriced > 0 {
		fmt.Fprintf(stdout, "\nNote: %d invocation(s) had no pricing and are excluded from EST_USD.\n", tot.Unpriced) //nolint:errcheck
	}
	fmt.Fprintln(stdout, "\"-\" is usage with no formula attribution. Wall-seconds are omitted: compute facts carry no formula; see the default table for WALL_S.") //nolint:errcheck
	fmt.Fprintf(stdout, "Estimates are list-price decision-support, not authoritative charges.\n")                                                                  //nolint:errcheck
	return 0
}

// printableFormulaName replaces control characters in a formula name with
// '?' so a pack-authored name cannot carry a terminal escape sequence or a
// line break into the --by-formula table. Buckets are keyed on the raw name;
// only what is printed changes.
func printableFormulaName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, name)
}

func truncRunID(s string) string {
	if len(s) > 28 {
		return s[:25] + "..."
	}
	return s
}
