//go:build !windows

package pidutil

import (
	"errors"
	"syscall"
)

// probeSignal sends a signal for Alive's existence probe. It is a seam so tests
// can model a process owned by another user, whose probe answers EPERM, or a
// PID reaped between two probes.
var probeSignal = syscall.Kill

// processProbes are the host reads Alive and AliveWithStartTime combine into
// one answer. Each reads the process table at a different instant, so a PID
// can change state between two of them; they are fields rather than direct
// calls so those interleavings can be tested deterministically.
type processProbes struct {
	// signal0 is kill(pid, 0): nil or EPERM means the PID exists, which
	// includes a zombie its parent has not reaped yet.
	signal0 func(pid int) error
	// readStat reads /proc/<pid>/stat; it fails where /proc is absent and
	// once the PID has been reaped.
	readStat func(pid int) ([]byte, error)
	// psZombie reports whether ps describes pid as a zombie.
	psZombie func(pid int) bool
	// startTime is StartTime.
	startTime func(pid int) (string, error)
}

// hostProbes are the processProbes wired to the real host, through the seams
// tests replace.
var hostProbes = processProbes{
	signal0:   func(pid int) error { return probeSignal(pid, 0) },
	readStat:  readPIDStat,
	psZombie:  psReportsZombie,
	startTime: StartTime,
}

func platformAlive(pid int) bool {
	return hostProbes.alive(pid)
}

func platformAliveWithStartTime(pid int, startTime string) bool {
	return hostProbes.aliveWithStartTime(pid, startTime)
}

// exists reports whether the kernel still has a process-table entry for pid.
func (p processProbes) exists(pid int) bool {
	err := p.signal0(pid)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (p processProbes) alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if !p.exists(pid) {
		return false
	}
	data, err := p.readStat(pid)
	if err != nil {
		if p.psZombie(pid) {
			return false
		}
		// The signal probe above succeeds on a zombie, and its parent can
		// reap it before the reads that would have shown the zombie state run:
		// then /proc and ps both find nothing, which is no evidence of life.
		// Probing again after those reads settles it -- a PID that answered
		// neither and no longer exists is dead. A live PID whose /proc entry
		// is hidden (hidepid, EPERM) or absent (darwin) still exists, so it
		// stays alive.
		return p.exists(pid)
	}
	state, ok := procStatState(string(data))
	if ok && state == "Z" {
		return false
	}
	return true
}

func (p processProbes) aliveWithStartTime(pid int, startTime string) bool {
	return aliveWithStartTime(p.alive, p.startTime, pid, startTime)
}

func platformStartTime(pid int) (string, error) {
	return psStartTime(pid)
}

func platformChildPIDs(parent int) ([]int, error) {
	return psChildPIDs(parent)
}
