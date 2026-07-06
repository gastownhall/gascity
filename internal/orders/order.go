// Package orders provides parsing, scanning, and trigger evaluation for Gas City
// orders. Orders are discovered from flat top-level orders/<name>.toml files,
// with optional legacy-infix orders/<name>.order.toml support. Older PackV1
// subdirectory layouts are rejected with migration guidance.
package orders

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Order is a parsed order definition from a discovered order file.
type Order struct {
	// Name is derived from the discovered filename or directory name (not from TOML).
	Name string `toml:"-"`
	// skipAliases lists alternate names that match [orders].skip without changing
	// the order's canonical runtime name.
	skipAliases []string
	// Description explains what this order does.
	Description string `toml:"description,omitempty"`
	// Formula is the formula name to dispatch when the trigger fires.
	// Mutually exclusive with Exec.
	Formula string `toml:"formula,omitempty"`
	// Exec is a shell command run directly by the controller, bypassing
	// the agent pipeline. Mutually exclusive with Formula.
	Exec string `toml:"exec,omitempty"`
	// Scope controls how the order is instantiated during pack expansion:
	// "city" registers the order exactly once regardless of how many rigs
	// import the pack; "rig" (the default when empty) registers it once per
	// importing rig. Mirrors [[named_session]].scope.
	Scope string `toml:"scope,omitempty"`
	// Trigger is the order scheduler selector: "cooldown", "cron",
	// "condition", "event", or "manual". This is distinct from the
	// separate "gate" concepts used elsewhere in the system.
	Trigger string `toml:"trigger"`
	// Interval is the minimum time between runs (for cooldown triggers). Go duration string.
	Interval string `toml:"interval,omitempty"`
	// Schedule is a cron-like expression (for cron triggers).
	Schedule string `toml:"schedule,omitempty"`
	// Check is a shell command that returns exit 0 when the formula should run (for condition triggers).
	Check string `toml:"check,omitempty"`
	// On is the event type to match (for event triggers). E.g., "bead.closed".
	On string `toml:"on,omitempty"`
	// Pool is the target agent/pool for dispatching the wisp.
	Pool string `toml:"pool,omitempty"`
	// Timeout is the per-order timeout. Go duration string (e.g., "90s").
	// Defaults to 60s for exec, 30s for formula.
	Timeout string `toml:"timeout,omitempty"`
	// Enabled controls whether the order is active. Defaults to true.
	Enabled *bool `toml:"enabled,omitempty"`
	// Idempotent marks an order whose dispatch is safe to repeat (a sweep/
	// feeder whose re-run is a no-op, e.g. routes only unrouted work, or
	// nudges an idle pool). Such orders fail OPEN when the single-flight /
	// open-work gate times out under store contention: they dispatch anyway
	// rather than be starved, since a duplicate run causes no harm
	// (gastownhall/gascity#2893). Non-idempotent orders (the
	// default, false) keep failing CLOSED on gate timeout.
	Idempotent bool `toml:"idempotent,omitempty"`
	// Env is a map of environment variables exported into an exec
	// order's child process. Use the `[order.env]` TOML table to
	// override thresholds (e.g. GC_DOCTOR_LATENCY_WARN_S) without
	// editing the order's shell scripts or the controller's parent
	// environment. Env is supported only for exec orders; controller-
	// owned routing and identity keys are rejected before dispatch.
	Env map[string]string `toml:"env,omitempty"`
	// Params declares the named arguments this order accepts through the
	// dispatch args channel (webhook rules and `gc order run --var`). A param
	// marked required must be present in the dispatch vars or the order refuses
	// to fire. Use the `[order.params]` TOML table, e.g. `repo = { required = true }`.
	Params map[string]OrderParam `toml:"params,omitempty"`
	// Source is the absolute file path to the discovered order file (set by scanner, not from TOML).
	Source string `toml:"-"`
	// FormulaLayer is the formula layer directory this order was
	// scanned from (set by scanner, not from TOML).
	FormulaLayer string `toml:"-"`
	// Rig is the rig name this order is scoped to. Empty for city-level orders.
	// Set by the scanning caller, not from TOML.
	Rig string `toml:"-"`
}

// OrderParam describes one declared order parameter in the `[order.params]`
// table.
type OrderParam struct {
	// Required makes dispatch fail when the param is absent from the vars
	// supplied by the args channel.
	Required bool `toml:"required,omitempty"`
}

// ScopedName returns a rig-qualified key for label scoping.
// City-level: "dolt-health". Rig-level: "dolt-health:rig:demo-repo".
func (a *Order) ScopedName() string {
	if a.Rig == "" {
		return a.Name
	}
	return a.Name + ":rig:" + a.Rig
}

type orderDecode struct {
	Description string                `toml:"description,omitempty"`
	Formula     string                `toml:"formula,omitempty"`
	Exec        string                `toml:"exec,omitempty"`
	Scope       string                `toml:"scope,omitempty"`
	Trigger     string                `toml:"trigger,omitempty"`
	Gate        string                `toml:"gate,omitempty"`
	Interval    string                `toml:"interval,omitempty"`
	Schedule    string                `toml:"schedule,omitempty"`
	Check       string                `toml:"check,omitempty"`
	On          string                `toml:"on,omitempty"`
	Pool        string                `toml:"pool,omitempty"`
	Timeout     string                `toml:"timeout,omitempty"`
	Enabled     *bool                 `toml:"enabled,omitempty"`
	Idempotent  bool                  `toml:"idempotent,omitempty"`
	Env         map[string]string     `toml:"env,omitempty"`
	Params      map[string]OrderParam `toml:"params,omitempty"`
	SkipAliases []string              `toml:"skip_aliases,omitempty"`
}

func (d orderDecode) normalized() Order {
	trigger := d.Trigger
	if trigger == "" {
		trigger = d.Gate
	}
	return Order{
		Description: d.Description,
		Formula:     d.Formula,
		Exec:        d.Exec,
		Scope:       d.Scope,
		Trigger:     trigger,
		Interval:    d.Interval,
		Schedule:    d.Schedule,
		Check:       d.Check,
		On:          d.On,
		Pool:        d.Pool,
		Timeout:     d.Timeout,
		Enabled:     d.Enabled,
		Idempotent:  d.Idempotent,
		Env:         d.Env,
		Params:      d.Params,
		skipAliases: d.SkipAliases,
	}
}

// orderFile wraps the TOML structure with an [order] header.
type orderFile struct {
	Order orderDecode `toml:"order"`
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

// IsCityScoped reports whether the order is city-scoped, i.e. instantiated
// exactly once during pack expansion regardless of how many rigs import the
// pack. The default (empty Scope) is rig-scoped.
func (a *Order) IsCityScoped() bool {
	return a.Scope == "city"
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
	return af.Order.normalized(), nil
}

// Validate checks an Order for structural correctness based on its trigger type.
func Validate(a Order) error {
	// formula XOR exec — exactly one required.
	if a.Formula == "" && a.Exec == "" {
		return fmt.Errorf("order %q: formula or exec is required", a.Name)
	}
	if a.Formula != "" && a.Exec != "" {
		return fmt.Errorf("order %q: formula and exec are mutually exclusive", a.Name)
	}
	if len(a.Env) > 0 && a.Exec == "" {
		return fmt.Errorf("order %q: env is supported only for exec orders", a.Name)
	}
	// Exec orders must not have a pool (no agent pipeline).
	if a.Exec != "" && a.Pool != "" {
		return fmt.Errorf("order %q: exec orders cannot have a pool", a.Name)
	}
	for key := range a.Env {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("order %q: env key is required", a.Name)
		}
		if strings.Contains(key, "=") {
			return fmt.Errorf("order %q: invalid env key %q: must not contain '='", a.Name, key)
		}
	}
	for name := range a.Params {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("order %q: param name is required", a.Name)
		}
	}
	// Scope, if set, must be a known value. Empty defaults to rig-scoped.
	switch a.Scope {
	case "", "city", "rig":
	default:
		return fmt.Errorf("order %q: unknown scope %q (want \"city\" or \"rig\")", a.Name, a.Scope)
	}
	// Validate timeout if set.
	if a.Timeout != "" {
		if _, err := time.ParseDuration(a.Timeout); err != nil {
			return fmt.Errorf("order %q: invalid timeout %q: %w", a.Name, a.Timeout, err)
		}
	}
	switch a.Trigger {
	case "cooldown":
		if a.Interval == "" {
			return fmt.Errorf("order %q: cooldown trigger requires interval", a.Name)
		}
		if _, err := time.ParseDuration(a.Interval); err != nil {
			return fmt.Errorf("order %q: invalid interval %q: %w", a.Name, a.Interval, err)
		}
	case "cron":
		if a.Schedule == "" {
			return fmt.Errorf("order %q: cron trigger requires schedule", a.Name)
		}
	case "condition":
		if a.Check == "" {
			return fmt.Errorf("order %q: condition trigger requires check command", a.Name)
		}
	case "event":
		if a.On == "" {
			return fmt.Errorf("order %q: event trigger requires on (event type)", a.Name)
		}
	case "manual":
		// No additional fields required.
	case "webhook":
		// Webhook-dispatched orders declare the named args they accept so the
		// receiver can validate required params before dispatch. An order with
		// no [order.params] could never receive webhook args meaningfully.
		if len(a.Params) == 0 {
			return fmt.Errorf("order %q: webhook trigger requires a non-empty [order.params]", a.Name)
		}
	case "":
		return fmt.Errorf("order %q: trigger is required", a.Name)
	default:
		return fmt.Errorf("order %q: unknown trigger type %q", a.Name, a.Trigger)
	}
	return nil
}

// MissingRequiredParams returns the names of declared-required params that are
// absent from vars, sorted. It returns nil when every required param is present.
func (a *Order) MissingRequiredParams(vars map[string]string) []string {
	var missing []string
	for name, p := range a.Params {
		if !p.Required {
			continue
		}
		if _, ok := vars[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// ValidateRequiredParams returns an error naming any declared-required params
// absent from the dispatch vars. A nil return means dispatch may proceed.
func ValidateRequiredParams(a Order, vars map[string]string) error {
	missing := a.MissingRequiredParams(vars)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("order %q: missing required param(s): %s", a.ScopedName(), strings.Join(missing, ", "))
}
