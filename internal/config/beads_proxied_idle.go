package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ProxiedIdleTimeoutEnv overrides every configured proxied idle timeout. It is
// a test and diagnosis knob: unlike the config fields it may go below
// MinProxiedIdleTimeout, and doctor reports it whenever it is set.
const ProxiedIdleTimeoutEnv = "GC_BEADS_PROXIED_IDLE_TIMEOUT"

// MinProxiedIdleTimeout is the smallest finite idle timeout config accepts.
// Below it gc's own polling cadence and bd's sampled idle watcher turn the
// scope into continuous proxy churn.
const MinProxiedIdleTimeout = time.Minute

// DefaultProxiedIdleTimeout is the idle timeout a scope gets when neither the
// city nor its rig sets one. Zero means never.
const DefaultProxiedIdleTimeout time.Duration = 0

// ProxiedIdleTimeoutSource names where a resolved idle timeout came from.
type ProxiedIdleTimeoutSource string

// Proxied idle timeout sources, in increasing precedence.
const (
	ProxiedIdleTimeoutSourceDefault ProxiedIdleTimeoutSource = "default"
	ProxiedIdleTimeoutSourceCity    ProxiedIdleTimeoutSource = "city"
	ProxiedIdleTimeoutSourceRig     ProxiedIdleTimeoutSource = "rig"
	ProxiedIdleTimeoutSourceEnv     ProxiedIdleTimeoutSource = "env"
)

// ProxiedIdleTimeout is the resolved idle timeout for one bd-owned proxied
// scope: how long its proxy and Dolt child stay up with no connections before
// bd retires them. A zero Duration means never.
type ProxiedIdleTimeout struct {
	Duration time.Duration
	Source   ProxiedIdleTimeoutSource
}

// Never reports whether the scope's proxy stays resident until stopped.
func (t ProxiedIdleTimeout) Never() bool { return t.Duration <= 0 }

// BdFlagValue renders the timeout for bd's --proxied-server-idle-timeout and
// --idle-timeout flags: "0" for never (bd persists it as -1ns), otherwise the
// Go duration (bd persists it as nanoseconds).
func (t ProxiedIdleTimeout) BdFlagValue() string {
	if t.Never() {
		return "0"
	}
	return t.Duration.String()
}

// String renders the timeout for diagnostics: "never" or the duration.
func (t ProxiedIdleTimeout) String() string {
	if t.Never() {
		return "never"
	}
	return t.Duration.String()
}

// ParseProxiedIdleTimeout parses a configured idle timeout. "0" means never;
// a finite value must be at least MinProxiedIdleTimeout.
func ParseProxiedIdleTimeout(value string) (time.Duration, error) {
	d, err := parseProxiedIdleDuration(value)
	if err != nil {
		return 0, err
	}
	if d > 0 && d < MinProxiedIdleTimeout {
		return 0, fmt.Errorf("%q must be 0 (never) or at least %s", value, MinProxiedIdleTimeout)
	}
	return d, nil
}

func parseProxiedIdleDuration(value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid duration: %w", value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q must not be negative", value)
	}
	return d, nil
}

// ProxiedIdleTimeoutFor resolves the idle timeout for a scope. Precedence is
// the GC_BEADS_PROXIED_IDLE_TIMEOUT environment override, then the rig's
// beads_proxied_idle_timeout, then the city's [beads] proxied_idle_timeout,
// then DefaultProxiedIdleTimeout. Pass a nil rig for the city scope and for a
// rig that shares the city's proxy root: one proxy serves every scope on a
// root, so they must all carry the city's value.
func ProxiedIdleTimeoutFor(city *City, rig *Rig) (ProxiedIdleTimeout, error) {
	if env := strings.TrimSpace(os.Getenv(ProxiedIdleTimeoutEnv)); env != "" {
		d, err := parseProxiedIdleDuration(env)
		if err != nil {
			return ProxiedIdleTimeout{}, fmt.Errorf("%s: %w", ProxiedIdleTimeoutEnv, err)
		}
		return ProxiedIdleTimeout{Duration: d, Source: ProxiedIdleTimeoutSourceEnv}, nil
	}
	if rig != nil && rig.BeadsProxiedIdleTimeout != nil {
		d, err := ParseProxiedIdleTimeout(*rig.BeadsProxiedIdleTimeout)
		if err != nil {
			return ProxiedIdleTimeout{}, fmt.Errorf("rig %q beads_proxied_idle_timeout: %w", rig.Name, err)
		}
		return ProxiedIdleTimeout{Duration: d, Source: ProxiedIdleTimeoutSourceRig}, nil
	}
	if city != nil && strings.TrimSpace(city.Beads.ProxiedIdleTimeout) != "" {
		d, err := ParseProxiedIdleTimeout(city.Beads.ProxiedIdleTimeout)
		if err != nil {
			return ProxiedIdleTimeout{}, fmt.Errorf("[beads] proxied_idle_timeout: %w", err)
		}
		return ProxiedIdleTimeout{Duration: d, Source: ProxiedIdleTimeoutSourceCity}, nil
	}
	return ProxiedIdleTimeout{Duration: DefaultProxiedIdleTimeout, Source: ProxiedIdleTimeoutSourceDefault}, nil
}

// ValidateProxiedIdleTimeouts reports, as load warnings, a configured proxied
// idle timeout that does not parse, is negative, or is finite and below
// MinProxiedIdleTimeout. They are warnings rather than load errors so a bad
// value cannot stop the rest of the city from loading — `gc stop` in
// particular must still retire the city's processes. Strict loads treat them
// as fatal like any other config warning, and initializing a scope with a bad
// value fails in ProxiedIdleTimeoutFor.
func ValidateProxiedIdleTimeouts(cfg *City, source string) []string {
	if cfg == nil {
		return nil
	}
	var warnings []string
	if v := cfg.Beads.ProxiedIdleTimeout; strings.TrimSpace(v) != "" {
		if _, err := ParseProxiedIdleTimeout(v); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: [beads] proxied_idle_timeout: %v", source, err))
		}
	}
	for _, r := range cfg.Rigs {
		if r.BeadsProxiedIdleTimeout == nil {
			continue
		}
		if _, err := ParseProxiedIdleTimeout(*r.BeadsProxiedIdleTimeout); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: rig %q beads_proxied_idle_timeout: %v", source, r.Name, err))
		}
	}
	return warnings
}
