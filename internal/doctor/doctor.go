package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"
)

// errFixTimedOut is the internal sentinel boundedFix returns when a fix
// exceeds the per-check timeout and is abandoned. It never reaches user
// output — the runner substitutes a descriptive FixError message.
var errFixTimedOut = errors.New("fix timed out and was abandoned")

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
	// CheckTimeout bounds each individual check's Run, its --fix remediation,
	// and the post-fix verification re-run. Zero (the default) preserves the
	// historical unbounded behavior. When one of those exceeds the bound it is
	// abandoned so one wedged check (e.g. a store read stuck behind a saturated
	// data plane, or a fix script blocked on I/O) cannot stall the entire
	// doctor run and hide every check registered after it. A timed-out Run is
	// reported as a timed-out advisory error; a timed-out fix is reported as an
	// unconfirmed remediation.
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

		// abandoned is true when a goroutine for this check may still be
		// running — a timed-out Run, or a fix/verify abandoned below. Its
		// internal state is not settled, so RenderExtras must be skipped to
		// avoid reading half-mutated state.
		abandoned := result.TimedOut

		// Attempt fix if requested and the check supports it. A timed-out
		// check is skipped: its Run never completed, so its failure state
		// is unknown and a fix (plus the verifying re-run) could wedge the
		// loop the same way the check did.
		if fix && result.Status != StatusOK && !result.TimedOut && c.CanFix() {
			var fixAbandoned bool
			result, fixAbandoned = d.fixAndVerify(c, ctx, result)
			abandoned = abandoned || fixAbandoned || result.TimedOut
		}

		if stream {
			printResult(w, result, ctx.Verbose)
			// Skip extras for a check with a still-running abandoned
			// goroutine (timed-out Run or fix): it may still be mutating
			// internal state RenderExtras would read.
			if r, ok := c.(Renderer); ok && !abandoned {
				r.RenderExtras(ctx, w)
			}
		}
		r.Results = append(r.Results, result)
		r.tally(result)
	}
	return r
}

// tally folds one check result into the report's running counts. A fixed
// check counts as passed; a failing check increments BlockingFailed only when
// its severity gates.
func (r *Report) tally(result *CheckResult) {
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

// fixAndVerify remediates a failing, fixable check and re-runs it to confirm,
// bounding both the fix and the verifying re-run under the per-check timeout.
// It returns the result to report (res mutated in place on fix failure, or the
// fresh verification result on success) and whether the fix was abandoned at
// the bound. A fix that exceeds the timeout is abandoned — not killed — so
// gc doctor --fix cannot hang on a wedged remediation, while the fix still runs
// to completion in the background rather than being interrupted mid-mutation.
func (d *Doctor) fixAndVerify(c Check, ctx *CheckContext, res *CheckResult) (*CheckResult, bool) {
	switch err := d.boundedFix(c, ctx); {
	case errors.Is(err, errFixTimedOut):
		res.FixAttempted = true
		res.FixError = fmt.Sprintf("fix timed out after %s and was abandoned; remediation unconfirmed", d.CheckTimeout)
		return res, true
	case err != nil:
		res.FixError = err.Error()
		res.FixAttempted = true
		return res, false
	}
	// Verify the fix worked, bounding the re-run exactly like the initial run.
	// A check that fails fast, fixes fast, then wedges on this verification
	// would otherwise hang gc doctor --fix — re-opening the very failure mode
	// the per-check timeout closes.
	verified := d.boundedRun(c, ctx)
	if verified.Status == StatusOK {
		verified.Fixed = true
	} else {
		verified.FixAttempted = true
	}
	return verified, false
}

// boundedFix runs a check's Fix under the doctor's per-check timeout using the
// same abandon-on-timeout mechanism as boundedRun, including its output
// isolation. Zero timeout runs Fix inline (historical behavior). Otherwise Fix
// runs in a goroutine against a context whose Output is a private buffer: on
// completion the buffer is flushed to the real writer (keeping a fix's
// diagnostics grouped before the check's result line); on timeout the goroutine
// is abandoned with its private buffer, so a still-running fix can never
// interleave writes with — or race against — the rest of the run. The abandoned
// goroutine keeps running until its own I/O returns or the process exits, so the
// fix is not interrupted mid-mutation. errFixTimedOut is returned on timeout;
// otherwise Fix's own error (or nil) is returned.
func (d *Doctor) boundedFix(c Check, ctx *CheckContext) error {
	if d.CheckTimeout <= 0 {
		return c.Fix(ctx)
	}
	var buf bytes.Buffer
	fixCtx := *ctx
	fixCtx.Output = &buf
	done := make(chan error, 1)
	go func() { done <- c.Fix(&fixCtx) }()
	select {
	case err := <-done:
		if buf.Len() > 0 && ctx.Output != nil {
			buf.WriteTo(ctx.Output) //nolint:errcheck // best-effort output
		}
		return err
	case <-time.After(d.CheckTimeout):
		return errFixTimedOut
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
