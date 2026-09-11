package convergence

// GateOutput is the read-side projection of the exec-gate output vocabulary —
// the convergence.gate_* metadata keys that Handler.persistGateOutcome stamps on
// convergence-loop root beads. It is the confinement boundary for that
// vocabulary: consumers (the orders API history handlers) read a GateOutput and
// never touch the convergence.gate_* keys directly, so internal/convergence
// stays the sole owner of the key literals. GateOutput is the read-side twin of
// the persistGateOutcome write path.
//
// The fields are raw strings on purpose: every consumer either forwards a value
// verbatim on the wire or does a presence check, and for these keys a
// present-but-empty value is indistinguishable from absent — so plain strings
// match the callers' `ok && v != ""` semantics exactly.
type GateOutput struct {
	// DurationMs is the wall-clock gate duration in milliseconds.
	DurationMs string
	// ExitCode is the gate command's process exit code.
	ExitCode string
	// Stdout is the captured gate standard output.
	Stdout string
	// Stderr is the captured gate standard error.
	Stderr string
}

// GateOutputFromMetadata projects a bead's metadata onto a GateOutput, reading
// only the convergence.gate_* fields. It is nil-map safe: a nil map yields the
// zero GateOutput.
func GateOutputFromMetadata(meta map[string]string) GateOutput {
	return GateOutput{
		DurationMs: meta[FieldGateDurationMs],
		ExitCode:   meta[FieldGateExitCode],
		Stdout:     meta[FieldGateStdout],
		Stderr:     meta[FieldGateStderr],
	}
}

// HasOutput reports whether the gate captured any stdout or stderr.
func (g GateOutput) HasOutput() bool {
	return g.Stdout != "" || g.Stderr != ""
}

// CombinedOutput returns the gate's combined output for display: stdout first,
// then stderr appended after a newline separator when both are present. It
// matches the order-history detail handler's prior inline assembly byte for
// byte.
func (g GateOutput) CombinedOutput() string {
	output := g.Stdout
	if g.Stderr != "" {
		if output != "" {
			output += "\n"
		}
		output += g.Stderr
	}
	return output
}
