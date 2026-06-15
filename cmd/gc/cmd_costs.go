package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/gastownhall/gascity/usage"
	"github.com/spf13/cobra"
)

func newCostsCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Show per-run usage and estimated cost for this city",
		Long: `Aggregate recorded usage facts (model tokens and compute wall-seconds)
by run for local cost insight.

Reads .gc/usage.jsonl (the usage sink output) and groups facts by run id.
Cost is a list-price estimate for decision support, not an authoritative
charge; invocations with no pricing are flagged "unpriced" and excluded from
the cost total.`,
		Example: "  gc costs",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doCosts(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
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
func aggregateRunCosts(facts []usage.UsageFact) []runCost {
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
			rc.Invocations++
			rc.InputTokens += f.InputTokens
			rc.OutputTokens += f.OutputTokens
			rc.CacheReadTokens += f.CacheReadTokens
			rc.CacheCreationTokens += f.CacheCreationTokens
			if f.Unpriced {
				rc.Unpriced++
			} else {
				rc.CostUSDEstimate += f.CostUSDEstimate
			}
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

func doCosts(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc costs: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	usagePath := filepath.Join(cityPath, ".gc", "usage.jsonl")
	facts, err := usage.ReadFacts(usagePath)
	if err != nil {
		fmt.Fprintf(stderr, "gc costs: reading %s: %v\n", usagePath, err) //nolint:errcheck // best-effort stderr
		return 1
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

func truncRunID(s string) string {
	if len(s) > 28 {
		return s[:25] + "..."
	}
	return s
}
