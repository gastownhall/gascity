//go:build darwin

package proctable

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinStartIdentityKeepsMicrosecondPrecision(t *testing.T) {
	first, err := darwinStartIdentity(unix.Timeval{Sec: 123, Usec: 1})
	if err != nil {
		t.Fatalf("first identity: %v", err)
	}
	second, err := darwinStartIdentity(unix.Timeval{Sec: 123, Usec: 2})
	if err != nil {
		t.Fatalf("second identity: %v", err)
	}
	if first == second {
		t.Fatalf("same-second process starts collapsed to %q", first)
	}
}

func TestDarwinProcessRecordUsesOneKinfoIdentity(t *testing.T) {
	process := unix.KinfoProc{
		Proc: unix.ExternProc{
			P_pid:       42,
			P_starttime: unix.Timeval{Sec: 123, Usec: 456},
		},
		Eproc: unix.Eproc{Ppid: 11, Pgid: 12},
	}
	copy(process.Proc.P_comm[:], []byte("pane-shell\x00foreign"))

	record, ok := darwinProcessRecord(process)
	if !ok {
		t.Fatal("valid KinfoProc was rejected")
	}
	wantIdentity, err := darwinStartIdentity(process.Proc.P_starttime)
	if err != nil {
		t.Fatalf("start identity: %v", err)
	}
	if record.PID != 42 || record.PPID != 11 || record.PGID != 12 || record.StartTime != wantIdentity || record.Name != "pane-shell" {
		t.Fatalf("darwinProcessRecord = %+v, want PID/PPID/PGID/start/name from one KinfoProc", record)
	}
}

func TestDarwinProcessRecordReportsSessionLeader(t *testing.T) {
	const eprocControllingTerminal = 0x1 // EPROC_CTTY
	for _, test := range []struct {
		name string
		flag int32
		want bool
	}{
		{name: "session leader", flag: eprocSessionLeader, want: true},
		{name: "session leader with a controlling terminal", flag: eprocControllingTerminal | eprocSessionLeader, want: true},
		{name: "member with a controlling terminal", flag: eprocControllingTerminal},
		{name: "member", flag: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			process := unix.KinfoProc{
				Proc:  unix.ExternProc{P_pid: 42, P_starttime: unix.Timeval{Sec: 123, Usec: 456}},
				Eproc: unix.Eproc{Ppid: 11, Pgid: 42, Flag: test.flag},
			}
			record, ok := darwinProcessRecord(process)
			if !ok {
				t.Fatal("valid KinfoProc was rejected")
			}
			if record.SessionLeader != test.want {
				t.Fatalf("SessionLeader = %t for e_flag %#x, want %t", record.SessionLeader, test.flag, test.want)
			}
		})
	}
}

func TestDarwinKinfoMissingErrorsRequireGoneProbe(t *testing.T) {
	for _, test := range []struct {
		name     string
		kinfoErr error
		probeErr error
		wantGone bool
	}{
		{name: "EIO corroborated by ESRCH", kinfoErr: unix.EIO, probeErr: unix.ESRCH, wantGone: true},
		{name: "EIO with live PID", kinfoErr: unix.EIO},
		{name: "EIO with denied probe", kinfoErr: unix.EIO, probeErr: unix.EPERM},
		{name: "ENOENT corroborated by ESRCH", kinfoErr: unix.ENOENT, probeErr: unix.ESRCH, wantGone: true},
		{name: "ENOENT with live PID", kinfoErr: unix.ENOENT},
		{name: "ENOENT with denied probe", kinfoErr: unix.ENOENT, probeErr: unix.EPERM},
		{name: "unrelated error stays indeterminate", kinfoErr: unix.EACCES, probeErr: unix.ESRCH},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := darwinKinfoErrorMeansGone(999, test.kinfoErr, func(int) error { return test.probeErr }); got != test.wantGone {
				t.Fatalf("darwinKinfoErrorMeansGone(%v, probe=%v) = %t, want %t", test.kinfoErr, test.probeErr, got, test.wantGone)
			}
		})
	}

	if !darwinKinfoErrorMeansGone(999, unix.ESRCH, func(int) error {
		t.Fatal("direct ESRCH should not require a liveness probe")
		return nil
	}) {
		t.Fatal("direct ESRCH was not classified as a vanished process")
	}
}

func TestSnapshotProcessIdentityMatchesCurrentReader(t *testing.T) {
	self := os.Getpid()
	records, err := SnapshotProcesses()
	if err != nil {
		t.Fatalf("SnapshotProcesses: %v", err)
	}
	var snapshotIdentity string
	for _, record := range records {
		if record.PID == self {
			snapshotIdentity = record.StartTime
			break
		}
	}
	if snapshotIdentity == "" {
		t.Fatalf("current process %d missing from coherent kernel snapshot", self)
	}

	currentIdentity, err := ProcessIdentity(self)
	if err != nil {
		t.Fatalf("ProcessIdentity(%d): %v", self, err)
	}
	if currentIdentity != snapshotIdentity {
		t.Fatalf("current identity = %q, snapshot identity = %q; destructive selection and signal recheck must share one contract", currentIdentity, snapshotIdentity)
	}
}
