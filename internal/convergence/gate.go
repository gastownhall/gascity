package convergence

import (
	"fmt"
	"time"
)

// DefaultGateTimeout is the default timeout for gate condition scripts.
// Set to 5 minutes to accommodate build/test commands (e.g., make check)
// that commonly exceed the previous 60-second default.
const DefaultGateTimeout = 5 * time.Minute

// MaxGateRetries is the maximum number of retries for gate_timeout_action=retry.
const MaxGateRetries = 3

// GateConfig holds the immutable gate configuration for a convergence loop.
type GateConfig struct {
	Mode          string        // "manual", "condition", "hybrid"
	Condition     string        // path to gate condition script (empty for manual-only)
	Timeout       time.Duration // gate script timeout (default 5m)
	TimeoutAction string        // "iterate", "retry", "manual", "terminate"
}

// GateResult holds the outcome of a gate evaluation.
type GateResult struct {
	Outcome    string        // "pass", "fail", "timeout", "error" (use GateOutcome constants)
	ExitCode   *int          // nil if not applicable (manual mode, timeout)
	RetryCount int           // number of retries before final result
	Stdout     string        // captured stdout (truncated to MaxOutputBytes)
	Stderr     string        // captured stderr (truncated to MaxOutputBytes)
	Duration   time.Duration // wall-clock execution time
	Truncated  bool          // true if stdout or stderr was truncated
}

// GateManualResult returns a GateResult for manual mode (no script execution).
func GateManualResult() GateResult {
	return GateResult{
		Outcome: GatePass,
	}
}

// ParseGateConfig extracts gate configuration from convergence metadata.
// Uses defaults for missing fields: timeout=5m, timeout_action=iterate.
func ParseGateConfig(meta map[string]string) (GateConfig, error) {
	mode := meta[FieldGateMode]
	if mode == "" {
		mode = GateModeManual
	}

	switch mode {
	case GateModeManual, GateModeCondition, GateModeHybrid:
		// valid
	default:
		return GateConfig{}, fmt.Errorf("parsing gate config: invalid gate mode %q", mode)
	}

	timeout := DefaultGateTimeout
	if raw, ok := meta[FieldGateTimeout]; ok && raw != "" {
		d, valid := DecodeDuration(raw)
		if !valid {
			return GateConfig{}, fmt.Errorf("parsing gate config: invalid gate timeout %q", raw)
		}
		if d <= 0 {
			return GateConfig{}, fmt.Errorf("parsing gate config: gate timeout must be positive, got %v", d)
		}
		timeout = d
	}

	timeoutAction := TimeoutActionIterate
	if raw, ok := meta[FieldGateTimeoutAction]; ok && raw != "" {
		switch raw {
		case TimeoutActionIterate, TimeoutActionRetry, TimeoutActionManual, TimeoutActionTerminate:
			timeoutAction = raw
		default:
			return GateConfig{}, fmt.Errorf("parsing gate config: invalid gate timeout action %q", raw)
		}
	}

	return GateConfig{
		Mode:          mode,
		Condition:     meta[FieldGateCondition],
		Timeout:       timeout,
		TimeoutAction: timeoutAction,
	}, nil
}

// NeedsConditionExecution returns true if the gate mode requires running
// a condition script. Manual mode never runs scripts. Condition and hybrid
// modes run scripts only when a condition path is configured.
func (gc GateConfig) NeedsConditionExecution() bool {
	switch gc.Mode {
	case GateModeManual:
		return false
	case GateModeCondition, GateModeHybrid:
		return gc.Condition != ""
	default:
		return false
	}
}
