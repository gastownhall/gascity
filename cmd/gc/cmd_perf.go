package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newPerfCmd builds the hidden `gc perf` subcommand tree.
// These commands are for development use: they measure command-line latency
// and are not part of the public gc interface.
func newPerfCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "perf",
		Short:  "Performance benchmarking harness (development use)",
		Hidden: true,
	}
	cmd.AddCommand(newPerfSessionNewCmd(stdout, stderr))
	cmd.AddCommand(newPerfRunCmd(stdout, stderr))
	return cmd
}

// perfCmdOptions holds flags shared across perf subcommands.
type perfCmdOptions struct {
	iter    int
	warmup  int
	jsonOut bool
}

// perfIterResult captures timing for one iteration.
type perfIterResult struct {
	Iter   int              `json:"iter"`
	WallMs int64            `json:"wall_ms"`
	Steps  map[string]int64 `json:"steps,omitempty"`
}

// perfReport is the aggregate output of a perf run.
type perfReport struct {
	Scenario   string           `json:"scenario"`
	Iterations int              `json:"iterations"`
	Results    []perfIterResult `json:"results"`
	Stats      perfStats        `json:"stats"`
}

// perfStats summarizes per-iteration wall-clock measurements.
type perfStats struct {
	MinMs  int64 `json:"min_ms"`
	MeanMs int64 `json:"mean_ms"`
	P50Ms  int64 `json:"p50_ms"`
	P95Ms  int64 `json:"p95_ms"`
	MaxMs  int64 `json:"max_ms"`
}

// computePerfStats builds summary statistics from a slice of wall-clock ms values.
func computePerfStats(walls []int64) perfStats {
	if len(walls) == 0 {
		return perfStats{}
	}
	sorted := make([]int64, len(walls))
	copy(sorted, walls)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum int64
	for _, v := range sorted {
		sum += v
	}
	n := len(sorted)
	mean := sum / int64(n)

	p50 := sorted[int(math.Round(float64(n)*0.50))-1]
	p95idx := int(math.Ceil(float64(n)*0.95)) - 1
	if p95idx >= n {
		p95idx = n - 1
	}
	p95 := sorted[p95idx]

	return perfStats{
		MinMs:  sorted[0],
		MeanMs: mean,
		P50Ms:  p50,
		P95Ms:  p95,
		MaxMs:  sorted[n-1],
	}
}

// gcBinaryPath returns the path to the running gc binary.
func gcBinaryPath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving gc binary: %w", err)
	}
	return path, nil
}

// lifecycleStepRe matches "phases=[start_call=Xms ...]" in lifecycle log output.
// It extracts the full phases substring for further parsing.
var lifecycleStepRe = regexp.MustCompile(`phases=\[([^\]]*)\]`)

// lifecycleStepPairRe matches one "key=Xms" pair inside a phases block.
var lifecycleStepPairRe = regexp.MustCompile(`(\w+)=(\d+(?:\.\d+)?)(ms|s|µs)`)

// parseLifecycleSteps extracts step timings from the stderr output of a gc
// command. It looks for "session lifecycle: ... phases=[...]" lines and
// returns a map of step-name → duration in milliseconds.
func parseLifecycleSteps(stderr string) map[string]int64 {
	m := lifecycleStepRe.FindStringSubmatch(stderr)
	if m == nil {
		return nil
	}
	pairs := lifecycleStepPairRe.FindAllStringSubmatch(m[1], -1)
	if len(pairs) == 0 {
		return nil
	}
	steps := make(map[string]int64, len(pairs))
	for _, p := range pairs {
		key := p[1]
		val, err := strconv.ParseFloat(p[2], 64)
		if err != nil {
			continue
		}
		unit := p[3]
		var ms int64
		switch unit {
		case "s":
			ms = int64(val * 1000)
		case "ms":
			ms = int64(val)
		case "µs":
			ms = int64(val / 1000)
		}
		steps[key] = ms
	}
	return steps
}

// printPerfReport writes a human-readable perf table to w.
func printPerfReport(w io.Writer, r perfReport) {
	fmt.Fprintf(w, "\ngc perf %s (%d iterations)\n\n", r.Scenario, r.Iterations) //nolint:errcheck
	// Collect all step names in stable order.
	stepNames := map[string]struct{}{}
	for _, res := range r.Results {
		for k := range res.Steps {
			stepNames[k] = struct{}{}
		}
	}
	sortedSteps := make([]string, 0, len(stepNames))
	for k := range stepNames {
		sortedSteps = append(sortedSteps, k)
	}
	sort.Strings(sortedSteps)

	// Header.
	header := fmt.Sprintf("%-6s  %10s", "Iter", "Wall(ms)")
	for _, s := range sortedSteps {
		header += fmt.Sprintf("  %16s", s)
	}
	fmt.Fprintln(w, header)                           //nolint:errcheck
	fmt.Fprintln(w, strings.Repeat("-", len(header))) //nolint:errcheck

	// Rows.
	for _, res := range r.Results {
		row := fmt.Sprintf("%-6d  %10d", res.Iter, res.WallMs)
		for _, s := range sortedSteps {
			v := res.Steps[s]
			row += fmt.Sprintf("  %16d", v)
		}
		fmt.Fprintln(w, row) //nolint:errcheck
	}

	// Stats row.
	fmt.Fprintln(w)                                                              //nolint:errcheck
	fmt.Fprintf(w, "Stats  min=%dms  mean=%dms  p50=%dms  p95=%dms  max=%dms\n", //nolint:errcheck
		r.Stats.MinMs, r.Stats.MeanMs, r.Stats.P50Ms, r.Stats.P95Ms, r.Stats.MaxMs)
}

// runPerfIterations executes gcBin with the given args iter times (after warmup
// warmup rounds), measuring wall-clock per run and parsing step timings from
// stderr. Returns a perfReport ready for output.
func runPerfIterations(gcBin, scenario string, args []string, opts perfCmdOptions) perfReport {
	total := opts.warmup + opts.iter
	results := make([]perfIterResult, 0, opts.iter)
	for i := 0; i < total; i++ {
		var stderrBuf bytes.Buffer
		cmd := exec.Command(gcBin, args...) //nolint:gosec // gcBin is resolved from os.Executable
		cmd.Stdout = io.Discard
		cmd.Stderr = &stderrBuf
		disableProductMetricsForChild(cmd)
		start := time.Now()
		runErr := cmd.Run()
		wallMs := time.Since(start).Milliseconds()
		if runErr != nil {
			// Non-zero exit is expected for some commands (e.g. session new without a
			// real city). Capture timing but mark steps unavailable.
			_ = runErr
		}
		if i < opts.warmup {
			continue // discard warmup
		}
		iter := i - opts.warmup + 1
		steps := parseLifecycleSteps(stderrBuf.String())
		results = append(results, perfIterResult{
			Iter:   iter,
			WallMs: wallMs,
			Steps:  steps,
		})
	}

	walls := make([]int64, len(results))
	for i, r := range results {
		walls[i] = r.WallMs
	}
	return perfReport{
		Scenario:   scenario,
		Iterations: opts.iter,
		Results:    results,
		Stats:      computePerfStats(walls),
	}
}

// newPerfSessionNewCmd implements `gc perf session-new`.
//
// It scaffolds a minimal temporary city with the "file" beads provider and a
// fast no-op agent, then runs `gc session new perf-worker --no-attach --json`
// N times to measure per-command latency. Step-level timing is extracted from
// lifecycle log lines in stderr.
func newPerfSessionNewCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := perfCmdOptions{}
	var template string
	cmd := &cobra.Command{
		Use:   "session-new",
		Short: "Measure gc session new latency with a fresh temp city",
		Long: `Sets up a temporary city with a minimal config and measures the
end-to-end latency of "gc session new --no-attach" across multiple
iterations. Step-level timing (start_call, post_start_observe, …) is
extracted from lifecycle log output when available.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPerfSessionNew(opts, template, stdout, stderr)
		},
	}
	cmd.Flags().IntVar(&opts.iter, "iter", 5, "number of measured iterations")
	cmd.Flags().IntVar(&opts.warmup, "warmup", 1, "warmup iterations excluded from statistics")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	cmd.Flags().StringVar(&template, "template", "perf-worker", "agent template name to use")
	return cmd
}

// perfCityTemplate is a self-contained city config that uses the in-process
// file-based bead store (no Dolt) and a no-op subprocess agent.
const perfCityTemplate = `[workspace]
name = "perf-city"

[beads]
provider = "file"
`

// perfPackTemplate declares a named session for the perf-worker template.
const perfPackTemplate = `[pack]
name = "perf-city"
schema = 2

[[named_session]]
template = "%s"
`

// perfAgentTemplate is the per-agent TOML config — uses codex as the base
// provider with "true" as the no-op start command.
const perfAgentTemplate = `provider = "codex"
start_command = "true"
`

// setupPerfCity creates a temp directory with the minimal scaffold for a
// gc session new run. It returns the city path and a cleanup function.
func setupPerfCity(agentName string) (cityPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "gc-perf-city-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp city: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("creating .gc dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(perfCityTemplate), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing city.toml: %w", err)
	}

	packContent := fmt.Sprintf(perfPackTemplate, agentName)
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte(packContent), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing pack.toml: %w", err)
	}

	agentDir := filepath.Join(dir, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("creating agent dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(perfAgentTemplate), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing agent.toml: %w", err)
	}

	siteContent := fmt.Sprintf("workspace_name = %q\n", "perf-city")
	if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte(siteContent), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing .gc/site.toml: %w", err)
	}

	return dir, cleanup, nil
}

// runPerfSessionNew is the testable core for `gc perf session-new`.
func runPerfSessionNew(opts perfCmdOptions, agentName string, stdout, _ io.Writer) error {
	gcBin, err := gcBinaryPath()
	if err != nil {
		return fmt.Errorf("gc perf session-new: %w", err)
	}

	cityPath, cleanup, err := setupPerfCity(agentName)
	if err != nil {
		return fmt.Errorf("gc perf session-new: %w", err)
	}
	defer cleanup()

	args := []string{"--city", cityPath, "session", "new", agentName, "--no-attach", "--json"}
	report := runPerfIterations(gcBin, "session-new", args, opts)
	if opts.jsonOut {
		return json.NewEncoder(stdout).Encode(report)
	}
	printPerfReport(stdout, report)
	return nil
}

// newPerfRunCmd implements `gc perf run -- <gc args...>`.
func newPerfRunCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := perfCmdOptions{}
	cmd := &cobra.Command{
		Use:   "run [flags] -- <gc args...>",
		Short: "Measure wall-clock latency of an arbitrary gc command",
		Long: `Runs an arbitrary gc command line repeatedly and reports timing statistics.
Pass the gc subcommand and its flags after "--".

Example:
  gc perf run --iter 10 -- status
  gc perf run --iter 5 -- session list`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			return runPerfRun(opts, args, stdout, stderr)
		},
	}
	cmd.Flags().IntVar(&opts.iter, "iter", 5, "number of measured iterations")
	cmd.Flags().IntVar(&opts.warmup, "warmup", 1, "warmup iterations excluded from statistics")
	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit JSON instead of a table")
	return cmd
}

// runPerfRun is the testable core for `gc perf run`.
func runPerfRun(opts perfCmdOptions, args []string, stdout, _ io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("gc perf run: no gc arguments provided; use -- <gc args>")
	}
	gcBin, err := gcBinaryPath()
	if err != nil {
		return fmt.Errorf("gc perf run: %w", err)
	}

	scenario := "run[" + strings.Join(args, " ") + "]"
	report := runPerfIterations(gcBin, scenario, args, opts)
	if opts.jsonOut {
		return json.NewEncoder(stdout).Encode(report)
	}
	printPerfReport(stdout, report)
	return nil
}
