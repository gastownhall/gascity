package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

func TestEscapedOrderGroups(t *testing.T) {
	const (
		orderPID = 100 // the order's shell, leader of its own group
		ownPGID  = 50  // the controller's group
	)
	rec := func(pid, ppid, pgid int) proctable.ProcessRecord {
		return proctable.ProcessRecord{PID: pid, PPID: ppid, PGID: pgid}
	}
	// sessionLeader is a process that called setsid: the leader of its own
	// session and of its own group.
	sessionLeader := func(pid, ppid int) proctable.ProcessRecord {
		return proctable.ProcessRecord{PID: pid, PPID: ppid, PGID: pid, SessionLeader: true}
	}
	tests := []struct {
		name    string
		records []proctable.ProcessRecord
		infra   []int // PIDs whose argv is city infrastructure
		want    []int
	}{
		{
			// sh -c <order> -> bash script -> subshell -> timeout (own leader)
			// -> gc bd -> bd: only timeout's group escaped.
			name: "bounded call under a backup script",
			records: []proctable.ProcessRecord{
				rec(1, 0, 1),
				rec(ownPGID, 1, ownPGID),
				rec(orderPID, ownPGID, orderPID),
				rec(101, orderPID, orderPID),
				rec(102, 101, orderPID),
				rec(103, 102, 103),
				rec(104, 103, 103),
				rec(105, 104, 103),
			},
			want: []int{103},
		},
		{
			name: "descendants that stayed in the order's group",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				rec(101, orderPID, orderPID),
				rec(102, 101, orderPID),
			},
			want: []int{},
		},
		{
			// 101 joined group 900, whose leader is not the order's: a member
			// carrying a foreign PGID proves nothing about who owns the group.
			name: "member of a group led outside the order's tree",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				rec(101, orderPID, 900),
				rec(900, 1, 900),
			},
			want: []int{},
		},
		{
			name: "init's group and the caller's group",
			records: []proctable.ProcessRecord{
				rec(1, 0, 1),
				rec(ownPGID, orderPID, ownPGID),
				rec(orderPID, ownPGID, orderPID),
				rec(101, orderPID, 1),
			},
			want: []int{},
		},
		{
			name: "unrelated tree",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				rec(200, 1, 200),
				rec(201, 200, 201),
			},
			want: []int{},
		},
		{
			name: "several escaped groups, sorted and unique",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				rec(130, orderPID, 130),
				rec(131, 130, 130),
				rec(120, orderPID, 120),
				rec(121, 120, 121),
			},
			want: []int{120, 121, 130},
		},
		{
			name: "ppid cycle terminates",
			records: []proctable.ProcessRecord{
				rec(orderPID, 102, orderPID),
				rec(101, orderPID, 101),
				rec(102, 101, 101),
			},
			want: []int{101},
		},
		{
			// sh -c <order> -> timeout (own leader) -> gc bd -> bd, which
			// cold-started the store: bd db-proxy-child (setsid) -> dolt in the
			// proxy's group. The proxy outlives the call that started it and
			// serves the whole city, so only timeout's group is the order's.
			name: "store proxy a bounded bd call started",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				rec(101, orderPID, 101),
				rec(102, 101, 101),
				rec(103, 102, 101),
				sessionLeader(104, 103),
				rec(105, 104, 104),
			},
			want: []int{101},
		},
		{
			// Nothing a daemon started is the order's either, even a process
			// leading its own group under it.
			name: "groups under a session leader",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				sessionLeader(101, orderPID),
				rec(102, 101, 102),
				rec(103, 102, 102),
			},
			want: []int{},
		},
		{
			// sh -c <order> -> gc, which started the managed Dolt server: gc's
			// scope watchdog only starts a group (Setpgid) and its Dolt server
			// leads a group of its own. The watchdog's argv fences both.
			name: "managed Dolt watchdog a gc call started",
			records: []proctable.ProcessRecord{
				rec(orderPID, ownPGID, orderPID),
				rec(101, orderPID, orderPID),
				rec(102, 101, 102),
				rec(103, 102, 103),
			},
			infra: []int{102},
			want:  []int{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isCityInfrastructure := func(pid int) bool { return slices.Contains(tt.infra, pid) }
			got := escapedOrderGroups(tt.records, orderPID, orderPID, ownPGID, isCityInfrastructure)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("escapedOrderGroups() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShellExecRunnerLeavesADaemonItsShellStartedRunning(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	workDir := t.TempDir()
	heartbeatPath := filepath.Join(workDir, "heartbeat")
	daemonPIDPath := filepath.Join(workDir, "daemon.pid")
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, daemonPIDPath) })
	oldWaitDelay := shellExecPostCancelWaitDelay
	oldSignalGrace := shellExecSignalGrace
	shellExecPostCancelWaitDelay = 100 * time.Millisecond
	shellExecSignalGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		shellExecPostCancelWaitDelay = oldWaitDelay
		shellExecSignalGrace = oldSignalGrace
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// The order's shell starts a daemon in a session of its own, the way bd
	// starts its db-proxy-child, and is still running when the order is
	// canceled, so the daemon is still its child.
	command := fmt.Sprintf("command -v setsid >/dev/null 2>&1 || { echo no-setsid; exit 0; }; setsid sh -c 'printf \"%%s\\n\" \"$$\" > %q; while :; do printf . >> %q; sleep 0.05; done' >/dev/null 2>&1 & sleep 60", daemonPIDPath, heartbeatPath)
	out, err := shellExecRunner(ctx, command, workDir, nil)
	if strings.Contains(string(out), "no-setsid") {
		t.Skip("needs a setsid on PATH")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shellExecRunner() error = %v, want %v", err, context.DeadlineExceeded)
	}

	size := processgrouptest.WaitForFileSize(t, heartbeatPath)
	processgrouptest.WaitForFileGrowth(t, heartbeatPath, size)
}

func TestShellExecRunnerReportsAFailedProcessSnapshotAndStillKillsTheOrder(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	workDir := t.TempDir()
	heartbeatPath := filepath.Join(workDir, "heartbeat")
	childPIDPath := filepath.Join(workDir, "child.pid")
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, childPIDPath) })
	snapshotErr := errors.New("process table unreadable")
	oldSnapshot := orderExecProcessSnapshot
	orderExecProcessSnapshot = func() ([]proctable.ProcessRecord, error) { return nil, snapshotErr }
	oldSignalGrace := shellExecSignalGrace
	shellExecSignalGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		orderExecProcessSnapshot = oldSnapshot
		shellExecSignalGrace = oldSignalGrace
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	command := fmt.Sprintf("sh -c 'printf \"%%s\\n\" \"$$\" > %q; trap \"\" TERM; while :; do printf . >> %q; sleep 0.05; done' & wait", childPIDPath, heartbeatPath)
	_, err := shellExecRunner(ctx, command, workDir, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shellExecRunner() error = %v, want it to wrap %v", err, context.DeadlineExceeded)
	}
	if !errors.Is(err, snapshotErr) {
		t.Fatalf("shellExecRunner() error = %v, want it to wrap the snapshot error %v", err, snapshotErr)
	}

	size := processgrouptest.WaitForFileSize(t, heartbeatPath)
	processgrouptest.AssertFileSizeStable(t, heartbeatPath, size, 300*time.Millisecond)
}
