package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// --- Duration reasonableness check ---

// DurationRangeCheck validates that duration fields in the config have
// reasonable values. Extremely small durations (< 100ms for timeouts) or
// extremely large ones (> 7 days for patrol intervals) are likely typos.
type DurationRangeCheck struct {
	cfg *config.City
}

// NewDurationRangeCheck creates a check for duration field reasonableness.
func NewDurationRangeCheck(cfg *config.City) *DurationRangeCheck {
	return &DurationRangeCheck{cfg: cfg}
}

// Name returns the check identifier.
func (c *DurationRangeCheck) Name() string { return "duration-range" }

// durationRange defines min/max bounds for a duration field.
type durationRange struct {
	context string
	field   string
	value   string
	min     time.Duration
	max     time.Duration
}

// Run checks all duration fields against reasonable bounds.
func (c *DurationRangeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	var issues []string

	ranges := c.collectRanges()
	for _, dr := range ranges {
		if dr.value == "" {
			continue
		}
		d, err := time.ParseDuration(dr.value)
		if err != nil {
			// ValidateDurations handles parse errors; skip here.
			continue
		}
		if d < dr.min {
			issues = append(issues, fmt.Sprintf(
				"%s %s = %q (%v) is below minimum %v",
				dr.context, dr.field, dr.value, d, dr.min))
		}
		if d > dr.max {
			issues = append(issues, fmt.Sprintf(
				"%s %s = %q (%v) exceeds maximum %v",
				dr.context, dr.field, dr.value, d, dr.max))
		}
	}

	if len(issues) == 0 {
		r.Status = StatusOK
		r.Message = "all durations within reasonable bounds"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d duration(s) outside reasonable bounds", len(issues))
	r.Details = issues
	return r
}

// collectRanges builds the list of (field, value, min, max) entries to check.
func (c *DurationRangeCheck) collectRanges() []durationRange {
	const (
		minTimeout  = 100 * time.Millisecond
		maxTimeout  = 1 * time.Hour
		minInterval = 100 * time.Millisecond
		maxInterval = 24 * time.Hour
		minWindow   = 1 * time.Minute
		maxWindow   = 7 * 24 * time.Hour // 7 days
		minTTL      = 1 * time.Minute
		maxTTL      = 30 * 24 * time.Hour // 30 days
	)

	var ranges []durationRange

	// Session config.
	ranges = append(ranges,
		durationRange{"[session]", "setup_timeout", c.cfg.Session.SetupTimeout, minTimeout, maxTimeout},
		durationRange{"[session]", "nudge_ready_timeout", c.cfg.Session.NudgeReadyTimeout, minTimeout, maxTimeout},
		durationRange{"[session]", "nudge_retry_interval", c.cfg.Session.NudgeRetryInterval, minInterval, maxTimeout},
		durationRange{"[session]", "nudge_lock_timeout", c.cfg.Session.NudgeLockTimeout, minTimeout, maxTimeout},
		durationRange{"[session]", "startup_timeout", c.cfg.Session.StartupTimeout, minTimeout, maxTimeout},
	)

	// Daemon config.
	ranges = append(ranges,
		durationRange{"[daemon]", "patrol_interval", c.cfg.Daemon.PatrolInterval, minInterval, maxInterval},
		durationRange{"[daemon]", "restart_window", c.cfg.Daemon.RestartWindow, minWindow, maxWindow},
		durationRange{"[daemon]", "shutdown_timeout", c.cfg.Daemon.ShutdownTimeout, minTimeout, maxTimeout},
		durationRange{"[daemon]", "wisp_gc_interval", c.cfg.Daemon.WispGCInterval, minInterval, maxInterval},
		durationRange{"[daemon]", "wisp_ttl", c.cfg.Daemon.WispTTL, minTTL, maxTTL},
		durationRange{"[daemon]", "drift_drain_timeout", c.cfg.Daemon.DriftDrainTimeout, minTimeout, maxTimeout},
	)

	// Per-agent durations.
	for _, a := range c.cfg.Agents {
		ctx := fmt.Sprintf("agent %q", a.QualifiedName())
		if a.IdleTimeout != "" {
			ranges = append(ranges,
				durationRange{ctx, "idle_timeout", a.IdleTimeout, minTimeout, maxWindow})
		}
		if a.DrainTimeout != "" {
			ranges = append(ranges,
				durationRange{ctx, "drain_timeout", a.DrainTimeout, minTimeout, maxTimeout})
		}
	}

	return ranges
}

// CanFix returns false — unreasonable durations must be corrected by the user.
func (c *DurationRangeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *DurationRangeCheck) Fix(_ *CheckContext) error { return nil }

// --- Event log size check ---

// EventLogSizeCheck warns when .gc/events.jsonl exceeds a size threshold.
// The event log grows unbounded; large files slow down reads and waste disk.
type EventLogSizeCheck struct {
	// MaxSize is the warning threshold in bytes. Defaults to 100 MB.
	MaxSize int64
}

// NewEventLogSizeCheck creates a check for event log size.
func NewEventLogSizeCheck() *EventLogSizeCheck {
	return &EventLogSizeCheck{MaxSize: 100 * 1024 * 1024} // 100 MB
}

// Name returns the check identifier.
func (c *EventLogSizeCheck) Name() string { return "events-log-size" }

// Run checks the size of events.jsonl.
func (c *EventLogSizeCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	path := filepath.Join(ctx.CityPath, ".gc", "events.jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		// File missing is OK — EventsLogCheck handles that.
		r.Status = StatusOK
		r.Message = "events.jsonl not present (nothing to check)"
		return r
	}

	size := fi.Size()
	if size <= c.MaxSize {
		r.Status = StatusOK
		r.Message = fmt.Sprintf("events.jsonl size: %s", humanSize(size))
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("events.jsonl is %s (exceeds %s threshold)",
		humanSize(size), humanSize(c.MaxSize))
	r.FixHint = "consider truncating or archiving .gc/events.jsonl"
	return r
}

// CanFix returns false — the user should decide how to handle large logs.
func (c *EventLogSizeCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *EventLogSizeCheck) Fix(_ *CheckContext) error { return nil }

// humanSize returns a human-readable file size string.
func humanSize(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// --- Config semantics check ---

// ConfigSemanticsCheck surfaces warnings from config.ValidateSemantics
// as doctor check results. This catches provider reference errors, bad
// enum values, and cross-field constraint violations.
type ConfigSemanticsCheck struct {
	cfg    *config.City
	source string
}

// NewConfigSemanticsCheck creates a check that runs semantic validation.
func NewConfigSemanticsCheck(cfg *config.City, source string) *ConfigSemanticsCheck {
	return &ConfigSemanticsCheck{cfg: cfg, source: source}
}

// Name returns the check identifier.
func (c *ConfigSemanticsCheck) Name() string { return "config-semantics" }

// Run executes ValidateSemantics and reports any warnings.
func (c *ConfigSemanticsCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	warnings := config.ValidateSemantics(c.cfg, c.source)
	if len(warnings) == 0 {
		r.Status = StatusOK
		r.Message = "config semantics valid"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf("%d config semantic warning(s)", len(warnings))
	r.Details = warnings
	return r
}

// CanFix returns false — semantic issues require manual config correction.
func (c *ConfigSemanticsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *ConfigSemanticsCheck) Fix(_ *CheckContext) error { return nil }
