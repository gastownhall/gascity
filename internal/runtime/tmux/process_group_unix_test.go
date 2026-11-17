//go:build !windows

package tmux

import (
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"
)

// unsignalableProcess stands in for a root-owned sudo wrapper: every signal
// this process sends it is refused with EPERM, and it exits only when its
// terminal hangs up (tmux's kill-session).
type unsignalableProcess struct {
	target   processTarget
	alive    bool
	signals  []processSignal
	hungUp   bool
	surplus  int // probes it stays alive for after the hangup
	probesUp int
}

func (p *unsignalableProcess) signal(target processTarget, signal processSignal) (bool, error) {
	return signalProcessTargetIfCurrent(target, signal, func(int) (string, error) {
		return p.target.StartTime, nil
	}, func(_ int, signal processSignal) error {
		p.signals = append(p.signals, signal)
		return syscall.EPERM
	})
}

func (p *unsignalableProcess) isAlive(processTarget) (bool, error) {
	if p.hungUp && p.alive {
		if p.probesUp >= p.surplus {
			p.alive = false
		}
		p.probesUp++
	}
	return p.alive, nil
}

func TestTeardownLeavesAnUnsignalableProcessToTheSessionKill(t *testing.T) {
	proc := &unsignalableProcess{target: processTarget{PID: 701, StartTime: "sudo"}, alive: true, surplus: 2}
	now := time.Unix(0, 0)
	terminate := func(plan processKillPlan) error {
		return terminateProcessSet(plan.Descendants, time.Second, proc.signal, proc.isAlive,
			func(d time.Duration) { now = now.Add(d) }, func() time.Time { return now })
	}
	var order []string
	err := teardownSessionProcessPlan(
		processKillPlan{Descendants: []processTarget{proc.target}},
		nil,
		func() error {
			order = append(order, "kill-session")
			proc.hungUp = true
			return nil
		},
		func(plan processKillPlan) error {
			order = append(order, "terminate")
			return terminate(plan)
		},
		sessionMissingOnLiveServer,
	)
	if err != nil {
		t.Fatalf("teardown = %v, want nil once the hangup took the unsignalable process down", err)
	}
	if want := []string{"kill-session", "terminate"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if want := []processSignal{processSignalTerm}; !slices.Equal(proc.signals, want) {
		t.Fatalf("signals = %v, want only the refused TERM (no KILL)", proc.signals)
	}
	if proc.alive {
		t.Fatal("the unsignalable process is still alive")
	}
}

func TestTerminateProcessSetReportsAnUnsignalableSurvivor(t *testing.T) {
	proc := &unsignalableProcess{target: processTarget{PID: 702, StartTime: "sudo"}, alive: true}
	now := time.Unix(0, 0)
	var slept time.Duration
	err := terminateProcessSet([]processTarget{proc.target}, 2*processExitCheckInterval, proc.signal, proc.isAlive,
		func(d time.Duration) { slept += d; now = now.Add(d) }, func() time.Time { return now })
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err = %v, want the EPERM refusal of a process that outlived the grace period", err)
	}
	if want := []processSignal{processSignalTerm}; !slices.Equal(proc.signals, want) {
		t.Fatalf("signals = %v, want only the refused TERM (no KILL)", proc.signals)
	}
	if slept != 2*processExitCheckInterval {
		t.Fatalf("slept = %s, want the full grace period %s", slept, 2*processExitCheckInterval)
	}
}

func TestTerminateProcessSetStillSignalsTheRestBesideAnUnsignalableProcess(t *testing.T) {
	denied := processTarget{PID: 703, StartTime: "sudo"}
	own := processTarget{PID: 704, StartTime: "shell"}
	alive := map[int]bool{703: true, 704: true}
	var signals []string
	now := time.Unix(0, 0)
	err := terminateProcessSet([]processTarget{denied, own}, time.Second,
		func(target processTarget, signal processSignal) (bool, error) {
			signals = append(signals, string(signal)+":"+target.String())
			if target == denied {
				return false, errors.Join(errors.New("signaling PID 703 with TERM"), syscall.EPERM)
			}
			alive[target.PID] = false
			alive[denied.PID] = false // its parent's exit hangs it up
			return true, nil
		},
		func(target processTarget) (bool, error) { return alive[target.PID], nil },
		func(d time.Duration) { now = now.Add(d) }, func() time.Time { return now })
	if err != nil {
		t.Fatalf("terminateProcessSet = %v, want nil", err)
	}
	if want := []string{"TERM:703", "TERM:704"}; !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
}
