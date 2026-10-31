package pidutil

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

// fakeProcess models one PID as the kernel reports it across the probes Alive
// and AliveWithStartTime combine. state is the /proc stat state letter; an
// empty state means the PID has been reaped and no longer exists. Each probe
// runs afterProbe once it has answered, which is how a test places the
// parent's reap -- or the process's own exit -- between two probes.
type fakeProcess struct {
	pid       int
	state     string
	startTime string
	// startTimeErr makes the start-time read fail while the PID still exists:
	// a host where neither /proc nor ps can describe it.
	startTimeErr error
	procAbsent   bool // no /proc on this host (darwin): stat reads always fail
	// statReapedErr is what a stat read answers once the PID is gone. The
	// default is fs.ErrNotExist, the answer when the entry vanished before the
	// open; a reap between the open and the read answers ESRCH instead.
	statReapedErr error
	afterProbe    func(f *fakeProcess, probe string)
	probeCount    map[string]int
}

func (f *fakeProcess) answered(probe string) {
	if f.probeCount == nil {
		f.probeCount = make(map[string]int)
	}
	f.probeCount[probe]++
	if f.afterProbe != nil {
		f.afterProbe(f, probe)
	}
}

func (f *fakeProcess) probes() processProbes {
	return processProbes{
		signal0: func(int) error {
			defer f.answered("signal0")
			if f.state == "" {
				return syscall.ESRCH
			}
			return nil
		},
		readStat: func(int) ([]byte, error) {
			defer f.answered("readStat")
			if f.procAbsent {
				return nil, fs.ErrNotExist
			}
			if f.state == "" {
				if f.statReapedErr != nil {
					return nil, f.statReapedErr
				}
				return nil, fs.ErrNotExist
			}
			return []byte(fmt.Sprintf("%d (tmux: server) %s 1 2 3", f.pid, f.state)), nil
		},
		psZombie: func(int) bool {
			defer f.answered("psZombie")
			return f.state == "Z"
		},
		startTime: func(int) (string, error) {
			defer f.answered("startTime")
			if f.state == "" {
				return "", fs.ErrNotExist
			}
			if f.startTimeErr != nil {
				return "", f.startTimeErr
			}
			return f.startTime, nil
		},
	}
}

// reapAfterFirst returns an afterProbe hook that reaps the PID (removes it from
// the process table) the first time probe answers.
func reapAfterFirst(probe string) func(*fakeProcess, string) {
	return func(f *fakeProcess, answered string) {
		if answered == probe && f.probeCount[probe] == 1 {
			f.state = ""
		}
	}
}

// TestAliveZombieReapedBetweenProbes pins the race where a killed process is a
// zombie until its parent -- a subreaper such as systemd --user -- reaps it,
// and that reap lands between Alive's kill(pid, 0) probe, which succeeds on a
// zombie, and its /proc read, which then finds nothing. ps finds nothing
// either, so "ps does not report a zombie" is no evidence of life: Alive must
// not report a PID that no longer exists as alive.
func TestAliveZombieReapedBetweenProbes(t *testing.T) {
	tests := []struct {
		name string
		proc fakeProcess
		want bool
	}{
		{
			name: "zombie reaped after the signal probe",
			proc: fakeProcess{state: "Z", afterProbe: reapAfterFirst("signal0")},
			want: false,
		},
		{
			name: "live process reaped after the signal probe",
			proc: fakeProcess{state: "S", afterProbe: reapAfterFirst("signal0")},
			want: false,
		},
		{
			name: "zombie reaped between opening and reading its stat",
			proc: fakeProcess{
				state:         "Z",
				statReapedErr: &fs.PathError{Op: "read", Path: "/proc/4242/stat", Err: syscall.ESRCH},
				afterProbe:    reapAfterFirst("signal0"),
			},
			want: false,
		},
		{
			name: "zombie reaped after the signal probe on a host without /proc",
			proc: fakeProcess{state: "Z", procAbsent: true, afterProbe: reapAfterFirst("signal0")},
			want: false,
		},
		{
			name: "zombie still unreaped on a host without /proc",
			proc: fakeProcess{state: "Z", procAbsent: true},
			want: false,
		},
		{
			name: "live process on a host without /proc",
			proc: fakeProcess{state: "S", procAbsent: true},
			want: true,
		},
		{
			name: "live process",
			proc: fakeProcess{state: "S"},
			want: true,
		},
		{
			name: "unreaped zombie",
			proc: fakeProcess{state: "Z"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := tt.proc
			proc.pid = 4242
			if got := proc.probes().alive(proc.pid); got != tt.want {
				t.Errorf("alive() = %v, want %v (probes answered: %v)", got, tt.want, proc.probeCount)
			}
		})
	}
}

// TestAliveWithStartTimeProcessGoneBeforeIdentityRead pins the same race one
// step later: Alive answers true for a live process, which then exits and is
// reaped before its start time can be read. An unreadable start time on a PID
// that is gone is a death, not grounds to keep the conservative "alive".
func TestAliveWithStartTimeProcessGoneBeforeIdentityRead(t *testing.T) {
	const token = "114063671"
	tests := []struct {
		name string
		proc fakeProcess
		want bool
	}{
		{
			name: "reaped after the liveness probe",
			proc: fakeProcess{state: "S", startTime: token, afterProbe: reapAfterFirst("readStat")},
			want: false,
		},
		{
			name: "start time unreadable while the process lives",
			proc: fakeProcess{state: "S", startTimeErr: errors.New("no start time reported")},
			want: true,
		},
		{
			name: "matching start time",
			proc: fakeProcess{state: "S", startTime: token},
			want: true,
		},
		{
			name: "recycled pid",
			proc: fakeProcess{state: "S", startTime: token + "0"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := tt.proc
			proc.pid = 4242
			probes := proc.probes()
			if got := probes.aliveWithStartTime(proc.pid, token); got != tt.want {
				t.Errorf("aliveWithStartTime() = %v, want %v (probes answered: %v)", got, tt.want, proc.probeCount)
			}
		})
	}
}
