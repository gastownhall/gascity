package main

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// orderExecProcessSnapshot reads the process table for the exec-order cancel
// path. A variable so a test can hand it a fixed table.
var orderExecProcessSnapshot = proctable.SnapshotProcessTree

// escapedOrderGroups returns the process groups, other than the order's own,
// that a descendant of the order's shell started for itself.
//
// Canceling an order kills its process group, and a descendant can leave that
// group: GNU timeout, which the bundled packs use to bound their gc and bd
// calls, makes itself a group leader so it can signal its own subtree. Once the
// order's shells die such a process is reparented to init and keeps running,
// past a controller shutdown and past gc stop, and its next bd read restarts the
// bead store gc stop just retired.
//
// Only a group whose leader (PID == PGID) is one of the order's descendants is
// returned: that is what shows the order's tree started it. rootPID must still
// be the order's shell, so the caller reads the table while the shell is
// unreaped.
//
// A descendant that detached from the order is not the order's, and neither is
// anything it started: the walk neither returns nor descends into a session
// leader (setsid is how a daemon detaches; GNU timeout only starts a group) or
// a group leader isCityInfrastructure reports. While the bd or gc call that
// started it is still running, the city's store is such a descendant: bd's
// db-proxy-child, a session leader with the Dolt server under it, or gc's
// managed-Dolt watchdog, a group leader whose Dolt server leads a group of its
// own.
//
// The walk follows parent links, so it cannot reach a descendant that was
// already orphaned (reparented away from the order's tree) before the cancel.
func escapedOrderGroups(records []proctable.ProcessRecord, rootPID, orderPGID, ownPGID int, isCityInfrastructure func(pid int) bool) []int {
	byPID := make(map[int]proctable.ProcessRecord, len(records))
	children := make(map[int][]int, len(records))
	for _, r := range records {
		byPID[r.PID] = r
		if r.PPID != r.PID {
			children[r.PPID] = append(children[r.PPID], r.PID)
		}
	}
	seen := map[int]bool{rootPID: true}
	queue := []int{rootPID}
	groups := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			r := byPID[child]
			groupLeader := r.PGID == r.PID
			if groupLeader && (r.SessionLeader || isCityInfrastructure(child)) {
				continue
			}
			queue = append(queue, child)
			if groupLeader && r.PGID > 1 && r.PGID != orderPGID && r.PGID != ownPGID {
				groups[r.PGID] = true
			}
		}
	}
	out := make([]int, 0, len(groups))
	for g := range groups {
		out = append(out, g)
	}
	sort.Ints(out)
	return out
}

// snapshotEscapedOrderGroups is escapedOrderGroups over a fresh process table.
// An order runs as its own group leader, so its shell's PID is its group.
func snapshotEscapedOrderGroups(cmd *exec.Cmd) ([]int, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, nil
	}
	records, err := orderExecProcessSnapshot()
	if err != nil {
		return nil, fmt.Errorf("listing the canceled order's processes: %w", err)
	}
	return escapedOrderGroups(records, cmd.Process.Pid, cmd.Process.Pid, syscall.Getpgrp(), proctable.IsCityInfrastructureRoot), nil
}

// terminateProcessGroups terminates every group at once, each with grace
// before SIGKILL.
func terminateProcessGroups(groups []int, grace time.Duration) error {
	errs := make([]error, len(groups))
	var wg sync.WaitGroup
	for i, g := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = processgroup.Terminate(g, grace, processgroup.Options{})
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
