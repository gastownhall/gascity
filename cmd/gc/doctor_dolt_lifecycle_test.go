package main

import (
	"bytes"
	"errors"
	"strconv"
	"testing"
)

// The failure mode this guards against is doctor stopping a managed dolt server
// it did not start. Every "false" row below is a server someone else owns.
func TestDoctorShouldStopManagedDolt(t *testing.T) {
	tests := []struct {
		name       string
		cityIsLive bool
		wasRunning bool
		nowRunning bool
		want       bool
	}{
		{
			name:       "doctor started it on a stopped city: stop it",
			cityIsLive: false,
			wasRunning: false,
			nowRunning: true,
			want:       true,
		},
		{
			name:       "already running before doctor: leave it",
			cityIsLive: false,
			wasRunning: true,
			nowRunning: true,
			want:       false,
		},
		{
			name:       "never started: nothing to stop",
			cityIsLive: false,
			wasRunning: false,
			nowRunning: false,
			want:       false,
		},
		{
			name:       "live city owns its dolt: never stop",
			cityIsLive: true,
			wasRunning: false,
			nowRunning: true,
			want:       false,
		},
		{
			name:       "live city, already running: never stop",
			cityIsLive: true,
			wasRunning: true,
			nowRunning: true,
			want:       false,
		},
		{
			name:       "live city takes precedence over a raced probe",
			cityIsLive: true,
			wasRunning: false,
			nowRunning: false,
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := doctorShouldStopManagedDolt(tc.cityIsLive, tc.wasRunning, tc.nowRunning)
			if got != tc.want {
				t.Fatalf("doctorShouldStopManagedDolt(live=%v, was=%v, now=%v) = %v, want %v",
					tc.cityIsLive, tc.wasRunning, tc.nowRunning, got, tc.want)
			}
		})
	}
}

// A live city must never arm the guard, so no probe result can lead to a stop.
func TestNewDoctorManagedDoltGuardLiveCityIsDisarmed(t *testing.T) {
	guard := newDoctorManagedDoltGuard(t.TempDir(), true)
	if guard.armed {
		t.Fatal("guard armed for a live city; doctor must never stop a running city's dolt")
	}
}

// No city path means no trustworthy "before" picture: fail closed.
func TestNewDoctorManagedDoltGuardEmptyCityPathIsDisarmed(t *testing.T) {
	for _, cityPath := range []string{"", "   "} {
		guard := newDoctorManagedDoltGuard(cityPath, false)
		if guard.armed {
			t.Fatalf("guard armed for cityPath %q; expected fail-closed", cityPath)
		}
	}
}

// Regression: a stopped city publishes no dolt port until the server starts, so
// an implementation that resolved the port only at snapshot time disarmed
// itself in precisely the case this guard exists for — and the server leaked.
// A stopped city with nothing published must arm, with wasRunning false.
func TestNewDoctorManagedDoltGuardStoppedCityWithNoPublishedPortArms(t *testing.T) {
	guard := newDoctorManagedDoltGuard(t.TempDir(), false)
	if !guard.armed {
		t.Fatal("guard disarmed for a stopped city with no published port; " +
			"a server started by doctor's own checks would leak")
	}
	if guard.wasRunning {
		t.Fatal("wasRunning true with no published port; nothing can be running")
	}
}

// release on a disarmed guard must be inert — no panic, no output, no stop.
func TestReleaseDisarmedGuardIsInert(t *testing.T) {
	var stderr bytes.Buffer
	guard := newDoctorManagedDoltGuard(t.TempDir(), true)
	guard.release(&stderr)
	if stderr.Len() != 0 {
		t.Fatalf("disarmed guard wrote to stderr: %q", stderr.String())
	}
}

// A nil guard must be safe: doDoctor defers release unconditionally.
func TestReleaseNilGuardIsSafe(t *testing.T) {
	var stderr bytes.Buffer
	var guard *doctorManagedDoltGuard
	guard.release(&stderr)
	if stderr.Len() != 0 {
		t.Fatalf("nil guard wrote to stderr: %q", stderr.String())
	}
}

// fakeManagedDolt is a controllable stand-in for the impure managed-dolt
// operations. It models a single server whose "running" state can flip between
// the guard's snapshot and its release, letting a test drive the real
// resolve/probe/decide/stop wiring deterministically without a live server.
type fakeManagedDolt struct {
	running   bool // whether a server is currently up
	resolveTo managedDoltPortResolution
	port      string
	probeErr  error
	live      bool // whether the city is live at the moment liveNow is asked
	liveCalls int
	stops     []string // ports stop was invoked for
}

func (f *fakeManagedDolt) deps() doctorManagedDoltDeps {
	return doctorManagedDoltDeps{
		resolvePort: func(string) (string, managedDoltPortResolution) {
			if f.resolveTo == managedDoltPortError {
				return "", managedDoltPortError
			}
			if f.running {
				return f.port, managedDoltPortFound
			}
			return "", managedDoltPortAbsent
		},
		probe: func(_, _, _ string) (managedDoltProbeReport, error) {
			if f.probeErr != nil {
				return managedDoltProbeReport{}, f.probeErr
			}
			return managedDoltProbeReport{Running: f.running}, nil
		},
		liveNow: func(string) bool { f.liveCalls++; return f.live },
		stop: func(_, port string) (managedDoltStopReport, error) {
			f.stops = append(f.stops, port)
			f.running = false
			return managedDoltStopReport{}, nil
		},
	}
}

// Finding 1 (the leak-regression mutant): when the snapshot saw nothing running
// and doctor's own checks then started a server on a stopped, non-live city,
// release MUST invoke the real stop path. Dead-coding that stop call — which
// re-leaks every server doctor starts (#4685) — leaves this test red.
func TestReleaseStopsServerDoctorStarted(t *testing.T) {
	fake := &fakeManagedDolt{running: false, port: "13317"} // stopped at snapshot
	g := newDoctorManagedDoltGuardWithDeps(t.TempDir(), false, fake.deps())
	if !g.armed {
		t.Fatal("guard should arm on a stopped, non-live city")
	}
	if g.wasRunning {
		t.Fatal("wasRunning should be false; nothing was running at snapshot")
	}

	fake.running = true // doctor's checks started one
	var stderr bytes.Buffer
	g.release(&stderr)

	if len(fake.stops) != 1 || fake.stops[0] != "13317" {
		t.Fatalf("release did not stop the server doctor started; stop calls=%v", fake.stops)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr on a clean stop: %q", stderr.String())
	}
}

// Finding 1 (suppress side): a server already up at snapshot belongs to someone
// else; release must never stop it even though it is still running at exit.
func TestReleaseSuppressesStopForPreexistingServer(t *testing.T) {
	fake := &fakeManagedDolt{running: true, port: "13317"} // running at snapshot
	g := newDoctorManagedDoltGuardWithDeps(t.TempDir(), false, fake.deps())
	if !g.armed {
		t.Fatal("guard should arm")
	}
	if !g.wasRunning {
		t.Fatal("wasRunning should be true; a server was up at snapshot")
	}

	var stderr bytes.Buffer
	g.release(&stderr)

	if len(fake.stops) != 0 {
		t.Fatalf("release stopped a pre-existing server; stop calls=%v", fake.stops)
	}
}

// Finding 2 (cityIsLive TOCTOU): a city that goes live between the snapshot and
// release owns its server, so release must re-check liveness and skip the stop
// even though the snapshot recorded cityIsLive=false.
func TestReleaseSkipsStopWhenCityBecameLive(t *testing.T) {
	fake := &fakeManagedDolt{running: false, port: "13317"} // stopped at snapshot
	g := newDoctorManagedDoltGuardWithDeps(t.TempDir(), false, fake.deps())
	if !g.armed || g.wasRunning {
		t.Fatal("expected an armed guard with wasRunning=false")
	}

	fake.running = true // a server is up at release...
	fake.live = true    // ...but the city went live mid-run and now owns it
	var stderr bytes.Buffer
	g.release(&stderr)

	if fake.liveCalls == 0 {
		t.Fatal("release did not re-check liveness before the stop decision")
	}
	if len(fake.stops) != 0 {
		t.Fatalf("release stopped a server the now-live city owns; stop calls=%v", fake.stops)
	}
}

// Finding 3 (fail-open on transient errors): a port-resolution error at snapshot
// leaves presence unknown, so the guard must disarm rather than assume nothing
// is running and later stop a server it never proved it started.
func TestNewDoctorManagedDoltGuardDisarmsOnPortResolutionError(t *testing.T) {
	fake := &fakeManagedDolt{resolveTo: managedDoltPortError}
	g := newDoctorManagedDoltGuardWithDeps(t.TempDir(), false, fake.deps())
	if g.armed {
		t.Fatal("guard armed despite a port-resolution error; transient errors must fail closed (#4827 finding 3)")
	}
}

// Finding 3 companion: a probe error at snapshot is equally untrustworthy and
// must disarm.
func TestNewDoctorManagedDoltGuardDisarmsOnProbeError(t *testing.T) {
	fake := &fakeManagedDolt{running: true, port: "13317", probeErr: errors.New("probe boom")}
	g := newDoctorManagedDoltGuardWithDeps(t.TempDir(), false, fake.deps())
	if g.armed {
		t.Fatal("guard armed despite a snapshot probe error; must fail closed")
	}
}

// Finding 3 boundary: a genuinely absent server (not an error) must still arm
// the guard with wasRunning=false — the stopped-city case the guard exists for.
func TestNewDoctorManagedDoltGuardAbsentPortArms(t *testing.T) {
	fake := &fakeManagedDolt{running: false} // resolvePort -> absent
	g := newDoctorManagedDoltGuardWithDeps(t.TempDir(), false, fake.deps())
	if !g.armed {
		t.Fatal("guard should arm on a genuinely stopped city")
	}
	if g.wasRunning {
		t.Fatal("wasRunning should be false when nothing is published")
	}
}

// Finding 3 core: the presence classifier must keep the deterministic
// "not running" verdicts (stopped, dead PID, wrong layout, unowned — folded into
// deterministicallyPresent=false) as absent, but treat an alive, owned server
// whose listener did not answer (deterministicallyPresent=true, portReachable=
// false) as error, never absent. Classing that transient case absent is exactly
// the fail-open the guard's wasRunning=false arm then turns into stopping a
// pre-existing server once the port answers again (#4827 review finding 3).
func TestClassifyDoltRuntimeStatePresence(t *testing.T) {
	tests := []struct {
		name                     string
		deterministicallyPresent bool
		portReachable            bool
		want                     managedDoltPortResolution
	}{
		{
			name:                     "alive, owned, reachable: found",
			deterministicallyPresent: true,
			portReachable:            true,
			want:                     managedDoltPortFound,
		},
		{
			name:                     "alive, owned, momentarily unreachable: error (fail closed)",
			deterministicallyPresent: true,
			portReachable:            false,
			want:                     managedDoltPortError,
		},
		{
			name:                     "deterministically not running: absent",
			deterministicallyPresent: false,
			portReachable:            false,
			want:                     managedDoltPortAbsent,
		},
		{
			name:                     "not present: absent regardless of a stray reachable probe",
			deterministicallyPresent: false,
			portReachable:            true,
			want:                     managedDoltPortAbsent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDoltRuntimeStatePresence(tc.deterministicallyPresent, tc.portReachable)
			if got != tc.want {
				t.Fatalf("classifyDoltRuntimeStatePresence(present=%v, reachable=%v) = %d, want %d",
					tc.deterministicallyPresent, tc.portReachable, got, tc.want)
			}
		})
	}
}

// Finding-3 wiring: resolveDoltRuntimeStatePresence must assemble the two
// booleans the classifier consumes from the injected probes, hermetically —
// spawning a real owned-but-unreachable server to prove this (the prior test)
// cost a subprocess, a fixed sleep, and the slow-process gate, all of which
// pushed cmd/gc past its resource-census baseline.
//
// An alive, owned server whose listener did not answer (identity+pidAlive+owned
// true, reachable false) must resolve to error, never absent: absent arms
// doctor's stop guard with wasRunning=false and lets release stop a pre-existing
// server once the port answers again (#4827 review finding 3). Each deterministic
// "not running" read (identity, pidAlive, or ownership false) must short-circuit
// to absent without paying the reachability probe.
func TestResolveDoltRuntimeStatePresence(t *testing.T) {
	state := doltRuntimeState{Running: true, PID: 4321, Port: 6543}
	tests := []struct {
		name       string
		identityOK bool
		pidAlive   bool
		owned      bool
		reachable  bool
		wantProbed bool
		want       managedDoltPortResolution
	}{
		{name: "alive, owned, reachable: found", identityOK: true, pidAlive: true, owned: true, reachable: true, wantProbed: true, want: managedDoltPortFound},
		{name: "alive, owned, unreachable: error (fail closed)", identityOK: true, pidAlive: true, owned: true, reachable: false, wantProbed: true, want: managedDoltPortError},
		{name: "wrong identity: absent, no reachability probe", identityOK: false, pidAlive: true, owned: true, reachable: true, wantProbed: false, want: managedDoltPortAbsent},
		{name: "dead pid: absent, no reachability probe", identityOK: true, pidAlive: false, owned: true, reachable: true, wantProbed: false, want: managedDoltPortAbsent},
		{name: "unowned: absent, no reachability probe", identityOK: true, pidAlive: true, owned: false, reachable: true, wantProbed: false, want: managedDoltPortAbsent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probedPort := ""
			probes := managedDoltStatePresenceProbes{
				identity: func(gotState doltRuntimeState, cityPath string) (managedDoltRuntimeLayout, bool) {
					if gotState != state {
						t.Fatalf("identity probe state = %+v, want %+v", gotState, state)
					}
					if cityPath != "/city" {
						t.Fatalf("identity probe cityPath = %q, want %q", cityPath, "/city")
					}
					return managedDoltRuntimeLayout{}, tc.identityOK
				},
				pidAlive: func(pid int) bool {
					if pid != state.PID {
						t.Fatalf("pidAlive pid = %d, want %d", pid, state.PID)
					}
					return tc.pidAlive
				},
				owned: func(doltRuntimeState, managedDoltRuntimeLayout) bool { return tc.owned },
				reachable: func(gotPort string) bool {
					probedPort = gotPort
					return tc.reachable
				},
			}

			got := resolveDoltRuntimeStatePresence(state, "/city", probes)
			if got != tc.want {
				t.Fatalf("resolveDoltRuntimeStatePresence = %d, want %d", got, tc.want)
			}
			probed := probedPort != ""
			if probed != tc.wantProbed {
				t.Fatalf("reachability probed = %v (port %q), want %v", probed, probedPort, tc.wantProbed)
			}
			if tc.wantProbed && probedPort != strconv.Itoa(state.Port) {
				t.Fatalf("reachability probed port = %q, want %q", probedPort, strconv.Itoa(state.Port))
			}
		})
	}
}

// The real resolver must classify a stopped city (no owned lifecycle, nothing
// published) as absent, and currentManagedDoltPort must project that to empty —
// the behavior the three bd_env.go callers depend on.
func TestResolveManagedDoltPortAbsentOnStoppedCity(t *testing.T) {
	city := t.TempDir()
	port, resolution := resolveManagedDoltPort(city)
	if resolution != managedDoltPortAbsent || port != "" {
		t.Fatalf("stopped city: got (%q, %d), want (\"\", absent=%d)", port, resolution, managedDoltPortAbsent)
	}
	if got := currentManagedDoltPort(city); got != "" {
		t.Fatalf("currentManagedDoltPort(stopped city) = %q, want empty", got)
	}
}
