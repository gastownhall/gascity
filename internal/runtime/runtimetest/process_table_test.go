package runtimetest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// hostProcessFake is a fake whose sessions run their command on this host,
// in a process group of their own with the session's env, and whose
// process-table scanner keeps RUNTIME-CONTRACT-024 unless a knob breaks one
// duty.
type hostProcessFake struct {
	*runtime.Fake
	mu    sync.Mutex
	procs map[string]*exec.Cmd

	offHost      bool // its sessions do not share the host: it cannot scan
	claimsScan   bool // with offHost: it claims the scanner anyway
	killsOffHost bool // with offHost: TerminateRuntime signals anyway
	untracked    bool // no root is tracked, a running session's included
	unbound      bool // TerminateRuntime ignores the start identity
	dropsInfra   bool // the scan leaves out city-infrastructure roots
}

func newHostProcessFake(t *testing.T, configure func(*hostProcessFake)) *hostProcessFake {
	f := &hostProcessFake{Fake: runtime.NewFake(), procs: map[string]*exec.Cmd{}}
	configure(f)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for name := range f.procs {
			f.endLocked(name)
		}
	})
	return f
}

func (f *hostProcessFake) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := f.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	cmd := exec.Command("sh", "-c", cfg.Command)
	cmd.Env = execenv.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[name] = cmd
	return nil
}

func (f *hostProcessFake) Stop(name string) error {
	f.mu.Lock()
	f.endLocked(name)
	f.mu.Unlock()
	return f.Fake.Stop(name)
}

// endLocked kills name's process group and reaps it. The caller holds f.mu.
func (f *hostProcessFake) endLocked(name string) {
	cmd, ok := f.procs[name]
	if !ok {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()
	delete(f.procs, name)
}

func (f *hostProcessFake) CanScanProcessTable() bool { return !f.offHost || f.claimsScan }

func (f *hostProcessFake) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	if f.offHost && !f.claimsScan {
		return nil, nil
	}
	found, err := proctable.ScanBySessionID(id)
	f.mu.Lock()
	tracked := map[string]string{}
	for name, cmd := range f.procs {
		for _, kv := range cmd.Env {
			if v, ok := strings.CutPrefix(kv, "GC_SESSION_ID="); ok {
				tracked[v] = name
			}
		}
	}
	f.mu.Unlock()
	var out []runtime.LiveRuntime
	for _, r := range found {
		if f.dropsInfra && proctable.IsCityInfrastructureRoot(r.PID) {
			continue
		}
		if name, ok := tracked[r.SessionID]; ok && !f.untracked {
			r.IsTracked, r.ProviderName = true, name
		}
		out = append(out, r)
	}
	return out, err
}

func (f *hostProcessFake) TerminateRuntime(r runtime.LiveRuntime) error {
	if f.offHost && !f.killsOffHost {
		return errors.New("off host")
	}
	if f.unbound {
		return proctable.KillByPID(r.PID)
	}
	return proctable.KillByPIDIdentity(r.PID, r.StartIdentity)
}

// processTableFixtureOf is the fixtures over hostProcessFakes configured by
// configure, the off-host one also marked offHost.
func processTableFixtureOf(configure func(*hostProcessFake)) ProcessTableScannerFixtures {
	return ProcessTableScannerFixtures{
		Scanning: func(t *testing.T) runtime.Provider { return newHostProcessFake(t, configure) },
		NotScanning: func(t *testing.T) runtime.Provider {
			return newHostProcessFake(t, func(f *hostProcessFake) { f.offHost = true; configure(f) })
		},
	}
}

// TestRunProcessTableScannerContractOnAConformingFake runs the suite against
// a fake that keeps every duty.
func TestRunProcessTableScannerContractOnAConformingFake(t *testing.T) {
	RunProcessTableScannerContract(t, processTableFixtureOf(func(*hostProcessFake) {}))
}

// Kills a vacuous RUNTIME-CONTRACT-024 check: each fake breaks one duty, and
// the check that names it must report it.
func TestProcessTableChecksCatchEachViolation(t *testing.T) {
	onHost := func(check func(*testing.T, runtime.Provider, contractLeftover) error) func(*testing.T, ProcessTableScannerFixtures) error {
		return func(t *testing.T, f ProcessTableScannerFixtures) error {
			sp, l := startLeftoverSession(t, f)
			return check(t, sp, l)
		}
	}
	offHost := func(t *testing.T, f ProcessTableScannerFixtures) error {
		requireProcfs(t)
		return checkNoScannerOffHost(t, f.NotScanning(t))
	}
	cases := []struct {
		name    string
		violate func(*hostProcessFake)
		check   func(*testing.T, ProcessTableScannerFixtures) error
	}{
		{"tracks no running session", func(f *hostProcessFake) { f.untracked = true }, onHost(checkLeftoverFound)},
		{"kills without the start identity", func(f *hostProcessFake) { f.unbound = true }, onHost(checkTerminateBoundToIdentity)},
		{"drops city-infrastructure roots", func(f *hostProcessFake) { f.dropsInfra = true }, onHost(checkInfrastructureRootReported)},
		{"claims the scanner off the host", func(f *hostProcessFake) { f.claimsScan = true }, offHost},
		{"terminates off the host", func(f *hostProcessFake) { f.killsOffHost = true }, offHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.check(t, processTableFixtureOf(tc.violate)); err == nil {
				t.Errorf("the check passed a provider that %s", tc.name)
			} else {
				t.Log(fmt.Sprint(err))
			}
		})
	}
}
