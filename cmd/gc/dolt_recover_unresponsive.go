package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// resolveManagedDoltUnresponsiveRestartAfter returns how long recovery
// observes a live, listening managed dolt that fails SQL probes before it
// restarts that server, read from `[daemon].dolt_unresponsive_restart_after`.
// A config that cannot be loaded (including a rejected negative value) and a
// malformed value both resolve to the default; each such fallback is returned
// as a diagnostic so it is never silent.
func resolveManagedDoltUnresponsiveRestartAfter(cityPath string) (time.Duration, []string) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return config.DefaultDoltUnresponsiveRestartAfter, []string{
			fmt.Sprintf("load city config for dolt_unresponsive_restart_after: %v; using default %s", err, config.DefaultDoltUnresponsiveRestartAfter),
		}
	}
	var diagnostics []string
	for _, warning := range config.ValidateDurations(cfg, "city.toml") {
		if strings.Contains(warning, "dolt_unresponsive_restart_after") {
			diagnostics = append(diagnostics, fmt.Sprintf("%s; using default %s", warning, config.DefaultDoltUnresponsiveRestartAfter))
		}
	}
	return cfg.Daemon.DoltUnresponsiveRestartAfterDuration(), diagnostics
}

// managedDoltRecoveryDeferredError reports that recovery left an owned, live,
// listening managed dolt running because it has failed SQL observations for
// less than the configured unresponsive window.
type managedDoltRecoveryDeferredError struct {
	Identity managedDoltServerIdentity
	Elapsed  time.Duration
	Window   time.Duration
	Cause    error
}

func (e *managedDoltRecoveryDeferredError) Error() string {
	return fmt.Sprintf("managed dolt (pid %d, port %d) is live and listening but failed its SQL check (%v); deferring restart until it has been unresponsive for %s (so far %s)",
		e.Identity.PID, e.Identity.Port, e.Cause, e.Window, e.Elapsed.Round(time.Second))
}

func (e *managedDoltRecoveryDeferredError) Unwrap() error {
	return e.Cause
}

// managedDoltLiveListenerIdentity returns the identity of an owned, live
// managed dolt that holds a kernel listener on the requested port or on the
// port the city's runtime state reports after a rebind. Ownership is proven
// either by the process arguments/working directory or by this city's runtime
// state recording that PID on that port; a process with deleted data inodes
// or an unreadable start time is not eligible.
func managedDoltLiveListenerIdentity(cityPath string, layout managedDoltRuntimeLayout, port string) (managedDoltServerIdentity, bool) {
	ports := []string{strings.TrimSpace(port)}
	if observed := recoverManagedDoltObservedPort(cityPath, port); observed != "" {
		ports = append(ports, observed)
	}
	for _, candidate := range ports {
		if identity, ok := managedDoltOwnedListenerIdentity(cityPath, layout, candidate); ok {
			return identity, true
		}
	}
	return managedDoltServerIdentity{}, false
}

func managedDoltOwnedListenerIdentity(cityPath string, layout managedDoltRuntimeLayout, port string) (managedDoltServerIdentity, bool) {
	portNum, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || portNum <= 0 {
		return managedDoltServerIdentity{}, false
	}
	recorded := managedDoltRecordedPIDForPort(cityPath, layout, portNum)
	holder := findPortHolderPID(port, recorded)
	if holder <= 0 || !pidAlive(holder) {
		return managedDoltServerIdentity{}, false
	}
	var owned, deleted bool
	if holder == recorded {
		owned, deleted = true, processHasDeletedDataInodes(holder, layout.DataDir)
	} else {
		owned, deleted = inspectManagedDoltOwnership(holder, layout)
	}
	if !owned || deleted {
		return managedDoltServerIdentity{}, false
	}
	startTime, err := pidutil.StartTime(holder)
	if err != nil || strings.TrimSpace(startTime) == "" {
		return managedDoltServerIdentity{}, false
	}
	return managedDoltServerIdentity{PID: holder, StartTime: startTime, Port: portNum}, true
}

// managedDoltRecordedPIDForPort returns the PID this city's runtime state
// records as running its managed dolt on port, or 0.
func managedDoltRecordedPIDForPort(cityPath string, layout managedDoltRuntimeLayout, port int) int {
	for _, path := range []string{providerManagedDoltStatePath(cityPath), managedDoltStatePath(cityPath)} {
		state, err := readDoltRuntimeStateFile(path)
		if err != nil || !state.Running || state.PID <= 0 || state.Port != port {
			continue
		}
		if dataDir := strings.TrimSpace(state.DataDir); dataDir != "" && !samePath(dataDir, layout.DataDir) {
			continue
		}
		return state.PID
	}
	return 0
}

// gateManagedDoltUnresponsiveRestart decides whether a failed SQL observation
// (cause) may proceed to stopping the managed dolt. It returns a nil error and
// the stop's journal entry to proceed with the existing recovery path, a
// *managedDoltRecoveryDeferredError (journaled here) while
// an owned, live, listening server is inside the unresponsive window, or
// another error when the history cannot be read or written; neither error
// authorizes a stop. Callers hold the managed dolt lifecycle lock.
func gateManagedDoltUnresponsiveRestart(cityPath, port string, layout managedDoltRuntimeLayout, ops managedDoltRecoveryOps, cause error, report *managedDoltRecoverReport) (managedDoltRecoveryJournalEntry, error) {
	stop := managedDoltRecoveryJournalEntry{
		Action:  managedDoltJournalStop,
		Reason:  cause.Error(),
		Elapsed: managedDoltJournalNotApplicable,
		Port:    port,
	}
	window, configDiagnostics := ops.unresponsiveWindow(cityPath)
	for _, diagnostic := range configDiagnostics {
		reportManagedDoltRecoveryDiagnostic(ops.diag, layout.LogFile, ops.now(), managedDoltJournalConfigDiagnostic, diagnostic)
	}
	stop.Window = max(window, 0)
	if window <= 0 {
		return stop, nil
	}
	identity, live := ops.liveListener(cityPath, layout, port)
	if !live {
		return stop, nil
	}
	path := managedDoltRecoveryStatePath(layout)
	state, diagnostic, err := readManagedDoltRecoveryState(path)
	if err != nil {
		return stop, fmt.Errorf("managed dolt (pid %d, port %d) failed its SQL check (%w); not restarting without recovery history: %w", identity.PID, identity.Port, cause, err)
	}
	now := ops.now()
	if diagnostic != "" {
		reportManagedDoltRecoveryDiagnostic(ops.diag, layout.LogFile, now, managedDoltJournalHistoryDiagnostic, diagnostic)
	}
	streak, diagnostic := continueManagedDoltUnresponsiveStreak(state.Streak, identity, now, window)
	if diagnostic != "" {
		reportManagedDoltRecoveryDiagnostic(ops.diag, layout.LogFile, now, managedDoltJournalHistoryDiagnostic, diagnostic)
	}
	elapsed := now.Sub(streak.FirstFailedAt)
	if elapsed < window {
		return stop, deferManagedDoltRestart(layout, state, streak, window, cause, ops, report)
	}
	// The window has elapsed: consume the streak so no later observation can
	// reuse it, then confirm the same instance still holds the listener. A
	// replacement instance cannot inherit the elapsed window.
	state.Streak = nil
	if err := ops.writeHistory(path, state); err != nil {
		return stop, fmt.Errorf("managed dolt (pid %d, port %d) failed its SQL check (%w); not restarting without recovery history: %w", identity.PID, identity.Port, cause, err)
	}
	current, stillLive := ops.liveListener(cityPath, layout, port)
	if stillLive && current != identity {
		fresh := managedDoltUnresponsiveStreak{managedDoltServerIdentity: current, FirstFailedAt: now, LastFailedAt: now, Failures: 1}
		return stop, deferManagedDoltRestart(layout, state, fresh, window, cause, ops, report)
	}
	stop.Elapsed = elapsed
	stop.Identity = identity
	return stop, nil
}

// deferManagedDoltRestart persists streak, records the deferral in the
// managed dolt log and returns it. A journal failure is joined to the
// deferral so neither hides the other.
func deferManagedDoltRestart(layout managedDoltRuntimeLayout, state managedDoltRecoveryState, streak managedDoltUnresponsiveStreak, window time.Duration, cause error, ops managedDoltRecoveryOps, report *managedDoltRecoverReport) error {
	identity := streak.managedDoltServerIdentity
	state.Streak = &streak
	if err := ops.writeHistory(managedDoltRecoveryStatePath(layout), state); err != nil {
		return fmt.Errorf("managed dolt (pid %d, port %d) failed its SQL check (%w); not restarting without recovery history: %w", identity.PID, identity.Port, cause, err)
	}
	report.HadPID = true
	report.PID = identity.PID
	report.Port = identity.Port
	deferred := &managedDoltRecoveryDeferredError{
		Identity: identity,
		Elapsed:  streak.LastFailedAt.Sub(streak.FirstFailedAt),
		Window:   window,
		Cause:    cause,
	}
	entry := managedDoltRecoveryJournalEntry{
		Action:   managedDoltJournalDefer,
		Reason:   cause.Error(),
		Elapsed:  deferred.Elapsed,
		Window:   window,
		Identity: identity,
		Port:     strconv.Itoa(identity.Port),
	}
	if err := appendManagedDoltRecoveryJournal(layout.LogFile, ops.now(), entry); err != nil {
		return errors.Join(deferred, err)
	}
	return deferred
}

// managedDoltUnresponsiveStreakMinGap is the shortest observation gap that
// ends a streak; the effective limit is max(window, this).
const managedDoltUnresponsiveStreakMinGap = 5 * time.Minute

// continueManagedDoltUnresponsiveStreak extends prior with a failure observed
// at now, or starts a fresh streak when prior belongs to another instance,
// its timestamps are out of order or in the future, or the gap since its last
// failure exceeds max(window, 5m). Only a continued streak can carry elapsed
// time toward the window; the returned diagnostic explains a clock reset.
func continueManagedDoltUnresponsiveStreak(prior *managedDoltUnresponsiveStreak, identity managedDoltServerIdentity, now time.Time, window time.Duration) (managedDoltUnresponsiveStreak, string) {
	fresh := managedDoltUnresponsiveStreak{managedDoltServerIdentity: identity, FirstFailedAt: now, LastFailedAt: now, Failures: 1}
	if prior == nil || prior.managedDoltServerIdentity != identity {
		return fresh, ""
	}
	if prior.FirstFailedAt.After(prior.LastFailedAt) || prior.LastFailedAt.After(now) {
		return fresh, fmt.Sprintf("recorded unresponsive streak for pid %d (first %s, last %s) is out of order or ahead of now %s (clock moved backwards?); resetting it",
			prior.PID, prior.FirstFailedAt.Format(time.RFC3339), prior.LastFailedAt.Format(time.RFC3339), now.Format(time.RFC3339))
	}
	if now.Sub(prior.LastFailedAt) > max(window, managedDoltUnresponsiveStreakMinGap) {
		return fresh, ""
	}
	continued := *prior
	continued.LastFailedAt = now
	continued.Failures++
	return continued, ""
}

// managedDoltHistoryLockTimeout bounds how long a healthy observation that
// does not already hold the lifecycle lock waits for it to clear a streak.
var managedDoltHistoryLockTimeout = 10 * time.Second

// clearManagedDoltUnresponsiveStreakLocked removes a pending unresponsive
// streak after a measured healthy observation and preserves the provider
// cooldown. Callers hold the managed dolt lifecycle lock, which serializes
// the clear with failure writes so an older snapshot cannot restore the
// streak. A failed write is returned because the stale streak would stay
// readable. An unreadable history is reported on diag and in the managed
// dolt log rather than failing the healthy return: the gate refuses to stop
// on a history it cannot read, so that history can never authorize a
// restart. Each reset is journaled in the managed dolt log.
func clearManagedDoltUnresponsiveStreakLocked(layout managedDoltRuntimeLayout, write func(string, managedDoltRecoveryState) error, diag io.Writer) error {
	path := managedDoltRecoveryStatePath(layout)
	state, diagnostic, err := readManagedDoltRecoveryState(path)
	if err != nil {
		reportManagedDoltRecoveryDiagnostic(diag, layout.LogFile, time.Now(), managedDoltJournalHistoryDiagnostic,
			fmt.Sprintf("cannot clear the unresponsive streak after a healthy check: %v", err))
		return nil
	}
	if diagnostic != "" {
		reportManagedDoltRecoveryDiagnostic(diag, layout.LogFile, time.Now(), managedDoltJournalHistoryDiagnostic, diagnostic)
	} else if state.Streak == nil {
		return nil
	}
	hadStreak := state.Streak != nil
	state.Streak = nil
	if err := write(path, state); err != nil {
		return fmt.Errorf("clear managed dolt unresponsive streak after a healthy check: %w", err)
	}
	if hadStreak {
		reportManagedDoltRecoveryNote(diag, layout.LogFile, time.Now(), managedDoltJournalReset, managedDoltJournalResetReason)
	}
	return nil
}

// clearManagedDoltUnresponsiveStreak is clearManagedDoltUnresponsiveStreakLocked
// for callers that do not hold the lifecycle lock. With no visible streak
// there is nothing to clear and it returns without waiting; otherwise it
// waits up to managedDoltHistoryLockTimeout for the lock and reports a
// synchronization error rather than returning healthy over a stale streak.
func clearManagedDoltUnresponsiveStreak(layout managedDoltRuntimeLayout, write func(string, managedDoltRecoveryState) error, diag io.Writer) error {
	if state, diagnostic, err := readManagedDoltRecoveryState(managedDoltRecoveryStatePath(layout)); err == nil && diagnostic == "" && state.Streak == nil {
		return nil
	}
	lockFile, err := openManagedDoltLifecycleLockAt(layout)
	if err != nil {
		return fmt.Errorf("synchronize managed dolt recovery history: %w", err)
	}
	defer releaseManagedDoltLifecycleLock(lockFile)
	if err := acquireManagedDoltLifecycleLockWithin(lockFile, managedDoltHistoryLockTimeout); err != nil {
		return fmt.Errorf("synchronize managed dolt recovery history: %w", err)
	}
	return clearManagedDoltUnresponsiveStreakLocked(layout, write, diag)
}

// clearProviderManagedDoltUnresponsiveStreak clears the streak after a
// healthy provider check, using the strict layout whose paths the provider
// passes to its recovery helper through providerLifecycleDoltPathEnv, so both
// resolve the same history and lock.
func clearProviderManagedDoltUnresponsiveStreak(cityPath string) error {
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		return err
	}
	return clearManagedDoltUnresponsiveStreak(layout, writeManagedDoltRecoveryState, os.Stderr)
}

// reserveManagedDoltProviderRecoverAttempt reserves a provider-driven
// recovery attempt at now unless one was reserved less than cooldown ago.
// The reservation lives in the city's recovery history under the lifecycle
// lock, so separate provider-health processes share one cooldown; it is
// written before recovery runs and the lock is released before the caller
// spawns the recovery helper, which takes that lock itself. A reservation
// ahead of now (the clock moved backwards) is reset with a diagnostic. An
// unreadable or unwritable history returns an error and reserves nothing.
func reserveManagedDoltProviderRecoverAttempt(cityPath string, now time.Time, cooldown time.Duration, diag io.Writer) (bool, error) {
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		return false, err
	}
	lockFile, err := openManagedDoltLifecycleLockAt(layout)
	if err != nil {
		return false, fmt.Errorf("synchronize managed dolt recovery history: %w", err)
	}
	defer releaseManagedDoltLifecycleLock(lockFile)
	if err := acquireManagedDoltLifecycleLockWithin(lockFile, managedDoltHistoryLockTimeout); err != nil {
		return false, fmt.Errorf("synchronize managed dolt recovery history: %w", err)
	}
	path := managedDoltRecoveryStatePath(layout)
	state, diagnostic, err := readManagedDoltRecoveryState(path)
	if err != nil {
		return false, err
	}
	if diagnostic != "" {
		reportManagedDoltRecoveryDiagnostic(diag, layout.LogFile, now, managedDoltJournalHistoryDiagnostic, diagnostic)
	}
	if last := state.LastProviderAttemptAt; last != nil {
		if last.After(now) {
			reportManagedDoltRecoveryDiagnostic(diag, layout.LogFile, now, managedDoltJournalHistoryDiagnostic,
				fmt.Sprintf("recorded provider recovery attempt %s is ahead of now %s (clock moved backwards?); resetting it",
					last.Format(time.RFC3339), now.Format(time.RFC3339)))
		} else if now.Sub(*last) < cooldown {
			return false, nil
		}
	}
	state.LastProviderAttemptAt = &now
	if err := writeManagedDoltRecoveryState(path, state); err != nil {
		return false, fmt.Errorf("reserve provider recovery attempt: %w", err)
	}
	return true, nil
}
