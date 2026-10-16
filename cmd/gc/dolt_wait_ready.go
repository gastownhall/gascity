package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type managedDoltWaitReadyReport struct {
	Ready         bool
	PIDAlive      bool
	DeletedInodes bool
	// Inconclusive is set when the wait ran out and each failed probe was a
	// dial that never left this host, so the wait learned nothing about the
	// server's health.
	Inconclusive bool
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
	var localDialErr error
	serverFailed := false
	for {
		report.PIDAlive = pidAlive(pid)
		if !report.PIDAlive {
			return report, fmt.Errorf("pid %d exited", pid)
		}
		if checkDeleted && processHasDeletedDataInodes(pid, dataDir) {
			report.DeletedInodes = true
			return report, fmt.Errorf("pid %d holds deleted data inodes under %s", pid, dataDir)
		}
		err := managedDoltReadyAttempt(cityPath, host, port, user, pid, checkDeleted)
		if err == nil {
			report.Ready = true
			return report, nil
		}
		if managedDoltDialFailedLocally(err) {
			localDialErr = err
		} else {
			serverFailed = true
		}
		if time.Now().After(deadline) {
			if localDialErr != nil && !serverFailed {
				report.Inconclusive = true
				return report, fmt.Errorf("could not dial Dolt on %s:%s from this host: %w", managedDoltConnectHost(host), port, localDialErr)
			}
			return report, fmt.Errorf("timed out waiting for Dolt on %s:%s", managedDoltConnectHost(host), port)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// managedDoltReadyAttempt runs one readiness probe. It returns nil when the
// server answered a dial and a query, then answered both again after a grace
// period; otherwise it returns the failure, so a dial that never left this
// host can be told apart from a server that is down.
func managedDoltReadyAttempt(cityPath, host, port, user string, pid int, checkDeleted bool) error {
	if err := managedDoltTCPProbe(host, port); err != nil {
		return err
	}
	if err := managedDoltQueryProbe(host, port, user); err != nil {
		return err
	}
	stable, err := confirmManagedDoltStillReady(cityPath, host, port, user, pid, checkDeleted, time.Second)
	if err != nil {
		return err
	}
	if !stable {
		return fmt.Errorf("dolt on %s:%s did not stay ready", managedDoltConnectHost(host), port)
	}
	return nil
}

// confirmManagedDoltStillReady re-probes after grace. A failed re-dial comes
// back as the error so that it can be classified like the first dial.
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
	if err := managedDoltTCPProbe(host, port); err != nil {
		return false, err
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
		"inconclusive\t" + strconv.FormatBool(report.Inconclusive),
	}
}

func managedDoltConnectHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" {
		return "127.0.0.1"
	}
	return host
}

// managedDoltDialFn is the dial managedDoltTCPProbe uses. Tests replace it to
// produce dial errors a real socket cannot produce on demand.
var managedDoltDialFn = net.DialTimeout

func managedDoltTCPProbe(host, port string) error {
	conn, err := managedDoltDialFn("tcp", net.JoinHostPort(managedDoltConnectHost(host), strings.TrimSpace(port)), 250*time.Millisecond)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func managedDoltTCPReachable(host, port string) bool {
	return managedDoltTCPProbe(host, port) == nil
}

// managedDoltDialFailedLocally reports whether a dial failed before it could
// leave this host: no ephemeral port to bind (EADDRNOTAVAIL), no buffer space
// (ENOBUFS), or no file descriptor (EMFILE, ENFILE). Such a failure says
// nothing about the server on the other end.
func managedDoltDialFailedLocally(err error) bool {
	if err == nil {
		return false
	}
	for _, local := range []syscall.Errno{syscall.EADDRNOTAVAIL, syscall.ENOBUFS, syscall.EMFILE, syscall.ENFILE} {
		if errors.Is(err, local) {
			return true
		}
	}
	return false
}
