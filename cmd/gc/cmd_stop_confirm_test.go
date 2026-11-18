package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A controller-handled stop is confirmed against the runtime (mc-3low3;
// staging J44 C: a SIGSTOPped tmux server outlives the bounded controller
// shutdown, and gc stop reported "City stopped." with its sessions alive).

// wedgedListProvider is a runtime whose listing never answers until
// release closes, as a SIGSTOPped tmux server's does.
type wedgedListProvider struct {
	*runtime.Fake
	release chan struct{}
}

func (p wedgedListProvider) ListRunning(prefix string) ([]string, error) {
	<-p.release
	return p.Fake.ListRunning(prefix)
}

// failingListProvider is a runtime whose listing fails.
type failingListProvider struct{ *runtime.Fake }

func (failingListProvider) ListRunning(string) ([]string, error) {
	return nil, errors.New("tmux: server not responding")
}

// lingeringListProvider keeps listing its sessions for the first linger
// listings after they stopped, as a deleted k8s pod lists Running through
// its grace period.
type lingeringListProvider struct {
	*runtime.Fake
	names  []string
	linger *atomic.Int32
}

func (p lingeringListProvider) ListRunning(prefix string) ([]string, error) {
	if p.linger.Add(-1) >= 0 {
		return p.names, nil
	}
	return p.Fake.ListRunning(prefix)
}

func lingering(t *testing.T, listings int32, names ...string) lingeringListProvider {
	t.Helper()
	var n atomic.Int32
	n.Store(listings)
	return lingeringListProvider{Fake: runningFake(t), names: names, linger: &n}
}

// sharedListProvider lists sessions of other cities too (k8s's namespace).
type sharedListProvider struct{ *runtime.Fake }

func (sharedListProvider) ListRunningShared() bool { return true }

// stopProbeProvider points the stop probe at sp, and bounds the confirming
// listings at timeout, polled every timeout/10.
func stopProbeProvider(t *testing.T, sp runtime.Provider, timeout time.Duration) {
	t.Helper()
	oldProbe, oldTimeout, oldPoll := sessionProviderForStopProbe, stopVerifyListTimeout, stopVerifyPollInterval
	sessionProviderForStopProbe = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	stopVerifyListTimeout, stopVerifyPollInterval = timeout, timeout/10
	t.Cleanup(func() {
		sessionProviderForStopProbe, stopVerifyListTimeout, stopVerifyPollInterval = oldProbe, oldTimeout, oldPoll
	})
}

func runningFake(t *testing.T, names ...string) *runtime.Fake {
	t.Helper()
	f := runtime.NewFake()
	for _, n := range names {
		if err := f.Start(context.Background(), n, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// Kills a controller-handled stop confirmed without looking: a session still
// listed at the deadline, an incomplete listing, or a listing that never
// answers fails it, each reported; an empty runtime confirms it, as does one
// whose stopped sessions leave the listing within the deadline (#7844
// review: a deleted k8s pod lists Running for its grace period); a runtime
// that does not attest its listing, or shares it with other cities, proves
// nothing, as before.
func TestConfirmControllerStopForStop(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	unattested := runningFake(t, "survivor")
	unattested.ListingUnattested = true
	for _, c := range []struct {
		name string
		sp   runtime.Provider
		want bool
		says string
	}{
		{"nothing running", runningFake(t), true, ""},
		{"a session survives", runningFake(t, "worker-2", "worker-1"), false, "session(s) worker-1, worker-2 still running 50ms after the controller stopped"},
		{"a stopped session lingers in the listing", lingering(t, 3, "worker-1"), true, ""},
		{"a session lingers past the deadline", lingering(t, 1000, "worker-1"), false, "worker-1 still running"},
		{"a listing shared with other cities", sharedListProvider{runningFake(t, "other-city-worker")}, true, ""},
		{"the listing fails", failingListProvider{runningFake(t)}, false, "the runtime inventory was incomplete"},
		{"the listing never answers", wedgedListProvider{runningFake(t, "worker-1"), release}, false, "did not answer a listing within 50ms"},
		{"an unattested listing", unattested, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stopProbeProvider(t, c.sp, 50*time.Millisecond)
			var stderr lockedBuffer
			start := time.Now()
			got := confirmControllerStopForStop(&config.City{}, t.TempDir(), &stderr)
			if got != c.want || c.says != "" && !strings.Contains(stderr.String(), c.says) {
				t.Fatalf("confirmed %v (stderr %q), want %v saying %q", got, stderr.String(), c.want, c.says)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("the confirmation took %s: it must be bounded", time.Since(start))
			}
		})
	}
}

// slowLastListProvider answers its first listing at once and every later
// one after delay, all listing the same survivor. Run it in a synctest
// bubble, where the delay is fake time.
type slowLastListProvider struct {
	*runtime.Fake
	calls *atomic.Int32
	delay time.Duration
}

func (p slowLastListProvider) ListRunning(prefix string) ([]string, error) {
	if p.calls.Add(1) > 1 {
		<-time.After(p.delay)
	}
	return p.Fake.ListRunning(prefix)
}

// Kills reporting a runtime that answers as wedged because the poll slept
// into the deadline: the final listing starts with almost no time left and
// answers well within one poll interval, so the stop names the survivor
// instead of saying the runtime did not answer a listing.
//
// On fake time: the first listing answers at 0 and the poll sleeps to 250ms,
// 50ms before the 300ms deadline; the final listing answers at 375ms, inside
// its poll-interval window (500ms) but after the deadline.
func TestConfirmControllerStopGivesTheFinalListingAPollIntervalToAnswer(t *testing.T) {
	var calls atomic.Int32
	sp := slowLastListProvider{Fake: runningFake(t, "worker-1"), calls: &calls, delay: 125 * time.Millisecond}
	stopProbeProvider(t, sp, 300*time.Millisecond)
	stopVerifyPollInterval = 250 * time.Millisecond
	cityPath := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		var stderr lockedBuffer
		if got := confirmControllerStopForStop(&config.City{}, cityPath, &stderr); got {
			t.Fatalf("confirmed a stop with worker-1 still running (stderr %q)", stderr.String())
		}
		if !strings.Contains(stderr.String(), "worker-1 still running") {
			t.Fatalf("stderr %q, want the survivor worker-1 named, not a wedged listing", stderr.String())
		}
	})
}

// Kills the standalone controller path reporting success over survivors: a
// controller acknowledges the stop, but a session is still listed, so gc
// stop exits non-zero, names it, and leaves the bead store up for it; with
// nothing running it succeeds as before.
func TestCmdStopBodyConfirmsAnAcknowledgedControllerStop(t *testing.T) {
	for _, c := range []struct {
		name     string
		running  []string
		wantCode int
	}{
		{"nothing running", nil, 0},
		{"a session survives", []string{"worker-1"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			cityDir, cfg := acknowledgedControllerStopCity(t, "ack-stop-confirm")
			startAcknowledgingStandaloneController(t, cityDir)
			stopProbeProvider(t, runningFake(t, c.running...), time.Second)
			shutdowns := 0
			overrideShutdownBeadsProviderForStop(t, func(string) error { shutdowns++; return nil })
			var stdout, stderr lockedBuffer
			code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
			switch {
			case code != c.wantCode:
				t.Fatalf("cmdStopBody() = %d, want %d; stdout=%q stderr=%q", code, c.wantCode, stdout.String(), stderr.String())
			case c.wantCode == 0 && (shutdowns != 1 || !strings.Contains(stdout.String(), "City stopped.")):
				t.Fatalf("shutdowns %d, stdout %q; want the store retired and success", shutdowns, stdout.String())
			case c.wantCode != 0 && (shutdowns != 0 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "worker-1 still running")):
				t.Fatalf("shutdowns %d, stdout %q, stderr %q; want no success, the store kept, the survivor named", shutdowns, stdout.String(), stderr.String())
			}
		})
	}
}

// Kills the supervisor-hosted path reporting success over survivors (J44 C):
// the supervisor stopped the controller, but a session is still listed, so
// gc stop exits non-zero and restores the registration; with nothing
// running it unregisters and succeeds.
func TestCmdStopSupervisorManagedCityConfirmsTheStop(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	for _, c := range []struct {
		name       string
		sp         runtime.Provider
		wantCode   int
		wantStderr string
	}{
		{"nothing running", runningFake(t), 0, ""},
		{"a session survives", runningFake(t, "worker-1"), 1, "worker-1 still running"},
		{"the runtime is wedged", wedgedListProvider{runningFake(t, "worker-1"), release}, 1, "did not answer a listing within"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			gcHome := t.TempDir()
			t.Setenv("GC_HOME", gcHome)
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_BEADS_SCOPE_ROOT", "")
			cityDir := filepath.Join(shortSocketTempDir(t, "gc-confirm-"), "supervised-city")
			if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"supervised\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			reg := registryAt(t, gcHome)
			if err := reg.Register(cityDir, "supervised-city"); err != nil {
				t.Fatal(err)
			}
			// The supervisor serves the city: its controller answers until
			// the unregister's reconcile stops it.
			startFakeControllerSocket(t, cityDir, "4242\n")
			withSupervisorTestHooks(t,
				func(_, _ io.Writer) int { return 0 },
				func(_, _ io.Writer) int { return 0 },
				func() int { return 4242 },
				func(string) (bool, string, bool) { return false, "", true },
				20*time.Millisecond, time.Millisecond)
			waitForSupervisorControllerStopHook = func(string, time.Duration) error { return nil }
			stopProbeProvider(t, c.sp, 200*time.Millisecond)
			// The serving controller's shutdown stopped the sessions; gc stop
			// only confirms it, and never stops them itself.
			oldFactory := sessionProviderForStopCity
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
				t.Error("gc stop stopped the sessions of a city its controller stopped")
				return nil, errors.New("unexpected session stop")
			}
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

			var stdout, stderr lockedBuffer
			code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 5*time.Second, false, false)
			entries, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case code != c.wantCode:
				t.Fatalf("gc stop = %d, want %d; stdout=%q stderr=%q", code, c.wantCode, stdout.String(), stderr.String())
			case c.wantCode == 0 && len(entries) != 0:
				t.Fatalf("registry %v after a confirmed stop, want the city unregistered", entries)
			case c.wantCode != 0 && (len(entries) != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), c.wantStderr)):
				t.Fatalf("registry %v, stdout %q, stderr %q; want the registration restored, no success, %q reported", entries, stdout.String(), stderr.String(), c.wantStderr)
			}
		})
	}
}

// A registered city no controller serves (a failed stop just restored its
// registration, and the supervisor has not relaunched it yet, or it is
// backing off a failed start) has no controller for the supervisor's
// reconcile to stop, so gc stop must stop its sessions itself, under the
// controller lock the unregister claimed, rather than only confirm and fail
// with them "still running after the controller stopped". A runtime that
// cannot be listed still fails the stop and restores the registration.
func TestCmdStopSupervisorManagedCityWithNoControllerStopsItsSessions(t *testing.T) {
	for _, c := range []struct {
		name       string
		sp         func(t *testing.T) (runtime.Provider, *runtime.Fake)
		reloaded   func(f *runtime.Fake)
		wantCode   int
		wantStderr []string
	}{
		{
			name: "its sessions still run",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker-1")
				return f, f
			},
		},
		{
			name: "a relaunch the reconcile stopped already stopped them",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker-1")
				return f, f
			},
			reloaded: func(f *runtime.Fake) { _ = f.Stop("worker-1") },
		},
		{
			name: "the runtime cannot be listed",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker-1")
				return failingListProvider{f}, f
			},
			wantCode:   1,
			wantStderr: []string{"the runtime inventory was incomplete", "restored registration for 'supervised-city'"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			gcHome := t.TempDir()
			t.Setenv("GC_HOME", gcHome)
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_BEADS_SCOPE_ROOT", "")
			cityDir := filepath.Join(t.TempDir(), "supervised-city")
			if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"supervised\"\n\n[beads]\nprovider = \"file\"\n\n[daemon]\nshutdown_timeout = \"0s\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			reg := registryAt(t, gcHome)
			if err := reg.Register(cityDir, "supervised-city"); err != nil {
				t.Fatal(err)
			}
			sp, fake := c.sp(t)
			withSupervisorTestHooks(t,
				func(_, _ io.Writer) int { return 0 },
				func(_, _ io.Writer) int {
					if c.reloaded != nil {
						c.reloaded(fake)
					}
					return 0
				},
				func() int { return 4242 },
				func(string) (bool, string, bool) { return false, "", false },
				20*time.Millisecond, time.Millisecond)
			stopProbeProvider(t, sp, 100*time.Millisecond)
			oldFactory := sessionProviderForStopCity
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			var providerStops atomic.Int32
			var lockHeldAtProviderStop atomic.Bool
			overrideShutdownBeadsProviderForStop(t, func(string) error {
				providerStops.Add(1)
				lockHeldAtProviderStop.Store(probeControllerLockHeld(t, cityDir))
				return nil
			})

			var stdout, stderr lockedBuffer
			code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 0, false, false)
			entries, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if code != c.wantCode {
				t.Fatalf("gc stop = %d, want %d; stdout=%q stderr=%q", code, c.wantCode, stdout.String(), stderr.String())
			}
			if probeControllerLockHeld(t, cityDir) {
				t.Fatal("controller lock still held after gc stop returned")
			}
			if c.wantCode != 0 {
				for _, want := range c.wantStderr {
					if !strings.Contains(stderr.String(), want) {
						t.Errorf("stderr %q, want it to contain %q", stderr.String(), want)
					}
				}
				// gc stop itself stopped the sessions; no controller did.
				if strings.Contains(stderr.String(), "after the controller stopped") {
					t.Errorf("stderr %q blames a controller stop that never ran", stderr.String())
				}
				if len(entries) != 1 || strings.Contains(stdout.String(), "City stopped.") {
					t.Fatalf("registry %v, stdout %q; want the registration restored and no success", entries, stdout.String())
				}
				return
			}
			if !strings.Contains(stdout.String(), "City stopped.") {
				t.Errorf("stdout %q, want City stopped.", stdout.String())
			}
			if running, _ := fake.ListRunning(""); len(running) != 0 {
				t.Errorf("sessions %v still running after a confirmed stop", running)
			}
			if len(entries) != 0 {
				t.Errorf("registry %v after a confirmed stop, want the city unregistered", entries)
			}
			if got := providerStops.Load(); got != 1 {
				t.Errorf("bead store stopped %d times, want 1", got)
			}
			if !lockHeldAtProviderStop.Load() {
				t.Error("bead store stopped without the controller lock held")
			}
		})
	}
}
