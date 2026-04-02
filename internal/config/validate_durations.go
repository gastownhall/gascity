package config

import (
	"fmt"
	"time"
)

// ValidateDurations checks all duration string fields in the config and returns
// warnings for any values that cannot be parsed by time.ParseDuration. This
// catches typos like "5mins" (should be "5m") at config load time rather than
// silently defaulting to zero at runtime.
func ValidateDurations(cfg *City, source string) []string {
	var warnings []string
	check := func(context, field, value string) {
		if value == "" {
			return
		}
		if _, err := time.ParseDuration(value); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s %s = %q is not a valid duration: %v",
				source, context, field, value, err))
		}
	}
	checkSleep := func(context, field, value string) {
		if value == "" {
			return
		}
		if _, _, err := ParseSleepAfterIdle(value); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s %s = %q is not a valid duration or %q: %v",
				source, context, field, value, SessionSleepOff, err))
		}
	}

	// Session config durations.
	check("[session]", "setup_timeout", cfg.Session.SetupTimeout)
	check("[session]", "nudge_ready_timeout", cfg.Session.NudgeReadyTimeout)
	check("[session]", "nudge_retry_interval", cfg.Session.NudgeRetryInterval)
	check("[session]", "nudge_lock_timeout", cfg.Session.NudgeLockTimeout)
	check("[session]", "startup_timeout", cfg.Session.StartupTimeout)

	// Daemon config durations.
	check("[daemon]", "patrol_interval", cfg.Daemon.PatrolInterval)
	check("[daemon]", "restart_window", cfg.Daemon.RestartWindow)
	check("[daemon]", "shutdown_timeout", cfg.Daemon.ShutdownTimeout)
	check("[daemon]", "wisp_gc_interval", cfg.Daemon.WispGCInterval)
	check("[daemon]", "wisp_ttl", cfg.Daemon.WispTTL)
	check("[daemon]", "drift_drain_timeout", cfg.Daemon.DriftDrainTimeout)

	// Orders config durations.
	check("[orders]", "max_timeout", cfg.Orders.MaxTimeout)

	// Chat sessions config durations.
	check("[chat_sessions]", "idle_timeout", cfg.ChatSessions.IdleTimeout)

	// Session sleep config durations.
	checkSleep("[session_sleep]", "interactive_resume", cfg.SessionSleep.InteractiveResume)
	checkSleep("[session_sleep]", "interactive_fresh", cfg.SessionSleep.InteractiveFresh)
	checkSleep("[session_sleep]", "noninteractive", cfg.SessionSleep.NonInteractive)

	for _, r := range cfg.Rigs {
		ctx := fmt.Sprintf("rig %q [session_sleep]", r.Name)
		checkSleep(ctx, "interactive_resume", r.SessionSleep.InteractiveResume)
		checkSleep(ctx, "interactive_fresh", r.SessionSleep.InteractiveFresh)
		checkSleep(ctx, "noninteractive", r.SessionSleep.NonInteractive)
	}

	// Per-agent durations.
	for _, a := range cfg.Agents {
		ctx := fmt.Sprintf("agent %q", a.QualifiedName())
		check(ctx, "idle_timeout", a.IdleTimeout)
		checkSleep(ctx, "sleep_after_idle", a.SleepAfterIdle)
		check(ctx, "drain_timeout", a.DrainTimeout)
	}

	return warnings
}
