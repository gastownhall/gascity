package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
	tests := []struct {
		name    string
		records []proctable.ProcessRecord
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapedOrderGroups(tt.records, orderPID, orderPID, ownPGID)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("escapedOrderGroups() = %v, want %v", got, tt.want)
			}
		})
	}
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
