package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type managedDoltRecoverReport struct {
	DiagnosedReadOnly bool
	HadPID            bool
	Forced            bool
	Ready             bool
	PID               int
	Port              int
	Healthy           bool
}

func recoverManagedDoltProcess(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltRecoverReport, error) {
	if strings.TrimSpace(cityPath) == "" {
		return managedDoltRecoverReport{}, fmt.Errorf("missing city path")
	}
	if strings.TrimSpace(port) == "" {
		return managedDoltRecoverReport{}, fmt.Errorf("missing port")
	}
	if strings.TrimSpace(host) == "" {
		host = "0.0.0.0"
	}
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if strings.TrimSpace(logLevel) == "" {
		logLevel = "warning"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	report := managedDoltRecoverReport{}

	lockFile, _, waitedForLifecycleLock, err := acquireManagedDoltLifecycleLock(cityPath)
	if err != nil {
		return report, cleanupFailedManagedDoltRecovery(cityPath, report.PID, report.Port, err)
	}
	defer releaseManagedDoltLifecycleLock(lockFile)

	if parsedPort, parseErr := strconv.Atoi(strings.TrimSpace(port)); parseErr == nil {
		report.Port = parsedPort
	}

	if waitedForLifecycleLock {
		if existing, err := assessExistingManagedDolt(cityPath, host, port, user, timeout); err == nil {
			if existing.ManagedPID > 0 {
				report.HadPID = true
				report.PID = existing.ManagedPID
			}
			if existing.StatePort > 0 {
				report.Port = existing.StatePort
			}
			if existing.Reusable && existing.StatePort > 0 {
				health, healthErr := managedDoltHealthCheck(host, strconv.Itoa(existing.StatePort), user, true)
				if healthErr == nil {
					if health.ReadOnly == "true" {
						report.DiagnosedReadOnly = true
					} else if health.QueryReady {
						report.Ready = true
						report.Healthy = true
						if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
							return report, fmt.Errorf("publish managed dolt runtime state: %w", err)
						}
						return report, nil
					}
				}
			}
		}
	}

	if err := managedDoltQueryProbe(host, port, user); err == nil {
		health, healthErr := managedDoltHealthCheck(host, port, user, true)
		if healthErr == nil && health.ReadOnly == "true" {
			report.DiagnosedReadOnly = true
		}
	}

	stopReport, stopErr := stopManagedDoltProcessWithOptions(cityPath, port, false)
	report.HadPID = stopReport.HadPID
	report.Forced = stopReport.Forced
	if stopReport.PID > 0 {
		report.PID = stopReport.PID
	}
	// Match shell recover semantics: stop is best-effort before restart.
	_ = stopErr

	if err := managedDoltPreflightCleanupFn(cityPath); err != nil {
		return report, cleanupFailedManagedDoltRecovery(cityPath, report.PID, report.Port, err)
	}
	time.Sleep(time.Second)

	startReport, err := startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel, timeout, false)
	report.Ready = startReport.Ready
	if startReport.PID > 0 {
		report.PID = startReport.PID
	}
	if startReport.Port > 0 {
		report.Port = startReport.Port
	} else if portNum, parseErr := strconv.Atoi(strings.TrimSpace(port)); parseErr == nil {
		report.Port = portNum
	}
	if err != nil {
		return report, err
	}

	health, err := managedDoltHealthCheck(host, strconv.Itoa(report.Port), user, true)
	if err != nil {
		return report, cleanupFailedManagedDoltRecovery(cityPath, report.PID, report.Port, err)
	}
	if health.ReadOnly == "true" {
		report.Healthy = false
		return report, cleanupFailedManagedDoltRecovery(cityPath, report.PID, report.Port, fmt.Errorf("dolt server on %s:%d is still read-only after recovery", managedDoltConnectHost(host), report.Port))
	}
	report.Healthy = health.QueryReady
	if !report.Healthy {
		return report, cleanupFailedManagedDoltRecovery(cityPath, report.PID, report.Port, fmt.Errorf("dolt server on %s:%d is not query-ready after recovery", managedDoltConnectHost(host), report.Port))
	}
	if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
		return report, cleanupFailedManagedDoltRecovery(cityPath, report.PID, report.Port, fmt.Errorf("publish managed dolt runtime state: %w", err))
	}
	return report, nil
}

func cleanupFailedManagedDoltRecovery(cityPath string, pid, port int, cause error) error {
	if cause == nil {
		return nil
	}
	cleanupErrs := make([]error, 0, 3)
	if pid > 0 {
		if err := terminateManagedDoltPID(pid); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("cleanup failed: %w", err))
		}
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		cleanupErrs = append(cleanupErrs, err)
	} else {
		portText := ""
		if port > 0 {
			portText = strconv.Itoa(port)
		}
		if err := clearManagedDoltRuntime(layout, portText); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	if len(cleanupErrs) == 0 {
		return cause
	}
	joined := append([]error{cause}, cleanupErrs...)
	return errors.Join(joined...)
}

func managedDoltRecoverFields(report managedDoltRecoverReport) []string {
	return []string{
		"diagnosed_read_only\t" + strconv.FormatBool(report.DiagnosedReadOnly),
		"had_pid\t" + strconv.FormatBool(report.HadPID),
		"forced\t" + strconv.FormatBool(report.Forced),
		"ready\t" + strconv.FormatBool(report.Ready),
		"pid\t" + strconv.Itoa(report.PID),
		"port\t" + strconv.Itoa(report.Port),
		"healthy\t" + strconv.FormatBool(report.Healthy),
	}
}
