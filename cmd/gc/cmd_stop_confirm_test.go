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
	for _, c := range []struct {
		name     string
		running  []string
		wantCode int
	}{
		{"nothing running", nil, 0},
		{"a session survives", []string{"worker-1"}, 1},
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
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"supervised\"\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			reg := registryAt(t, gcHome)
			if err := reg.Register(cityDir, "supervised-city"); err != nil {
				t.Fatal(err)
			}
			withSupervisorTestHooks(t,
				func(_, _ io.Writer) int { return 0 },
				func(_, _ io.Writer) int { return 0 },
				func() int { return 4242 },
				func(string) (bool, string, bool) { return false, "", true },
				20*time.Millisecond, time.Millisecond)
			waitForSupervisorControllerStopHook = func(string, time.Duration) error { return nil }
			stopProbeProvider(t, runningFake(t, c.running...), time.Second)

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
			case c.wantCode != 0 && (len(entries) != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "worker-1 still running")):
				t.Fatalf("registry %v, stdout %q, stderr %q; want the registration restored, no success, the survivor named", entries, stdout.String(), stderr.String())
			}
		})
	}
}
