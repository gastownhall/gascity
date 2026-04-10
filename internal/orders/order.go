// Package orders provides parsing, scanning, and gate evaluation for Gas City
// orders. Orders are discovered from top-level orders/<name>.order.toml files,
// with deprecated fallback support for older directory layouts.
package orders

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// Order is a parsed order definition from a discovered order file.
type Order struct {
	// Name is derived from the discovered filename or directory name (not from TOML).
	Name string `toml:"-"`
	// Description explains what this order does.
	Description string `toml:"description,omitempty"`
	// Formula is the formula name to dispatch when the gate opens.
	// Mutually exclusive with Exec.
	Formula string `toml:"formula,omitempty"`
	// Exec is a shell command run directly by the controller, bypassing
	// the agent pipeline. Mutually exclusive with Formula.
	Exec string `toml:"exec,omitempty"`
	// Gate is the gate type: "cooldown", "cron", "condition", or "manual".
	Gate string `toml:"gate"`
	// Interval is the minimum time between runs (for cooldown gates). Go duration string.
	Interval string `toml:"interval,omitempty"`
	// Schedule is a cron-like expression (for cron gates).
	Schedule string `toml:"schedule,omitempty"`
	// Check is a shell command that returns exit 0 when the formula should run (for condition gates).
	Check string `toml:"check,omitempty"`
	// On is the event type to match (for event gates). E.g., "bead.closed".
	On string `toml:"on,omitempty"`
	// Pool is the target agent/pool for dispatching the wisp.
	Pool string `toml:"pool,omitempty"`
	// Timeout is the per-order timeout. Go duration string (e.g., "90s").
	// Defaults to 60s for exec, 30s for formula.
	Timeout string `toml:"timeout,omitempty"`
	// Enabled controls whether the order is active. Defaults to true.
	Enabled *bool `toml:"enabled,omitempty"`
	// Source is the absolute file path to the discovered order file (set by scanner, not from TOML).
	Source string `toml:"-"`
	// FormulaLayer is the formula layer directory this order was
	// scanned from (set by scanner, not from TOML).
	FormulaLayer string `toml:"-"`
	// Rig is the rig name this order is scoped to. Empty for city-level orders.
	// Set by the scanning caller, not from TOML.
	Rig string `toml:"-"`
}

// ScopedName returns a rig-qualified key for label scoping.
// City-level: "dolt-health". Rig-level: "dolt-health:rig:demo-repo".
func (a *Order) ScopedName() string {
	if a.Rig == "" {
		return a.Name
	}
	return a.Name + ":rig:" + a.Rig
}

// orderFile wraps the TOML structure with an [order] header.
type orderFile struct {
	Order Order `toml:"order"`
}

// IsEnabled reports whether the order is enabled. Defaults to true if not set.
func (a *Order) IsEnabled() bool {
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// IsExec reports whether this order uses exec (script) dispatch
// rather than formula (wisp) dispatch.
func (a *Order) IsExec() bool {
	return a.Exec != ""
}

// TimeoutOrDefault returns the order's configured timeout, or the
// default: 300s for exec orders, 30s for formula orders.
func (a *Order) TimeoutOrDefault() time.Duration {
	if a.Timeout != "" {
		if d, err := time.ParseDuration(a.Timeout); err == nil {
			return d
		}
	}
	if a.IsExec() {
		return 300 * time.Second
	}
	return 30 * time.Second
}

// Parse decodes TOML data into an Order.
func Parse(data []byte) (Order, error) {
	var af orderFile
	if _, err := toml.Decode(string(data), &af); err != nil {
		return Order{}, fmt.Errorf("parsing order: %w", err)
	}
	return af.Order, nil
}

// Validate checks an Order for structural correctness based on its gate type.
func Validate(a Order) error {
	// formula XOR exec — exactly one required.
	if a.Formula == "" && a.Exec == "" {
		return fmt.Errorf("order %q: formula or exec is required", a.Name)
	}
	if a.Formula != "" && a.Exec != "" {
		return fmt.Errorf("order %q: formula and exec are mutually exclusive", a.Name)
	}
	// Exec orders must not have a pool (no agent pipeline).
	if a.Exec != "" && a.Pool != "" {
		return fmt.Errorf("order %q: exec orders cannot have a pool", a.Name)
	}
	// Validate timeout if set.
	if a.Timeout != "" {
		if _, err := time.ParseDuration(a.Timeout); err != nil {
			return fmt.Errorf("order %q: invalid timeout %q: %w", a.Name, a.Timeout, err)
		}
	}
	switch a.Gate {
	case "cooldown":
		if a.Interval == "" {
			return fmt.Errorf("order %q: cooldown gate requires interval", a.Name)
		}
		if _, err := time.ParseDuration(a.Interval); err != nil {
			return fmt.Errorf("order %q: invalid interval %q: %w", a.Name, a.Interval, err)
		}
	case "cron":
		if a.Schedule == "" {
			return fmt.Errorf("order %q: cron gate requires schedule", a.Name)
		}
	case "condition":
		if a.Check == "" {
			return fmt.Errorf("order %q: condition gate requires check command", a.Name)
		}
	case "event":
		if a.On == "" {
			return fmt.Errorf("order %q: event gate requires on (event type)", a.Name)
		}
	case "manual":
		// No additional fields required.
	case "":
		return fmt.Errorf("order %q: gate is required", a.Name)
	default:
		return fmt.Errorf("order %q: unknown gate type %q", a.Name, a.Gate)
	}
	return nil
}
