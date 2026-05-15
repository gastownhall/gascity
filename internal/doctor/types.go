// Package doctor provides system health diagnostics for a Gas City workspace.
// It defines a Check interface and runner that executes checks with streaming
// output, optional --fix support, and a summary report.
package doctor

import "io"

// CheckStatus represents the outcome of a health check.
type CheckStatus int

const (
	// StatusOK means the check passed.
	StatusOK CheckStatus = iota
	// StatusWarning means the check found a non-critical issue.
	StatusWarning
	// StatusError means the check found a critical problem.
	StatusError
)

// Check is a single diagnostic check. Implementations are registered with
// a Doctor and executed sequentially during Run.
type Check interface {
	// Name returns a short, unique identifier for this check (e.g. "city-config").
	Name() string
	// Run executes the check and returns a result.
	Run(ctx *CheckContext) *CheckResult
	// CanFix reports whether this check supports automatic remediation.
	CanFix() bool
	// Fix attempts to automatically remediate the issue found by Run.
	// Only called when CanFix returns true and Run returned a non-OK status.
	Fix(ctx *CheckContext) error
}

// CheckContext carries shared state for all checks during a doctor run.
type CheckContext struct {
	// CityPath is the absolute path to the city root directory.
	CityPath string
	// Verbose enables extra diagnostic output in check results.
	Verbose bool
	// Output is the writer used for doctor output during Doctor.Run.
	// Checks that need to surface fix-time diagnostics should use this
	// writer so captured doctor output includes the diagnostics.
	Output io.Writer
}

// CheckResult holds the outcome of a single check execution.
type CheckResult struct {
	// Name identifies which check produced this result.
	Name string
	// Status is the outcome: OK, Warning, or Error.
	Status CheckStatus
	// Message is a human-readable summary of the result.
	Message string
	// Details holds extra lines shown only in verbose mode.
	Details []string
	// FixHint is a suggestion shown when the check fails and cannot auto-fix.
	FixHint string
	// FixError describes why an attempted automatic remediation failed.
	FixError string
	// FixAttempted is true when automatic remediation ran but did not
	// leave the check passing.
	FixAttempted bool
	// Fixed is true when --fix successfully remediated the issue.
	Fixed bool
}
