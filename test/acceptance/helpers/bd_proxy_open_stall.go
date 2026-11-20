package acceptancehelpers

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// bdProxyOpenStallLine matches a gc init whose provider-owned beads init failed
// on bd v1.3.1's fixed 15 s proxy open deadline (beads
// internal/storage/dbproxy/proxy/endpoint.go, openDeadline): the proxy bd
// spawned was still not serving, so bd killed it and returned the first text,
// or the second when that kill was not confirmed within 5 s. A host under load
// is enough to cause it, and so is the backend port lost to another listener
// (beads#7184), which a fresh .beads also gets past by picking a new port. This
// is how the acceptance gate's gc init calls failed (ga-yi96afx, tracker
// ga-vkhfnj).
//
// The match runs to the end of the line on purpose. The upstream fix, beads
// be-q6uduz (ruling be-6qvlzq), keeps the leading "timeout waiting for proxy"
// phrase and follows it with the child's pid, the open budget and the log path,
// so a bd that carries the fix no longer matches, and the same stall from it
// fails the test, as the tracker says a sighting after the fix must. The
// provider-owned beads init step runs before gc registers or starts anything,
// so the city directory holds everything the failed attempt left behind.
var bdProxyOpenStallLine = regexp.MustCompile(`(?m)provider-owned beads init: Error: failed to open uow provider: uow: get proxy endpoint: timeout waiting for proxy (?:to become ready on its OS-assigned port|on its OS-assigned port; safe child kill failed: timeout \(5s\) waiting for pid \d+)$`)

// bdScopeProcessKillWait bounds how long resetBDProxiedCity waits for the
// processes it killed to leave the process table.
const bdScopeProcessKillWait = 30 * time.Second

// bdScopeLogTailBytes is how much of each scope log a stall report keeps.
const bdScopeLogTailBytes = 4096

// isBDProxyOpenStall reports whether out, a failed gc init's output, carries
// bdProxyOpenStallLine.
func isBDProxyOpenStall(out string) bool {
	return bdProxyOpenStallLine.MatchString(out)
}

// initRecoveringBDProxyOpenStall runs init and returns its result, except when
// that attempt failed with a bd proxy-open stall (isBDProxyOpenStall) and armed
// reports true: then it hands the failed output to report, resets the scope,
// and returns a second init's result. Only the first attempt is retried, and
// only on that signature. Any other failure, and a stall armed refuses, are
// returned as they are; a failed reset is returned with the first attempt's
// output and the reset error wrapped in.
func initRecoveringBDProxyOpenStall(init func() (string, error), armed func() bool, reset func() error, report func(out string)) (string, error) {
	out, err := init()
	if err == nil || !isBDProxyOpenStall(out) || !armed() {
		return out, err
	}
	report(out)
	if resetErr := reset(); resetErr != nil {
		return out, fmt.Errorf("%w; resetting the city to retry the bd proxy-open stall: %w", err, resetErr)
	}
	return init()
}

// runInit runs gc init with args for this city, retrying once from an empty
// directory when the attempt fails with bd's proxy-open stall
// (initRecoveringBDProxyOpenStall). The retry is armed only when the city
// directory was empty before the first attempt, so emptying it restores exactly
// what the caller set up.
func (c *City) runInit(args ...string) (string, error) {
	c.t.Helper()
	startedEmpty := dirIsEmpty(c.Dir)
	return initRecoveringBDProxyOpenStall(
		func() (string, error) { return RunGC(c.Env, "", args...) },
		func() bool { return startedEmpty },
		func() error { return resetBDProxiedCity(c.Dir) },
		func(out string) {
			c.t.Logf("gc %s hit bd v1.3.1's 15 s proxy open deadline (beads be-q6uduz, tracker ga-vkhfnj); retrying once from an empty city directory. First attempt:\n%s%s",
				strings.Join(args, " "), out, bdScopeLogTails(c.Dir))
		},
	)
}

// dirIsEmpty reports whether dir exists and has no entries.
func dirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

// resetBDProxiedCity returns cityDir to the empty directory gc init started
// from. It first kills every bd proxy and dolt sql-server whose command line
// names a path under cityDir, and waits for them to leave the process table:
// bd kills only the proxy child it spawned, never that child's dolt, and its
// kill can go unconfirmed, so either may still be running and holding the
// scope's port and data directory. Then it removes everything in cityDir.
func resetBDProxiedCity(cityDir string) error {
	deadline := time.Now().Add(bdScopeProcessKillWait)
	for {
		pids, err := bdScopeProcessesUnder(cityDir)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bd proxy or dolt processes under %s still running %s after they were killed: %v", cityDir, bdScopeProcessKillWait, pids)
		}
		for _, pid := range pids {
			if err := killPID(pid); err != nil {
				return fmt.Errorf("killing pid %d under %s: %w", pid, cityDir, err)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	entries, err := os.ReadDir(cityDir)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cityDir, err)
	}
	for _, entry := range entries {
		path := filepath.Join(cityDir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing %s: %w", path, err)
		}
	}
	return nil
}

// killPID kills pid, treating a process that has already exited as killed.
func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// bdScopeProcessesUnder returns the pids of the bd proxies and dolt sql-servers
// whose command line names a path under root, read from the process table.
func bdScopeProcessesUnder(root string) ([]int, error) {
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return nil, fmt.Errorf("reading the process table: %w", err)
	}
	return bdScopePIDs(string(out), root)
}

// bdScopePIDs picks out of table, `ps -eo pid=,args=` output, the pids of the
// bd proxies and dolt sql-servers whose command line names a path under root.
func bdScopePIDs(table, root string) ([]int, error) {
	under := filepath.Clean(root) + string(filepath.Separator)
	var pids []int
	for _, line := range strings.Split(table, "\n") {
		pidField, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.Contains(args, under) {
			continue
		}
		if !strings.Contains(args, "db-proxy-child") && !strings.Contains(args, "sql-server") {
			continue
		}
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			return nil, fmt.Errorf("parsing pid %q from the process table: %w", pidField, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// bdScopeLogTails returns the tails of the city scope's dolt and proxy logs for
// a stall report: resetBDProxiedCity deletes them, and a retry that passes
// leaves no failure artifacts behind.
func bdScopeLogTails(cityDir string) string {
	var b strings.Builder
	for _, name := range []string{"server.log", "proxy.log"} {
		path := filepath.Join(cityDir, ".beads", "dolt", name)
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(&b, "\n--- %s: %v ---", path, err)
			continue
		}
		if len(data) > bdScopeLogTailBytes {
			data = data[len(data)-bdScopeLogTailBytes:]
		}
		fmt.Fprintf(&b, "\n--- %s (last %d bytes) ---\n%s", path, len(data), data)
	}
	return b.String()
}
