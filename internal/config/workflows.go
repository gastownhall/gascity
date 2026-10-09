package config

// WorkflowsConfig holds graph-workflow outcome policy (the [workflows] table).
type WorkflowsConfig struct {
	// FailHalts makes a step's terminal gc.outcome=fail binding on the rest of
	// its workflow. A needs edge on a failed step halts the dependent (it closes
	// skipped, naming the failed step, instead of running), and a workflow root
	// cannot close pass while any step's terminal outcome is fail: it closes
	// fail and names the step. For a retried step the terminal outcome is the
	// last attempt's. Finalizers always run. Off by default, which keeps the
	// behaviour where a later step runs past a failed one and finalize grades
	// only its direct blockers and abort_scope members; turn it on once the
	// formulas' correct refusals no longer stop the work after them.
	FailHalts bool `toml:"fail_halts,omitempty"`
}

// FailHaltsEnabled reports whether [workflows] fail_halts is on. A nil config
// reads as off.
func (c *City) FailHaltsEnabled() bool {
	return c != nil && c.Workflows.FailHalts
}
