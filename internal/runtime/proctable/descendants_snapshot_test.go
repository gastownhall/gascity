//go:build linux || darwin

package proctable

import (
	"os"
	"syscall"
	"testing"
)

func TestSnapshotProcessTreeIsSnapshotProcessesWithoutEnrichment(t *testing.T) {
	self := os.Getpid()
	find := func(t *testing.T, name string, snapshot func() ([]ProcessRecord, error)) ProcessRecord {
		t.Helper()
		records, err := snapshot()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, record := range records {
			if record.PID == self {
				return record
			}
		}
		t.Fatalf("%s: current process %d missing", name, self)
		return ProcessRecord{}
	}

	tree := find(t, "SnapshotProcessTree", SnapshotProcessTree)
	full := find(t, "SnapshotProcesses", SnapshotProcesses)
	if tree.Name != "" || tree.SessionID != "" {
		t.Fatalf("SnapshotProcessTree record %+v carries enrichment, want Name and SessionID empty", tree)
	}
	if full.Name == "" {
		t.Fatalf("SnapshotProcesses record %+v has no Name", full)
	}
	full.Name, full.SessionID = "", ""
	if tree != full {
		t.Fatalf("SnapshotProcessTree record %+v, want SnapshotProcesses' %+v", tree, full)
	}
	if tree.PPID != os.Getppid() || tree.PGID != syscall.Getpgrp() || tree.StartTime == "" {
		t.Fatalf("SnapshotProcessTree record %+v, want PPID %d, PGID %d and a start identity", tree, os.Getppid(), syscall.Getpgrp())
	}
}
