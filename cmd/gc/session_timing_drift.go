package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sessionTimingKey is one [session] timing key that tmuxConfigFromSession
// copies into the base session provider when the provider is built, so a
// reload that changes it does not reach the running provider. value renders
// the resolved value (the accessor the provider is built from), never the raw
// TOML text, so a spelling that resolves to the running value is not a change.
type sessionTimingKey struct {
	name  string
	value func(config.SessionConfig) string
}

func durationValue(d time.Duration) string { return d.String() }

// sessionTimingKeys lists the frozen keys in the order a notice names them.
// A new provider-construction-time [session] field needs a row here; the
// structural guard TestSessionTimingReloadProviderFieldsHaveCoverage fails
// until it has one or an explicit non-timing exclusion.
var sessionTimingKeys = [...]sessionTimingKey{
	{"setup_timeout", func(sc config.SessionConfig) string { return durationValue(sc.SetupTimeoutDuration()) }},
	{"setup_max_timeout", func(sc config.SessionConfig) string { return durationValue(sc.SetupMaxTimeoutDuration()) }},
	{"nudge_ready_timeout", func(sc config.SessionConfig) string { return durationValue(sc.NudgeReadyTimeoutDuration()) }},
	{"nudge_retry_interval", func(sc config.SessionConfig) string { return durationValue(sc.NudgeRetryIntervalDuration()) }},
	{"nudge_lock_timeout", func(sc config.SessionConfig) string { return durationValue(sc.NudgeLockTimeoutDuration()) }},
	{"debounce_ms", func(sc config.SessionConfig) string { return strconv.Itoa(sc.DebounceMsOrDefault()) }},
	{"display_ms", func(sc config.SessionConfig) string { return strconv.Itoa(sc.DisplayMsOrDefault()) }},
}

// sessionTimingDrift reports that the [session] timing values on disk differ
// from the ones the running base session provider was built with. It latches
// those values together with the identity of the base provider instance, and
// re-latches only when a reload publishes a different base instance, which was
// built from the candidate's values. It keeps no reported flag: the notice
// repeats on every applied or no-change reload while a difference stands.
type sessionTimingDrift struct {
	running [len(sessionTimingKeys)]string
	base    runtime.Provider
}

// newSessionTimingDrift latches the timing values cfg resolves to and the base
// leg of sp, the provider that was built from cfg.
func newSessionTimingDrift(sp runtime.Provider, cfg *config.City) sessionTimingDrift {
	d := sessionTimingDrift{}
	d.latch(sp, cfg)
	return d
}

func (d *sessionTimingDrift) latch(sp runtime.Provider, cfg *config.City) {
	d.running = sessionTimingValues(cfg)
	d.base = sessionProviderLegs(sp).base
}

func sessionTimingValues(cfg *config.City) [len(sessionTimingKeys)]string {
	var sc config.SessionConfig
	if cfg != nil {
		sc = cfg.Session
	}
	var values [len(sessionTimingKeys)]string
	for i, key := range sessionTimingKeys {
		values[i] = key.value(sc)
	}
	return values
}

// sync re-latches to cfg's timing when the published provider sp carries a
// base instance other than the latched one. A carried base (the same instance)
// keeps the baseline: the provider still runs the values it was built with.
func (d *sessionTimingDrift) sync(sp runtime.Provider, cfg *config.City) {
	if sessionProviderLegs(sp).base != d.base {
		d.latch(sp, cfg)
	}
}

// observe returns the pending-restart warning for cfg, or "" when every
// resolved timing value equals the one the running base provider was built with.
func (d *sessionTimingDrift) observe(cfg *config.City) string {
	onDisk := sessionTimingValues(cfg)
	var rows []string
	for i, key := range sessionTimingKeys {
		if onDisk[i] != d.running[i] {
			rows = append(rows, fmt.Sprintf("%s is %s on disk but this controller runs %s", key.name, onDisk[i], d.running[i]))
		}
	}
	if len(rows) == 0 {
		return ""
	}
	tail := "the new value is"
	if len(rows) > 1 {
		tail = "the new values are"
	}
	return "pending restart: [session] " + strings.Join(rows, "; ") + "; " + tail + " not active until the supervisor restarts"
}

// report warns, through warn, about every difference between cfg and the
// running provider's timing. warn is called at most once.
func (d *sessionTimingDrift) report(cfg *config.City, warn func(string)) {
	if warning := d.observe(cfg); warning != "" {
		warn(warning)
	}
}

// publishAndReport is report for a reload that has just published sp: the
// baseline follows a newly published base instance first.
func (d *sessionTimingDrift) publishAndReport(sp runtime.Provider, cfg *config.City, warn func(string)) {
	d.sync(sp, cfg)
	d.report(cfg, warn)
}
