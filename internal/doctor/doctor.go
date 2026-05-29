package doctor

import (
	"fmt"
	"io"
)

// Report summarizes the results of a doctor run.
type Report struct {
	// Passed is the number of checks with StatusOK.
	Passed int
	// Warned is the number of checks with StatusWarning.
	Warned int
	// Failed is the number of checks with StatusError (any severity).
	Failed int
	// BlockingFailed is the number of failed checks whose Severity is
	// SeverityBlocking — the subset of Failed that should gate dispatch,
	// CLI exit codes, and other automation.
	BlockingFailed int
	// Fixed is the number of checks remediated by --fix.
	Fixed int
	// Results holds the per-check results in the order they ran. Populated
	// by Run so callers that need structured output (e.g. `gc doctor --json`)
	// can project every result without re-running checks.
	Results []*CheckResult
}

// Doctor runs registered health checks and reports results.
type Doctor struct {
	checks []Check
}

// Register adds a check to the doctor's check list.
func (d *Doctor) Register(c Check) {
	d.checks = append(d.checks, c)
}

// Run executes all registered checks, streaming results to w as each
// completes. When fix is true, fixable checks that fail are remediated
// and re-run. Returns a summary report whose Results field holds every
// check result in execution order.
func (d *Doctor) Run(ctx *CheckContext, w io.Writer, fix bool) *Report {
	return d.run(ctx, w, fix, true)
}

// RunCollect executes all registered checks without streaming per-check
// output. The returned Report's Results field holds every check result in
// execution order so callers can render structured output (e.g. JSON).
// Fix semantics match Run.
func (d *Doctor) RunCollect(ctx *CheckContext, fix bool) *Report {
	return d.run(ctx, io.Discard, fix, false)
}

func (d *Doctor) run(ctx *CheckContext, w io.Writer, fix, stream bool) *Report {
	// Normalize ctx so individual checks always get a non-nil context with
	// an Output writer set. Done here so both Run and RunCollect benefit
	// — RunCollect routes Output to io.Discard so a check that writes to
	// ctx.Output incidentally won't disturb the JSON-collect path.
	if ctx == nil {
		ctx = &CheckContext{}
	}
	runCtx := *ctx
	if runCtx.Output == nil {
		runCtx.Output = w
	}
	ctx = &runCtx

	r := &Report{}
	for _, c := range d.checks {
		result := c.Run(ctx)

		// Attempt fix if requested and the check supports it.
		if fix && result.Status != StatusOK && c.CanFix() {
			if err := c.Fix(ctx); err == nil {
				// Re-run to verify the fix worked.
				result = c.Run(ctx)
				if result.Status == StatusOK {
					result.Fixed = true
				} else {
					result.FixAttempted = true
				}
			} else {
				result.FixError = err.Error()
				result.FixAttempted = true
			}
		}

		if stream {
			printResult(w, result, ctx.Verbose)
			if r, ok := c.(Renderer); ok {
				r.RenderExtras(ctx, w)
			}
		}
		r.Results = append(r.Results, result)

		switch {
		case result.Fixed:
			r.Fixed++
			r.Passed++ // Fixed counts as passed.
		case result.Status == StatusOK:
			r.Passed++
		case result.Status == StatusWarning:
			r.Warned++
		case result.Status == StatusError:
			r.Failed++
			if result.Severity == SeverityBlocking {
				r.BlockingFailed++
			}
		}
	}
	return r
}

// printResult writes a single check result line to w.
func printResult(w io.Writer, r *CheckResult, verbose bool) {
	var icon string
	switch {
	case r.Fixed:
		icon = "✓" // Fixed shows as pass.
	case r.Status == StatusOK:
		icon = "✓"
	case r.Status == StatusWarning:
		icon = "⚠"
	case r.Status == StatusError:
		icon = "✗"
	}

	suffix := ""
	if r.Fixed {
		suffix = " (fixed)"
	}
	fmt.Fprintf(w, "  %s %s — %s%s\n", icon, r.Name, r.Message, suffix) //nolint:errcheck // best-effort output
	if verbose {
		for _, d := range r.Details {
			fmt.Fprintf(w, "      %s\n", d) //nolint:errcheck // best-effort output
		}
	}
	if r.FixError != "" && r.Status != StatusOK && !r.Fixed {
		fmt.Fprintf(w, "      fix failed: %s\n", r.FixError) //nolint:errcheck // best-effort output
	} else if r.FixAttempted && r.Status != StatusOK && !r.Fixed {
		fmt.Fprintf(w, "      fix attempted; check still failing\n") //nolint:errcheck // best-effort output
	}
	if r.FixHint != "" && r.Status != StatusOK && !r.Fixed {
		fmt.Fprintf(w, "      hint: %s\n", r.FixHint) //nolint:errcheck // best-effort output
	}
}

// PrintSummary writes the final summary line to w.
func PrintSummary(w io.Writer, r *Report) {
	parts := []string{}
	if r.Passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", r.Passed))
	}
	if r.Warned > 0 {
		parts = append(parts, fmt.Sprintf("%d warnings", r.Warned))
	}
	if r.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", r.Failed))
	}
	if advisory := r.Failed - r.BlockingFailed; advisory > 0 {
		parts = append(parts, fmt.Sprintf("%d advisory", advisory))
	}
	if r.Fixed > 0 {
		parts = append(parts, fmt.Sprintf("%d fixed", r.Fixed))
	}
	if len(parts) == 0 {
		fmt.Fprintln(w, "\nNo checks ran.") //nolint:errcheck // best-effort output
		return
	}
	fmt.Fprintf(w, "\n") //nolint:errcheck // best-effort output
	for i, p := range parts {
		if i > 0 {
			fmt.Fprintf(w, ", ") //nolint:errcheck // best-effort output
		}
		fmt.Fprintf(w, "%s", p) //nolint:errcheck // best-effort output
	}
	fmt.Fprintf(w, "\n") //nolint:errcheck // best-effort output
}
