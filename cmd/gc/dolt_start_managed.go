package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type managedDoltStartReport struct {
	Ready        bool
	PID          int
	Port         int
	AddressInUse bool
	Attempts     int
}

func startManagedDoltProcess(cityPath, host, port, user, logLevel string, timeout time.Duration) (managedDoltStartReport, error) {
	return startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel, timeout, true)
}

func startManagedDoltProcessWithOptions(cityPath, host, port, user, logLevel string, timeout time.Duration, publish bool) (managedDoltStartReport, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return managedDoltStartReport{}, err
	}
	portNum, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || portNum <= 0 {
		return managedDoltStartReport{}, fmt.Errorf("invalid port %q", port)
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

	report := managedDoltStartReport{}
	currentPort := portNum
	for attempt := 1; attempt <= 5; attempt++ {
		report.Attempts = attempt
		report.AddressInUse = false

		if err := managedDoltPreflightCleanupFn(cityPath); err != nil {
			return report, err
		}
		if err := writeManagedDoltConfigFile(layout.ConfigFile, host, strconv.Itoa(currentPort), layout.DataDir, logLevel); err != nil {
			return report, err
		}

		logOffset, err := managedDoltLogSize(layout.LogFile)
		if err != nil {
			return report, err
		}

		logFile, err := os.OpenFile(layout.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return report, fmt.Errorf("open log file: %w", err)
		}

		cmd := exec.Command("dolt", "sql-server", "--config", layout.ConfigFile)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = doltServerEnv(os.Environ())
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			return report, fmt.Errorf("start dolt sql-server: %w", err)
		}
		_ = logFile.Close()

		report.PID = cmd.Process.Pid
		report.Port = currentPort
		if err := os.MkdirAll(filepath.Dir(layout.PIDFile), 0o755); err != nil {
			_ = terminateManagedDoltPID(cmd.Process.Pid)
			return report, fmt.Errorf("create pid dir: %w", err)
		}
		if err := os.WriteFile(layout.PIDFile, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
			_ = terminateManagedDoltPID(cmd.Process.Pid)
			return report, fmt.Errorf("write pid file: %w", err)
		}
		if err := writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
			Running:   true,
			PID:       cmd.Process.Pid,
			Port:      currentPort,
			DataDir:   layout.DataDir,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			_ = terminateManagedDoltPID(cmd.Process.Pid)
			_ = os.Remove(layout.PIDFile)
			return report, fmt.Errorf("write provider state: %w", err)
		}

		readyReport, readyErr := waitForManagedDoltReady(cityPath, host, strconv.Itoa(currentPort), user, cmd.Process.Pid, timeout, false)
		if readyErr == nil && readyReport.Ready {
			report.Ready = true
			if publish {
				if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
					return report, fmt.Errorf("publish managed dolt runtime state: %w", err)
				}
			}
			return report, nil
		}

		if readyReport.PIDAlive {
			_ = terminateManagedDoltPID(cmd.Process.Pid)
			_ = os.Remove(layout.PIDFile)
			_ = writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
				Running:   false,
				PID:       0,
				Port:      currentPort,
				DataDir:   layout.DataDir,
				StartedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return report, fmt.Errorf("dolt server started (pid %d) but did not become query-ready within %s (check %s)", cmd.Process.Pid, timeout, layout.LogFile)
		}

		_ = os.Remove(layout.PIDFile)
		_ = writeDoltRuntimeStateFile(layout.StateFile, doltRuntimeState{
			Running:   false,
			PID:       0,
			Port:      currentPort,
			DataDir:   layout.DataDir,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})

		startupOutput, readErr := managedDoltLogSuffix(layout.LogFile, logOffset)
		if readErr == nil && strings.Contains(strings.ToLower(startupOutput), "address already in use") {
			report.AddressInUse = true
			currentPort = nextAvailableManagedDoltPort(currentPort + 1)
			report.Port = currentPort
			continue
		}
		if readyErr != nil {
			return report, fmt.Errorf("dolt server exited during startup: %w", readyErr)
		}
		return report, fmt.Errorf("dolt server exited during startup (check %s)", layout.LogFile)
	}

	return report, fmt.Errorf("dolt server could not find a free port after repeated address-in-use failures (last port %d)", report.Port)
}

func managedDoltStartFields(report managedDoltStartReport) []string {
	return []string{
		"ready\t" + strconv.FormatBool(report.Ready),
		"pid\t" + strconv.Itoa(report.PID),
		"port\t" + strconv.Itoa(report.Port),
		"address_in_use\t" + strconv.FormatBool(report.AddressInUse),
		"attempts\t" + strconv.Itoa(report.Attempts),
	}
}

func managedDoltLogSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

func managedDoltLogSuffix(path string, offset int64) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if offset >= int64(len(data)) {
		return "", nil
	}
	if offset < 0 {
		offset = 0
	}
	return string(data[offset:]), nil
}

func terminateManagedDoltPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = process.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = process.Signal(syscall.SIGKILL)
	time.Sleep(250 * time.Millisecond)
	return nil
}

// doltServerEnv augments the parent environment with overrides we need
// applied to every managed dolt sql-server we launch. Currently it
// disables Dolt's load-average auto-GC scheduler, which on multi-core
// hosts (>~16 CPUs) silently prevents auto-GC from ever running. See
// https://github.com/dolthub/dolt/issues/10944. Users who explicitly
// set DOLT_GC_SCHEDULER are respected.
func doltServerEnv(parent []string) []string {
	const key = "DOLT_GC_SCHEDULER"
	prefix := key + "="
	for _, kv := range parent {
		if strings.HasPrefix(kv, prefix) {
			return parent
		}
	}
	return append(append([]string(nil), parent...), prefix+"NONE")
}
