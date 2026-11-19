package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
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

// absentServerProvider is a runtime whose server is not running at all, as
// after a stop tore it down: its listing fails with the server's absence.
type absentServerProvider struct{ *runtime.Fake }

func (absentServerProvider) ListRunning(string) ([]string, error) {
	return nil, &runtime.PartialListError{Err: errors.New("no server running"), ServerAbsent: true}
}

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

// Kills gc stop skipping its own stop of a registered city no controller
// served on a runtime that does not prove nothing is running: only a listing
// the runtime attests and does not share with other cities, answering
// complete and empty (an absent runtime server holds no session), proves it.
// A listed session, a failed or wedged listing, an unattested or shared one,
// or a runtime that cannot be opened proves nothing, and the check is bounded.
func TestCityRuntimeEmptyForStop(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	unattested := runningFake(t)
	unattested.ListingUnattested = true
	for _, c := range []struct {
		name string
		sp   runtime.Provider
		want bool
	}{
		{"nothing running", runningFake(t), true},
		{"the runtime server is not running", absentServerProvider{runningFake(t)}, true},
		{"a session runs", runningFake(t, "worker-1"), false},
		{"the listing fails", failingListProvider{runningFake(t)}, false},
		{"the listing never answers", wedgedListProvider{runningFake(t), release}, false},
		{"an unattested listing", unattested, false},
		{"a listing shared with other cities", sharedListProvider{runningFake(t)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stopProbeProvider(t, c.sp, 50*time.Millisecond)
			start := time.Now()
			if got := cityRuntimeEmptyForStop(&config.City{}, t.TempDir()); got != c.want {
				t.Fatalf("cityRuntimeEmptyForStop = %v, want %v", got, c.want)
			}
			if time.Since(start) > 5*time.Second {
				t.Fatalf("the check took %s: it must be bounded", time.Since(start))
			}
		})
	}
	t.Run("the runtime cannot be opened", func(t *testing.T) {
		oldProbe := sessionProviderForStopProbe
		sessionProviderForStopProbe = func(*config.City, string) (runtime.Provider, error) {
			return nil, errors.New("exec provider: script missing")
		}
		t.Cleanup(func() { sessionProviderForStopProbe = oldProbe })
		if cityRuntimeEmptyForStop(&config.City{}, t.TempDir()) {
			t.Fatal("cityRuntimeEmptyForStop = true over a runtime it could not open, want false")
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

// confirmListingWatcher is the stop probe's runtime over the stop's own
// sessions. listed runs at each of its listings: on the paths of a city whose
// bead store is not a provider-owned proxied one (so the already-stopped
// check never lists it), those are the stop confirmation's listings.
type confirmListingWatcher struct {
	*runtime.Fake
	listed func()
}

func (p confirmListingWatcher) ListRunning(prefix string) ([]string, error) {
	p.listed()
	return p.Fake.ListRunning(prefix)
}

// workerAgentToml configures the city's agent "worker" as a named session
// (max_active_sessions = 1, so it does not expand into a pool), whose
// session name is "worker".
const workerAgentToml = "\n[[agent]]\nname = \"worker\"\nmax_active_sessions = 1\n"

// A registered city no controller serves (a failed stop just restored its
// registration, and the supervisor has not relaunched it yet, or it is
// backing off a failed start) has no controller for the supervisor's
// reconcile to stop, so gc stop must stop its sessions itself, under the
// controller lock the unregister claimed, rather than only confirm and fail
// with them "still running after the controller stopped". A runtime that
// cannot be listed, or a bead store that will not stop, still fails the stop
// and restores the registration, as on the path a controller stopped. So
// does a session, configured or not, that outlives its stop: the stop is
// confirmed against the runtime, while gc stop still holds the controller
// lock and the pending unregister, even though the runtime offers no server
// teardown that would end the session. (A city with nothing running needs no
// stop of its own; see
// TestCmdStopSupervisorManagedCityWithNothingRunningSucceedsWithoutReadingItsStore.)
func TestCmdStopSupervisorManagedCityWithNoControllerStopsItsSessions(t *testing.T) {
	for _, c := range []struct {
		name       string
		sp         func(t *testing.T) (runtime.Provider, *runtime.Fake)
		configured bool
		storeErr   error
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
			name: "its configured and other sessions still run",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker", "worker-1")
				return f, f
			},
			configured: true,
		},
		{
			name: "a session's stop fails",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker", "worker-1")
				f.StopErrors = map[string]error{"worker": errors.New("session wedged"), "worker-1": errors.New("session wedged")}
				return f, f
			},
			configured: true,
			wantCode:   1,
			wantStderr: []string{"session(s) worker, worker-1 still running 100ms after gc stop tried to stop the city's sessions", "restored registration for 'supervised-city'"},
		},
		{
			name: "a session's stop leaves it running",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker", "worker-1")
				f.StopLeavesRunning = map[string]bool{"worker": true, "worker-1": true}
				return f, f
			},
			configured: true,
			wantCode:   1,
			wantStderr: []string{"session(s) worker, worker-1 still running 100ms after gc stop tried to stop the city's sessions", "restored registration for 'supervised-city'"},
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
		{
			name: "the bead store will not stop",
			sp: func(t *testing.T) (runtime.Provider, *runtime.Fake) {
				f := runningFake(t, "worker-1")
				return f, f
			},
			storeErr:   errors.New("dolt still serving"),
			wantCode:   1,
			wantStderr: []string{"gc stop: bead store: dolt still serving", "restored registration for 'supervised-city'"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			sp, fake := c.sp(t)
			cityDir, reg := unservedRegisteredCity(t, sp, "0s", 4242, nil)
			if c.configured {
				appendToFile(t, filepath.Join(cityDir, "city.toml"), workerAgentToml)
			}
			var providerStops atomic.Int32
			var lockHeldAtProviderStop atomic.Bool
			overrideShutdownBeadsProviderForStop(t, func(string) error {
				providerStops.Add(1)
				lockHeldAtProviderStop.Store(probeControllerLockHeld(t, cityDir))
				return c.storeErr
			})
			var confirmListings, unguardedConfirmListings atomic.Int32
			stopProbeProvider(t, confirmListingWatcher{Fake: fake, listed: func() {
				confirmListings.Add(1)
				if entries, err := reg.List(); err != nil || len(entries) != 0 || !probeControllerLockHeld(t, cityDir) {
					unguardedConfirmListings.Add(1)
				}
			}}, 100*time.Millisecond)

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
			if got := unguardedConfirmListings.Load(); got != 0 {
				t.Errorf("%d of %d confirming listings ran after gc stop released the controller lock or the pending unregister", got, confirmListings.Load())
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
			if confirmListings.Load() == 0 {
				t.Error("gc stop reported success without confirming its stop against the runtime")
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

// A registered city no controller served whose runtime proves that nothing is
// running (nothing ever started, or the supervisor's reload already stopped
// its sessions) leaves gc stop's own stop nothing to stop. gc stop unregisters
// it and retires its bead store as after a controller stopped, without opening
// the session store, as it did before it stopped such a city's sessions
// itself. A session store that cannot list the city's sessions (a gc-managed
// Dolt that is down, a stale proxy record: often why the supervisor keeps
// failing to start the city) then does not fail the stop and hand the city
// back to the supervisor's retries. The runtime is listed under the controller
// lock the unregister claimed, with the registration removed, so nothing can
// start after the listing.
func TestCmdStopSupervisorManagedCityWithNothingRunningSucceedsWithoutReadingItsStore(t *testing.T) {
	for _, c := range []struct {
		name     string
		running  []string
		reloaded bool
	}{
		{"nothing ever ran", nil, false},
		{"the supervisor's reload already stopped its sessions", []string{"worker-1"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := runningFake(t, c.running...)
			var reload func()
			if c.reloaded {
				reload = func() {
					for _, name := range c.running {
						_ = fake.Stop(name)
					}
				}
			}
			cityDir, reg := unservedRegisteredCity(t, fake, "0s", 4242, reload)
			storeOpens := unreadableSessionStore(t)
			var listings, unguardedListings atomic.Int32
			stopProbeProvider(t, confirmListingWatcher{Fake: fake, listed: func() {
				listings.Add(1)
				if entries, err := reg.List(); err != nil || len(entries) != 0 || !probeControllerLockHeld(t, cityDir) {
					unguardedListings.Add(1)
				}
			}}, 100*time.Millisecond)
			oldFactory := sessionProviderForStopCity
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
				t.Error("gc stop ran its own stop of a city with nothing running")
				return nil, errors.New("unexpected own stop")
			}
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

			var stdout, stderr lockedBuffer
			if code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 0, false, false); code != 0 || !strings.Contains(stdout.String(), "City stopped.") {
				t.Fatalf("gc stop = %d, stdout %q, stderr %q; want City stopped.", code, stdout.String(), stderr.String())
			}
			if entries := registryEntries(t, reg); len(entries) != 0 {
				t.Errorf("registry %v after gc stop, want the city unregistered", entries)
			}
			if got := storeOpens.Load(); got != 0 {
				t.Errorf("gc stop opened the session store %d times, want none", got)
			}
			if listings.Load() == 0 {
				t.Error("gc stop reported success without listing the runtime")
			}
			if got := unguardedListings.Load(); got != 0 {
				t.Errorf("%d of %d listings ran without the controller lock held or with the city still registered", got, listings.Load())
			}
			if probeControllerLockHeld(t, cityDir) {
				t.Error("controller lock still held after gc stop returned")
			}
		})
	}
}

// gc stop's own stop of a registered city no controller served fails over a
// session store that cannot list the city's sessions, since it cannot check
// for one left creating by an interrupted start, and the registration is
// restored. It still stops the sessions the runtime lists, so a rerun finds
// nothing running and succeeds without opening the store: a store that stays
// unreadable does not fail every later stop of the city.
func TestCmdStopSupervisorManagedCityWithNoControllerFinishesOnARerunOverAnUnreadableStore(t *testing.T) {
	fake := runningFake(t, "worker-1")
	cityDir, reg := unservedRegisteredCity(t, fake, "0s", 4242, nil)
	storeOpens := unreadableSessionStore(t)
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	var stdout, stderr lockedBuffer
	if code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 0, false, false); code != 1 {
		t.Fatalf("gc stop = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"could not check for sessions left creating by an interrupted start", "injected: store unreadable", "restored registration for 'supervised-city'"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q, want it to contain %q", stderr.String(), want)
		}
	}
	if storeOpens.Load() == 0 {
		t.Fatal("gc stop's own stop never opened the session store")
	}
	if running, _ := fake.ListRunning(""); len(running) != 0 {
		t.Fatalf("sessions %v still running after gc stop's own stop", running)
	}
	if entries := registryEntries(t, reg); len(entries) != 1 {
		t.Fatalf("registry %v, want the registration restored", entries)
	}

	opensBefore := storeOpens.Load()
	var rerunOut, rerunErr lockedBuffer
	if code := cmdStopJSON([]string{cityDir}, &rerunOut, &rerunErr, 0, false, false); code != 0 || !strings.Contains(rerunOut.String(), "City stopped.") {
		t.Fatalf("rerun of gc stop = %d, stdout %q, stderr %q; want City stopped.", code, rerunOut.String(), rerunErr.String())
	}
	if got := storeOpens.Load() - opensBefore; got != 0 {
		t.Errorf("the rerun opened the session store %d times, want none", got)
	}
	if entries := registryEntries(t, reg); len(entries) != 0 {
		t.Errorf("registry %v after the rerun, want the city unregistered", entries)
	}
}

// unreadableSessionStore makes the session store gc stop opens fail every
// listing of the city's sessions, as a gc-managed Dolt that is down or a
// proxied scope with a stale proxy record does, and counts the opens.
func unreadableSessionStore(t *testing.T) *atomic.Int32 {
	t.Helper()
	var opens atomic.Int32
	oldOpen := openCityStoreForStop
	openCityStoreForStop = func(cityPath string) (beads.Store, error) {
		opens.Add(1)
		store, err := oldOpen(cityPath)
		if err != nil {
			return nil, err
		}
		return failingListByLabelStore{Store: store, err: errors.New("injected: store unreadable")}, nil
	}
	t.Cleanup(func() { openCityStoreForStop = oldOpen })
	return &opens
}

// gc stop's own stop of a registered city no controller served grows the
// default wall-clock cap by its confirmation's bound before it confirms the
// stop. The default cap is sized for the stop alone; firing while the
// confirmation still lists a survivor, it would leave the city unregistered
// with its session rather than restore the registration. Under the
// operator's --timeout there is nothing to grow, and the stop is still
// confirmed.
func TestStopAndConfirmUnservedRegisteredCityGrowsTheDefaultCapForItsConfirmation(t *testing.T) {
	for _, c := range []struct {
		name       string
		defaultCap bool
	}{
		{"under the default cap", true},
		{"under --timeout", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := runningFake(t, "worker-1")
			fake.StopLeavesRunning = map[string]bool{"worker-1": true}
			cityDir, _ := unservedRegisteredCity(t, fake, "0s", 4242, nil)
			cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityDir, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			var mu sync.Mutex
			var grown time.Duration
			var grownAtListing []time.Duration
			stopProbeProvider(t, confirmListingWatcher{Fake: fake, listed: func() {
				mu.Lock()
				defer mu.Unlock()
				grownAtListing = append(grownAtListing, grown)
			}}, 100*time.Millisecond)
			var extendCap func(time.Duration)
			if c.defaultCap {
				extendCap = func(d time.Duration) {
					mu.Lock()
					defer mu.Unlock()
					grown += d
				}
			}

			var stdout, stderr lockedBuffer
			if code := stopAndConfirmUnservedRegisteredCity(cityDir, cfg, false, &stdout, &stderr, extendCap); code != 1 || !strings.Contains(stderr.String(), "worker-1 still running") {
				t.Fatalf("stop = %d, stderr %q; want 1 with the survivor worker-1 named", code, stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			want := time.Duration(0)
			if c.defaultCap {
				want = stopVerifyListTimeout + stopVerifyPollInterval
			}
			if len(grownAtListing) == 0 {
				t.Fatal("the stop was not confirmed against the runtime")
			}
			for i, got := range grownAtListing {
				if got != want {
					t.Fatalf("confirming listing %d ran with the cap grown by %s, want %s", i, got, want)
				}
			}
		})
	}
}

// gc stop's own stop of a registered city no controller served owns the
// controller lock the unregister claimed until the stop ends. The default
// wall-clock cap firing while that stop still runs (here, blocked in the bead
// store's stop) releases neither the lock nor the pending unregister: gc stop
// fails and leaves the city unregistered, saying so, rather than let the
// supervisor relaunch the city under the stop. The stop releases the lock
// when it ends, and the city stays unregistered. The report names the city's
// path and gives it in the command that finishes the stop: the city's name is
// no longer registered, so a city stopped by name cannot be stopped by that
// name again, and the printed command must work either way.
func TestCmdStopSupervisorManagedCityWithNoControllerHoldsItsStopPastTheCap(t *testing.T) {
	for _, c := range []struct {
		name   string
		byName bool
	}{
		{"stopped by its path", false},
		{"stopped by its registered name", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := runningFake(t, "worker-1")
			cityDir, reg := unservedRegisteredCity(t, fake, "10ms", 4242, nil)
			entries := registryEntries(t, reg)
			if len(entries) != 1 {
				t.Fatalf("registry %v, want the city registered", entries)
			}
			cityPath := entries[0].Path
			cityArg := cityDir
			if c.byName {
				cityArg = entries[0].EffectiveName()
			}
			// Size the default cap (defaultStopWallClockTimeout) at tens of
			// milliseconds, so it fires while the bead store's stop blocks.
			oldStop, oldMargin := stopPerTargetTimeoutDefault, interruptPerTargetTimeoutMargin
			stopPerTargetTimeoutDefault, interruptPerTargetTimeoutMargin = 10*time.Millisecond, 0
			restoreCap := func() { stopPerTargetTimeoutDefault, interruptPerTargetTimeoutMargin = oldStop, oldMargin }
			t.Cleanup(restoreCap)
			storeStopEntered := make(chan struct{}, 1)
			releaseStoreStop := make(chan struct{})
			overrideShutdownBeadsProviderForStop(t, func(string) error {
				storeStopEntered <- struct{}{}
				<-releaseStoreStop
				return nil
			})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseStoreStop) }) }
			stopWorker := captureStopWorker(t, release)

			var stdout, stderr lockedBuffer
			if code := cmdStopJSON([]string{cityArg}, &stdout, &stderr, 0, false, false); code != 1 {
				t.Fatalf("gc stop = %d, want 1 (timed out); stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			select {
			case <-storeStopEntered:
			case <-time.After(hangBudget):
				t.Fatal("gc stop's own stop never reached the bead store")
			}
			if !probeControllerLockHeld(t, cityDir) {
				t.Error("controller lock released while gc stop's own stop still runs")
			}
			if entries := registryEntries(t, reg); len(entries) != 0 {
				t.Errorf("registry %v while gc stop's own stop still runs, want the city left unregistered", entries)
			}
			finish := "run gc stop " + shellQuotePath(cityPath) + " to finish the stop"
			for _, want := range []string{"timed out after", "left 'supervised-city' (" + cityPath + ") unregistered", finish} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q, want it to contain %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), "restored registration") || strings.Contains(stdout.String(), "City stopped.") {
				t.Errorf("stdout %q, stderr %q; want neither success nor a restored registration", stdout.String(), stderr.String())
			}

			release()
			awaitClose(t, stopWorker(), "gc stop's own stop after its bead store stop returned")
			if probeControllerLockHeld(t, cityDir) {
				t.Error("controller lock still held after gc stop's own stop ended")
			}
			if entries := registryEntries(t, reg); len(entries) != 0 {
				t.Errorf("registry %v after gc stop's own stop ended, want the city still unregistered", entries)
			}

			// The printed command finishes the stop, under the default cap.
			restoreCap()
			var rerunOut, rerunErr lockedBuffer
			if code := cmdStopJSON([]string{cityPath}, &rerunOut, &rerunErr, 0, false, false); code != 0 || !strings.Contains(rerunOut.String(), "City stopped.") {
				t.Errorf("%s: gc stop = %d, stdout %q, stderr %q; want City stopped.", finish, code, rerunOut.String(), rerunErr.String())
			}
		})
	}
}

// A wall-clock cap that gave up on gc stop before its own stop of an unserved
// registered city began (here, while the unregister still waited for the
// supervisor's controller to stop) restored the registration, handing the
// city back to the supervisor. The own stop then does not begin: it stops
// neither the sessions nor the bead store of a city the supervisor may be
// relaunching, and the controller lock the unregister claimed is released.
func TestCmdStopSupervisorManagedCityWithNoControllerSkipsItsStopOnceTheCapHandedItBack(t *testing.T) {
	fake := runningFake(t, "worker-1")
	cityDir, reg := unservedRegisteredCity(t, fake, "0s", 4242, nil)
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		close(waitEntered)
		<-releaseWait
		return nil
	}
	var storeStops atomic.Int32
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		storeStops.Add(1)
		return nil
	})
	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	commandExited := make(chan struct{})
	var releaseOnce sync.Once
	releaseTheWait := func() { releaseOnce.Do(func() { close(releaseWait) }) }
	stopWorker := captureStopWorker(t, func() {
		releaseTheWait()
		select {
		case <-commandExited:
		case <-time.After(hangBudget):
			t.Errorf("gc stop did not return after the supervisor's controller wait ended")
		}
	})
	const testWallClockCap = 100 * time.Millisecond
	go func() {
		defer close(commandExited)
		stopDone <- cmdStopJSON([]string{cityDir}, &stdout, &stderr, testWallClockCap, false, false)
	}()
	awaitClose(t, waitEntered, "gc stop's wait for the supervisor's controller to stop")
	var code int
	select {
	case code = <-stopDone:
	case <-time.After(hangBudget):
		t.Fatalf("gc stop did not honor its wall-clock cap %s", testWallClockCap)
	}
	if code != 1 || !strings.Contains(stderr.String(), "restored registration for 'supervised-city'") {
		t.Fatalf("gc stop = %d, stderr %q; want the cap to fail it and restore the registration", code, stderr.String())
	}

	releaseTheWait()
	awaitClose(t, stopWorker(), "gc stop's sequence after the supervisor's controller wait ended")
	if running, _ := fake.ListRunning(""); len(running) != 1 {
		t.Errorf("sessions %v, want worker-1 left running for the supervisor that has the city back", running)
	}
	if got := storeStops.Load(); got != 0 {
		t.Errorf("bead store stopped %d times, want none", got)
	}
	if probeControllerLockHeld(t, cityDir) {
		t.Error("controller lock still held after gc stop's sequence ended")
	}
	if entries := registryEntries(t, reg); len(entries) != 1 {
		t.Errorf("registry %v, want the registration the cap restored", entries)
	}
}

// gc stop's own stop of a registered city no controller served runs only
// under the controller lock the unregister claimed once the supervisor had
// dropped the city. A supervisor that missed the unregister's probe (so the
// unregister waited on nothing and claimed no lock) but answers the next one
// proves nothing about a controller start already in flight, or already
// holding the lock: gc stop then does not stop the city's sessions. It only
// confirms the stop, as for a city a controller stopped, so a session still
// running fails it before the bead store is touched, and the registration is
// restored. A rerun finds the supervisor up and stops the city under the lock.
func TestCmdStopSupervisorManagedCityWithNoControllerOnlyConfirmsWithoutTheLock(t *testing.T) {
	for _, c := range []struct {
		name        string
		lockedStart bool
	}{
		{"the controller lock is free", false},
		{"a controller start holds the controller lock", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := runningFake(t, "worker-1")
			cityDir, reg := unservedRegisteredCity(t, fake, "0s", 4242, nil)
			// The supervisor misses the unregister's probe and answers every
			// later one.
			var probes atomic.Int32
			supervisorAliveHook = func() int {
				if probes.Add(1) == 1 {
					return 0
				}
				return 4242
			}
			var sessionStops, sessionStopsUnlocked, storeStops, storeStopsUnlocked atomic.Int32
			oldFactory := sessionProviderForStopCity
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
				sessionStops.Add(1)
				if !probeControllerLockHeld(t, cityDir) {
					sessionStopsUnlocked.Add(1)
				}
				return fake, nil
			}
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			overrideShutdownBeadsProviderForStop(t, func(string) error {
				storeStops.Add(1)
				if !probeControllerLockHeld(t, cityDir) {
					storeStopsUnlocked.Add(1)
				}
				return nil
			})
			if c.lockedStart {
				lock, err := acquireControllerLock(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}

			var stdout, stderr lockedBuffer
			if code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 0, false, false); code != 1 {
				t.Fatalf("gc stop = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if got := probes.Load(); got < 2 {
				t.Fatalf("supervisor probed %d times, want the unregister's probe and the next one", got)
			}
			if sessionStops.Load() != 0 || storeStops.Load() != 0 {
				t.Errorf("gc stop opened the runtime to stop sessions %d times and stopped the bead store %d times without the controller lock, want neither", sessionStops.Load(), storeStops.Load())
			}
			if running, _ := fake.ListRunning(""); len(running) != 1 {
				t.Errorf("sessions %v, want worker-1 left running", running)
			}
			// No controller stopped the sessions, and gc stop left them to a
			// rerun: the report says so rather than blame a controller stop
			// or the runtime.
			for _, want := range []string{
				"session(s) worker-1 still running 100ms after gc stop unregistered the city; no controller served the city, and gc stop did not stop them without its controller lock; run gc stop again",
				"restored registration for 'supervised-city'",
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q, want it to contain %q", stderr.String(), want)
				}
			}
			if strings.Contains(stderr.String(), "after the controller stopped") {
				t.Errorf("stderr %q blames a controller stop that never ran", stderr.String())
			}
			if strings.Contains(stdout.String(), "City stopped.") {
				t.Errorf("stdout %q, want no success", stdout.String())
			}
			if entries := registryEntries(t, reg); len(entries) != 1 {
				t.Fatalf("registry %v, want the registration restored", entries)
			}
			if c.lockedStart {
				return
			}

			var rerunOut, rerunErr lockedBuffer
			if code := cmdStopJSON([]string{cityDir}, &rerunOut, &rerunErr, 0, false, false); code != 0 {
				t.Fatalf("rerun of gc stop = %d, want 0; stdout=%q stderr=%q", code, rerunOut.String(), rerunErr.String())
			}
			if sessionStops.Load() == 0 || storeStops.Load() != 1 {
				t.Errorf("rerun opened the runtime to stop sessions %d times and stopped the bead store %d times, want its own stop to run once", sessionStops.Load(), storeStops.Load())
			}
			if sessionStopsUnlocked.Load() != 0 || storeStopsUnlocked.Load() != 0 {
				t.Errorf("rerun stopped sessions (%d) or the bead store (%d) without the controller lock held", sessionStopsUnlocked.Load(), storeStopsUnlocked.Load())
			}
			if running, _ := fake.ListRunning(""); len(running) != 0 {
				t.Errorf("sessions %v still running after the rerun", running)
			}
			if entries := registryEntries(t, reg); len(entries) != 0 {
				t.Errorf("registry %v after the rerun, want the city unregistered", entries)
			}
		})
	}
}

// A registered city whose supervisor is down (stopped with its sessions
// preserved, or crashed) takes the standalone stop once unregistered. There
// too a bead store that will not stop fails the stop and restores the
// registration, as on every path of a registered city, rather than print
// "City stopped." and leave the city unregistered with its store running
// unmanaged. An unregistered city's stop only warns.
func TestCmdStopRegisteredCityWithItsSupervisorDownFailsOnABeadStoreThatWillNotStop(t *testing.T) {
	for _, c := range []struct {
		name       string
		registered bool
		wantCode   int
		wantStderr string
	}{
		{"a registered city", true, 1, "restored registration for 'supervised-city'"},
		{"an unregistered city", false, 0, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := runningFake(t, "worker-1")
			cityDir, reg := unservedRegisteredCity(t, fake, "0s", 0, nil)
			if !c.registered {
				if err := reg.Unregister(cityDir); err != nil {
					t.Fatal(err)
				}
			}
			overrideShutdownBeadsProviderForStop(t, func(string) error { return errors.New("dolt still serving") })

			var stdout, stderr lockedBuffer
			code := cmdStopJSON([]string{cityDir}, &stdout, &stderr, 0, false, false)
			if code != c.wantCode {
				t.Fatalf("gc stop = %d, want %d; stdout=%q stderr=%q", code, c.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "gc stop: bead store: dolt still serving") || !strings.Contains(stderr.String(), c.wantStderr) {
				t.Errorf("stderr %q, want the store failure reported and %q", stderr.String(), c.wantStderr)
			}
			if running, _ := fake.ListRunning(""); len(running) != 0 {
				t.Errorf("sessions %v still running: the stop stops them before the bead store", running)
			}
			entries := registryEntries(t, reg)
			if c.registered && (len(entries) != 1 || strings.Contains(stdout.String(), "City stopped.")) {
				t.Errorf("registry %v, stdout %q; want the registration restored and no success", entries, stdout.String())
			}
			if !c.registered && (len(entries) != 0 || !strings.Contains(stdout.String(), "City stopped.")) {
				t.Errorf("registry %v, stdout %q; want the city stopped and still unregistered", entries, stdout.String())
			}
		})
	}
}

// The standalone stop's acknowledged-controller arm fails on a bead store
// that will not stop when the stop requires it (a registered city's), and
// only warns otherwise (an unregistered city's).
func TestCmdStopBodyAcknowledgedControllerStopFailsOnABeadStoreThatWillNotStopWhenRequired(t *testing.T) {
	for _, c := range []struct {
		name             string
		requireStoreStop bool
		wantCode         int
	}{
		{"an unregistered city only warns", false, 0},
		{"a registered city fails", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			cityDir, cfg := acknowledgedControllerStopCity(t, "ack-stop-store")
			startAcknowledgingStandaloneController(t, cityDir)
			stopProbeProvider(t, runningFake(t), time.Second)
			overrideShutdownBeadsProviderForStop(t, func(string) error { return errors.New("dolt still serving") })
			var stdout, stderr lockedBuffer
			code := cmdStopBodyWithoutSuccessExtendingCap(cityDir, cfg, false, &stdout, &stderr, nil, c.requireStoreStop)
			if code != c.wantCode || !strings.Contains(stderr.String(), "gc stop: bead store: dolt still serving") {
				t.Fatalf("stop = %d, stderr %q; want %d with the store failure reported", code, stderr.String(), c.wantCode)
			}
		})
	}
}

// unservedRegisteredCity is a city registered as "supervised-city" with the
// supervisor alive as supervisorPID (0: down) and no controller serving it.
// Its sessions run on sp, which both the stop and its confirmation use; its
// [daemon] shutdown_timeout is shutdownTimeout; reload, when set, runs as the
// supervisor's reload. It returns the city and the registry it is in.
func unservedRegisteredCity(t *testing.T, sp runtime.Provider, shutdownTimeout string, supervisorPID int, reload func()) (string, *supervisor.Registry) {
	t.Helper()
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	cityDir := filepath.Join(t.TempDir(), "supervised-city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"supervised\"\n\n[beads]\nprovider = \"file\"\n\n[daemon]\nshutdown_timeout = \"" + shutdownTimeout + "\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "supervised-city"); err != nil {
		t.Fatal(err)
	}
	withSupervisorTestHooks(t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			if reload != nil {
				reload()
			}
			return 0
		},
		func() int { return supervisorPID },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond, time.Millisecond)
	stopProbeProvider(t, sp, 100*time.Millisecond)
	oldFactory := sessionProviderForStopCity
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	return cityDir, reg
}

// registryEntries lists reg, failing the test when it cannot.
func registryEntries(t *testing.T, reg *supervisor.Registry) []supervisor.CityEntry {
	t.Helper()
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// captureStopWorker records the done channel of the bounded stop worker
// (stopBodyLifecycleHook), which the returned func reads once gc stop has
// returned. At cleanup it runs release, which must unblock the worker and
// return only once the gc stop that started it has, then waits for the
// worker to end, so it cannot outlive the test into another's stop hooks.
func captureStopWorker(t *testing.T, release func()) func() <-chan struct{} {
	t.Helper()
	oldHook := stopBodyLifecycleHook
	var bodyDone <-chan struct{}
	stopBodyLifecycleHook = func(done <-chan struct{}) { bodyDone = done }
	t.Cleanup(func() {
		release()
		if bodyDone != nil {
			select {
			case <-bodyDone:
			case <-time.After(hangBudget):
				t.Errorf("gc stop's worker did not end after its release")
			}
		}
		stopBodyLifecycleHook = oldHook
	})
	return func() <-chan struct{} { return bodyDone }
}
