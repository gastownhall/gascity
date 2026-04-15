package main

import (
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

	if err := managedDoltQueryProbe(host, port, user); err == nil {
		health, healthErr := managedDoltHealthCheck(host, port, user, true)
		if healthErr == nil && health.ReadOnly == "true" {
			report.DiagnosedReadOnly = true
		}
	}

	stopReport, stopErr := stopManagedDoltProcess(cityPath, port)
	report.HadPID = stopReport.HadPID
	report.Forced = stopReport.Forced
	if stopReport.PID > 0 {
		report.PID = stopReport.PID
	}
	// Match shell recover semantics: stop is best-effort before restart.
	_ = stopErr

	if err := preflightManagedDoltCleanup(cityPath); err != nil {
		return report, err
	}
	time.Sleep(time.Second)

	startReport, err := startManagedDoltProcess(cityPath, host, port, user, logLevel, timeout)
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
		return report, err
	}
	if health.ReadOnly == "true" {
		report.Healthy = false
		return report, fmt.Errorf("dolt server on %s:%d is still read-only after recovery", managedDoltConnectHost(host), report.Port)
	}
	report.Healthy = health.QueryReady
	if !report.Healthy {
		return report, fmt.Errorf("dolt server on %s:%d is not query-ready after recovery", managedDoltConnectHost(host), report.Port)
	}
	return report, nil
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
