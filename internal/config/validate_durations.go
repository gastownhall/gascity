package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	setupTimeoutMaskedByStartupWarningFragment   = "setup_timeout can never fire because startup_timeout bounds the whole session Start() call first"
	setupTimeoutBecomesIdleBudgetWarningFragment = "setup_timeout now bounds idle/silence time between output, not total setup runtime"
)

// IsSessionSetupTimeoutAdvisory reports whether warning is one of the
// [session] setup-timeout advisories. They describe how the timeout knobs
// reinterpret each other rather than a config the loader cannot honor: the
// setup_max_timeout advisory fires on every config that sets the field,
// including a deliberately correct one. Callers that promote warnings to
// errors must keep these advisory, or setting a documented field would stop a
// city from starting.
// The fragments are matched as a suffix rather than anywhere in the string:
// the unparseable-duration warnings in this file quote the operator's raw
// value verbatim and then append the parse error, so a value that happens to
// contain an advisory sentence must not be mistaken for the advisory itself
// and downgraded out of strict-fatal handling.
func IsSessionSetupTimeoutAdvisory(warning string) bool {
	return strings.HasSuffix(warning, setupTimeoutMaskedByStartupWarningFragment) ||
		strings.HasSuffix(warning, setupTimeoutBecomesIdleBudgetWarningFragment)
}

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
	checkPositiveWithDays := func(context, field, value string) {
		if value == "" {
			return
		}
		dur, err := parseConfigDurationWithDays(value)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s %s = %q is not a valid duration: %v",
				source, context, field, value, err))
			return
		}
		if dur <= 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s %s = %q must be a positive duration",
				source, context, field, value))
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
	// checkPositive warns on both unparseable and non-positive values. Used for
	// knobs where a zero or negative duration silently reverts to a default at
	// runtime instead of failing loudly (e.g. an order override's
	// check_timeout).
	checkPositive := func(context, field, value string) {
		if value == "" {
			return
		}
		dur, err := time.ParseDuration(value)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s %s = %q is not a valid duration: %v",
				source, context, field, value, err))
			return
		}
		if dur <= 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%s: %s %s = %q must be a positive duration",
				source, context, field, value))
		}
	}

	// Session config durations.
	check("[session]", "setup_timeout", cfg.Session.SetupTimeout)
	check("[session]", "setup_max_timeout", cfg.Session.SetupMaxTimeout)
	check("[session]", "nudge_ready_timeout", cfg.Session.NudgeReadyTimeout)
	check("[session]", "nudge_retry_interval", cfg.Session.NudgeRetryInterval)
	check("[session]", "nudge_poll_interval", cfg.Session.NudgePollInterval)
	check("[session]", "nudge_lock_timeout", cfg.Session.NudgeLockTimeout)
	check("[session]", "startup_timeout", cfg.Session.StartupTimeout)
	check("[session]", "progress_stall_timeout", cfg.Session.ProgressStallTimeout)
	check("[session]", "claim_holder_stall_timeout", cfg.Session.ClaimHolderStallTimeout)

	// Cross-field: startup_timeout wraps the whole Start() call (pre_start and
	// setup included), so a setup_timeout that is >= startup_timeout can never
	// actually fire — startup_timeout kills Start() first. Compared as
	// effective (defaulted) durations so an unset field that inherits a
	// conflicting default is still caught (gastownhall/gascity#5279).
	if setupDur, startupDur := cfg.Session.SetupTimeoutDuration(), cfg.Session.StartupTimeoutDuration(); setupDur >= startupDur {
		warnings = append(warnings, fmt.Sprintf(
			"%s: [session] setup_timeout (%s) >= startup_timeout (%s): %s",
			source, setupDur, startupDur, setupTimeoutMaskedByStartupWarningFragment))
	}

	// setup_max_timeout > 0 silently reinterprets setup_timeout from a
	// total-runtime budget into an idle/silence budget (see runSetupCommand in
	// internal/runtime/tmux/adapter.go) — easy to miss when only setup_timeout
	// is being tuned (gastownhall/gascity#5279).
	if maxDur := cfg.Session.SetupMaxTimeoutDuration(); maxDur > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s: [session] setup_max_timeout (%s) is set: %s",
			source, maxDur, setupTimeoutBecomesIdleBudgetWarningFragment))
	}

	// Daemon config durations.
	check("[daemon]", "patrol_interval", cfg.Daemon.PatrolInterval)
	check("[daemon]", "restart_window", cfg.Daemon.RestartWindow)
	check("[daemon]", "session_circuit_breaker_window", cfg.Daemon.SessionCircuitBreakerWindow)
	check("[daemon]", "session_circuit_breaker_reset_after", cfg.Daemon.SessionCircuitBreakerResetAfter)
	check("[daemon]", "shutdown_timeout", cfg.Daemon.ShutdownTimeout)
	check("[daemon]", "wisp_gc_interval", cfg.Daemon.WispGCInterval)
	check("[daemon]", "wisp_ttl", cfg.Daemon.WispTTL)
	check("[daemon]", "drift_drain_timeout", cfg.Daemon.DriftDrainTimeout)
	check("[daemon]", "start_ready_timeout", cfg.Daemon.StartReadyTimeout)
	check("[daemon]", "dolt_stop_timeout", cfg.Daemon.DoltStopTimeout)
	check("[daemon]", "dolt_start_address_in_use_retry_window", cfg.Daemon.DoltStartAddressInUseRetryWindow)
	check("[dolt]", "dolt_lock_release_timeout", cfg.Dolt.DoltLockReleaseTimeout)

	// Orders config durations.
	check("[orders]", "max_timeout", cfg.Orders.MaxTimeout)
	for i := range cfg.Orders.Overrides {
		ov := cfg.Orders.Overrides[i]
		if ov.CheckTimeout != nil {
			// A non-positive check_timeout override parses cleanly but reverts
			// the condition probe to the 10s default at dispatch, so surface it
			// at config load like an unparseable typo.
			checkPositive(
				fmt.Sprintf("[[orders.overrides]] %q", ov.Name),
				"check_timeout", *ov.CheckTimeout)
		}
	}

	// Mail config durations.
	check("[mail]", "retention_ttl", cfg.Mail.RetentionTTL)

	// Events config durations.
	check("[events.rotation]", "archive_retain_age", cfg.Events.Rotation.ArchiveRetainAge)

	for name, policy := range cfg.Beads.Policies {
		checkPositiveWithDays(fmt.Sprintf("[beads.policies.%s]", name), "delete_after_close", policy.DeleteAfterClose)
		if !ValidBeadPolicyStorage(policy.Storage) {
			warnings = append(warnings, fmt.Sprintf(
				"%s: [beads.policies.%s] storage = %q is not valid: must be one of %q, %q, or %q",
				source, name, policy.Storage, BeadStorageHistory, BeadStorageNoHistory, BeadStorageEphemeral))
		}
	}

	// Chat sessions config durations.
	check("[chat_sessions]", "idle_timeout", cfg.ChatSessions.IdleTimeout)
	check("[chat_sessions]", "grace_period", cfg.ChatSessions.GracePeriod)

	// Maintenance (dolt) config durations.
	check("[maintenance.dolt]", "interval", cfg.Maintenance.Dolt.Interval)
	check("[maintenance.dolt]", "gc_timeout", cfg.Maintenance.Dolt.GCTimeout)

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

	for _, monitor := range cfg.GitHub.PRMonitors {
		ctx := fmt.Sprintf("github.pr_monitor %q", monitor.Name)
		check(ctx, "poll_interval", monitor.PollInterval)
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

// ValidateNonNegativeDurations checks duration fields that must not be negative
// and retention fields that must be positive, returning a hard error for the
// first violation. Unlike
// ValidateDurations (which only warns on unparseable typos), a negative
// duration that parses cleanly is silently destructive — e.g. a negative
// dolt_stop_timeout collapses the managed-dolt SIGTERM→SIGKILL grace to an
// immediate kill, risking journal corruption (gastownhall/gascity#2090).
// Such values are rejected at config load rather than at runtime.
//
// Empty and unparseable values are left to ValidateDurations; this function
// only rejects values that parse to a negative time.Duration.
func ValidateNonNegativeDurations(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	checkNonNegative := func(context, field, value string) error {
		if value == "" {
			return nil
		}
		dur, err := time.ParseDuration(value)
		if err != nil {
			// Parse errors are reported as warnings by ValidateDurations.
			return nil
		}
		if dur < 0 {
			return fmt.Errorf("%s: %s %s must not be negative: got %q",
				source, context, field, value)
		}
		return nil
	}
	checkPositiveWithDays := func(context, field, value string) error {
		if value == "" {
			return nil
		}
		dur, err := parseConfigDurationWithDays(value)
		if err != nil {
			return fmt.Errorf("%s: %s %s = %q is not a valid duration: %w",
				source, context, field, value, err)
		}
		if dur <= 0 {
			return fmt.Errorf("%s: %s %s must be a positive duration: got %q",
				source, context, field, value)
		}
		return nil
	}

	if err := checkNonNegative("[daemon]", "dolt_stop_timeout", cfg.Daemon.DoltStopTimeout); err != nil {
		return err
	}
	if err := checkNonNegative("[daemon]", "dolt_start_address_in_use_retry_window", cfg.Daemon.DoltStartAddressInUseRetryWindow); err != nil {
		return err
	}
	if err := checkNonNegative("[dolt]", "dolt_lock_release_timeout", cfg.Dolt.DoltLockReleaseTimeout); err != nil {
		return err
	}
	for name, policy := range cfg.Beads.Policies {
		if err := checkPositiveWithDays(fmt.Sprintf("[beads.policies.%s]", name), "delete_after_close", policy.DeleteAfterClose); err != nil {
			return err
		}
	}
	return nil
}

// ValidateEventsRotation returns non-fatal warnings for risky but intentional
// events rotation settings.
func ValidateEventsRotation(cfg *City) []string {
	if cfg == nil {
		return nil
	}
	raw := cfg.Events.Rotation.ArchiveRetainAge
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d >= 168*time.Hour {
		return nil
	}
	return []string{fmt.Sprintf("events.rotation: warning: archive_retain_age=%s may delete recent archives", raw)}
}
