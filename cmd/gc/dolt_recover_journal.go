package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// managedDoltRecoverCallerEnv names the diagnostic-only tag that records in
// the recovery journal which path asked for managed dolt recovery.
const managedDoltRecoverCallerEnv = "GC_DOLT_RECOVER_CALLER"

// Recovery caller tags written to the journal.
const (
	managedDoltRecoverCallerDirect         = "direct-helper"
	managedDoltRecoverCallerProviderHealth = "provider-health"
	managedDoltRecoverCallerBdRetry        = "bd-retry"
)

// Recovery journal actions.
const (
	managedDoltJournalDefer = "defer"
	managedDoltJournalStop  = "stop"
	// managedDoltJournalReset records a streak cleared by a measured healthy
	// observation.
	managedDoltJournalReset = "reset"
	// managedDoltJournalHistoryDiagnostic records recovery history that was
	// discarded or reset, or that could not be read or written.
	managedDoltJournalHistoryDiagnostic = "history-diagnostic"
	// managedDoltJournalConfigDiagnostic records an unresponsive window that
	// fell back to its default.
	managedDoltJournalConfigDiagnostic = "config-diagnostic"
)

// managedDoltJournalResetReason is the journaled reason for a streak reset;
// the provider cooldown is independent of the streak and survives it.
const managedDoltJournalResetReason = "healthy observation; provider cooldown preserved"

// managedDoltJournalNotApplicable marks a journal duration that recovery did
// not measure.
const managedDoltJournalNotApplicable time.Duration = -1

// managedDoltRecoveryJournalEntry is one deferral or stop decision recorded
// in the managed dolt log.
type managedDoltRecoveryJournalEntry struct {
	Action string
	Reason string
	// Elapsed and Window are managedDoltJournalNotApplicable when the
	// decision did not measure them.
	Elapsed time.Duration
	Window  time.Duration
	// Identity.PID is zero when no owned live listener was identified.
	Identity managedDoltServerIdentity
	Port     string
}

// managedDoltRecoverCaller returns the tag that the path which spawned this
// recovery helper process set in its environment, defaulting to a direct
// helper invocation. A path that journals from its own process, such as the
// provider health check, passes its own tag to the journal writers instead.
func managedDoltRecoverCaller() string {
	if caller := strings.TrimSpace(os.Getenv(managedDoltRecoverCallerEnv)); caller != "" {
		return caller
	}
	return managedDoltRecoverCallerDirect
}

// withManagedDoltRecoverCaller returns environ (or the process environment
// when environ is empty) tagged with caller for the recovery journal.
func withManagedDoltRecoverCaller(environ []string, caller string) []string {
	if len(environ) == 0 {
		environ = os.Environ()
	}
	tagged := make([]string, 0, len(environ)+1)
	tagged = append(tagged, environ...)
	return append(tagged, managedDoltRecoverCallerEnv+"="+caller)
}

func formatManagedDoltJournalDuration(d time.Duration) string {
	if d == managedDoltJournalNotApplicable {
		return "n/a"
	}
	return d.String()
}

// formatManagedDoltRecoveryJournalLine renders entry as one escaped,
// timestamped log line.
func formatManagedDoltRecoveryJournalLine(at time.Time, entry managedDoltRecoveryJournalEntry, caller string) string {
	port := strconv.Quote(entry.Port)
	if n, err := strconv.Atoi(entry.Port); err == nil {
		port = strconv.Itoa(n)
	}
	identity := "pid=n/a start_time=n/a port=" + port
	if entry.Identity.PID > 0 {
		identity = fmt.Sprintf("pid=%d start_time=%q port=%d", entry.Identity.PID, entry.Identity.StartTime, entry.Identity.Port)
	}
	return fmt.Sprintf("%s gc dolt recovery: action=%s reason=%q elapsed=%s window=%s %s caller=%q\n",
		at.UTC().Format(time.RFC3339Nano), entry.Action, entry.Reason,
		formatManagedDoltJournalDuration(entry.Elapsed), formatManagedDoltJournalDuration(entry.Window),
		identity, caller)
}

// appendManagedDoltRecoveryJournal appends entry, attributed to caller, to the
// managed dolt log at logFile.
func appendManagedDoltRecoveryJournal(logFile string, at time.Time, entry managedDoltRecoveryJournalEntry, caller string) error {
	return appendManagedDoltRecoveryLogLine(logFile, entry.Action, formatManagedDoltRecoveryJournalLine(at, entry, caller))
}

// formatManagedDoltRecoveryNoteLine renders a recovery note (a streak reset
// or a diagnostic) as one escaped, timestamped log line.
func formatManagedDoltRecoveryNoteLine(at time.Time, action, reason, caller string) string {
	return fmt.Sprintf("%s gc dolt recovery: action=%s reason=%q caller=%q\n",
		at.UTC().Format(time.RFC3339Nano), action, reason, caller)
}

// appendManagedDoltRecoveryNote appends a recovery note, attributed to
// caller, to the managed dolt log at logFile.
func appendManagedDoltRecoveryNote(logFile string, at time.Time, action, reason, caller string) error {
	return appendManagedDoltRecoveryLogLine(logFile, action, formatManagedDoltRecoveryNoteLine(at, action, reason, caller))
}

// reportManagedDoltRecoveryNote appends a recovery note, attributed to
// caller, to the managed dolt log at logFile. A journal failure is reported
// on diag rather than failing the recovery decision the note accompanies.
func reportManagedDoltRecoveryNote(diag io.Writer, logFile string, at time.Time, action, reason, caller string) {
	if err := appendManagedDoltRecoveryNote(logFile, at, action, reason, caller); err != nil {
		fmt.Fprintf(diag, "gc dolt recovery: %v\n", err) //nolint:errcheck // best-effort diagnostic
	}
}

// reportManagedDoltRecoveryDiagnostic writes a non-fatal recovery diagnostic
// to diag and journals it, attributed to caller, in the managed dolt log at
// logFile as action.
func reportManagedDoltRecoveryDiagnostic(diag io.Writer, logFile string, at time.Time, action, reason, caller string) {
	fmt.Fprintf(diag, "gc dolt recovery: %s\n", reason) //nolint:errcheck // best-effort diagnostic
	reportManagedDoltRecoveryNote(diag, logFile, at, action, reason, caller)
}

func appendManagedDoltRecoveryLogLine(logFile, action, line string) error {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		return fmt.Errorf("record managed dolt recovery %s in journal %s: %w", action, logFile, err)
	}
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("record managed dolt recovery %s in journal %s: %w", action, logFile, err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("record managed dolt recovery %s in journal %s: %w", action, logFile, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("record managed dolt recovery %s in journal %s: %w", action, logFile, err)
	}
	return nil
}
