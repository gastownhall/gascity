package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type managedDoltWaitReadyReport struct {
	Ready         bool
	PIDAlive      bool
	DeletedInodes bool
}

func waitForManagedDoltReady(cityPath, host, port, user string, pid int, timeout time.Duration, checkDeleted bool) (managedDoltWaitReadyReport, error) {
	report := managedDoltWaitReadyReport{}
	if pid <= 0 {
		return report, fmt.Errorf("invalid pid %d", pid)
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return report, fmt.Errorf("missing port")
	}
	if timeout <= 0 {
		return report, fmt.Errorf("invalid timeout %s", timeout)
	}

	dataDir := ""
	if checkDeleted {
		layout, err := resolveManagedDoltRuntimeLayout(cityPath)
		if err != nil {
			return report, err
		}
		dataDir = layout.DataDir
	}

	deadline := time.Now().Add(timeout)
	for {
		report.PIDAlive = pidAlive(pid)
		if !report.PIDAlive {
			return report, fmt.Errorf("pid %d exited", pid)
		}
		if checkDeleted && processHasDeletedDataInodes(pid, dataDir) {
			report.DeletedInodes = true
			return report, fmt.Errorf("pid %d holds deleted data inodes under %s", pid, dataDir)
		}
		if managedDoltTCPReachable(host, port) {
			if err := managedDoltQueryProbe(host, port, user); err == nil {
				if stable, stableErr := confirmManagedDoltStillReady(cityPath, host, port, user, pid, checkDeleted, time.Second); stableErr == nil && stable {
					report.Ready = true
					return report, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return report, fmt.Errorf("timed out waiting for Dolt on %s:%s", managedDoltConnectHost(host), port)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func confirmManagedDoltStillReady(cityPath, host, port, user string, pid int, checkDeleted bool, grace time.Duration) (bool, error) {
	if grace > 0 {
		time.Sleep(grace)
	}
	if pid <= 0 || !pidAlive(pid) {
		return false, nil
	}
	if checkDeleted {
		layout, err := resolveManagedDoltRuntimeLayout(cityPath)
		if err != nil {
			return false, err
		}
		if processHasDeletedDataInodes(pid, layout.DataDir) {
			return false, nil
		}
	}
	if !managedDoltTCPReachable(host, port) {
		return false, nil
	}
	if err := managedDoltQueryProbe(host, port, user); err != nil {
		return false, nil
	}
	return true, nil
}

func managedDoltWaitReadyFields(report managedDoltWaitReadyReport) []string {
	return []string{
		"ready\t" + strconv.FormatBool(report.Ready),
		"pid_alive\t" + strconv.FormatBool(report.PIDAlive),
		"deleted_inodes\t" + strconv.FormatBool(report.DeletedInodes),
	}
}

func managedDoltConnectHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" {
		return "127.0.0.1"
	}
	return host
}

func managedDoltTCPReachable(host, port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(managedDoltConnectHost(host), strings.TrimSpace(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
