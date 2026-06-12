package doctor

import (
	"bytes"
	"fmt"
	"io"
	"time"
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
	// CheckTimeout bounds each individual check's Run. Zero (the default)
	// preserves the historical unbounded behavior. When a check exceeds
	// the bound it is abandoned and reported as a timed-out advisory error
	// so one wedged check (e.g. a store read stuck behind a saturated data
	// plane) cannot stall the entire doctor run and hide every check
	// registered after it.
	CheckTimeout time.Duration
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
		result := d.boundedRun(c, ctx)

		// Attempt fix if requested and the check supports it. A timed-out
		// check is skipped: its Run never completed, so its failure state
		// is unknown and a fix (plus the verifying re-run) could wedge the
		// loop the same way the check did.
		if fix && result.Status != StatusOK && !result.TimedOut && c.CanFix() {
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
			// Skip extras for a timed-out check: its Run goroutine may
			// still be mutating internal state RenderExtras would read.
			if r, ok := c.(Renderer); ok && !result.TimedOut {
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

// boundedRun executes one check under the doctor's per-check timeout.
// Zero timeout runs the check inline (historical behavior). Otherwise the
// check runs in a goroutine against a context whose Output is a private
// buffer: on completion the buffer is flushed to the real writer (keeping a
// check's incidental output grouped before its result line); on timeout the
// goroutine is abandoned with its private buffer, so a still-running check
// can never interleave writes with — or race against — the rest of the run.
func (d *Doctor) boundedRun(c Check, ctx *CheckContext) *CheckResult {
	if d.CheckTimeout <= 0 {
		return c.Run(ctx)
	}
	var buf bytes.Buffer
	checkCtx := *ctx
	checkCtx.Output = &buf
	done := make(chan *CheckResult, 1)
	go func() { done <- c.Run(&checkCtx) }()
	select {
	case result := <-done:
		if buf.Len() > 0 && ctx.Output != nil {
			buf.WriteTo(ctx.Output) //nolint:errcheck // best-effort output
		}
		return result
	case <-time.After(d.CheckTimeout):
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusError,
			Severity: SeverityAdvisory,
			TimedOut: true,
			Message:  fmt.Sprintf("timed out after %s and was abandoned (outcome unknown); re-run alone or raise --check-timeout", d.CheckTimeout),
		}
	}
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
	advisorySuffix := ""
	if r.Status != StatusOK && !r.Fixed && r.Severity == SeverityAdvisory {
		advisorySuffix = " (advisory)"
	}
	fmt.Fprintf(w, "  %s %s — %s%s%s\n", icon, r.Name, r.Message, advisorySuffix, suffix) //nolint:errcheck // best-effort output
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
