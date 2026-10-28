//go:build windows

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// listBlockingStore holds the first full-scan List until unblock, so a test
// can catch a caching store's reconcile pass in flight.
type listBlockingStore struct {
	beads.Store
	listing     chan struct{}
	release     chan struct{}
	listOnce    sync.Once
	releaseOnce sync.Once
	// releasedAt is when the held List went on, in Unix nanoseconds.
	releasedAt atomic.Int64
}

func newListBlockingStore(t *testing.T) *listBlockingStore {
	t.Helper()
	s := &listBlockingStore{
		Store:   beads.NewMemStore(),
		listing: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(s.unblock)
	return s
}

func (s *listBlockingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	held := false
	s.listOnce.Do(func() {
		held = true
		close(s.listing)
	})
	if held {
		<-s.release
		s.releasedAt.Store(time.Now().UnixNano())
	}
	return s.Store.List(query)
}

func (s *listBlockingStore) unblock() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// reconcilingCachingStore starts a caching store's reconciler over backing and
// returns once its first pass is blocked in List. The returned cancel is the
// city context's.
func reconcilingCachingStore(t *testing.T, backing *listBlockingStore) (*beads.CachingStore, context.CancelFunc) {
	t.Helper()
	cached := beads.NewCachingStoreForTest(backing, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cached.StartReconciler(ctx, beads.WithStaggerOff(), "")
	select {
	case <-backing.listing:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconciler never started a pass")
	}
	return cached, cancel
}

func managedCityOverStore(name string, store beads.Store, cancel context.CancelFunc, done chan struct{}, shutdownTimeout string) *managedCity {
	return &managedCity{
		name:   name,
		cancel: cancel,
		done:   done,
		cr: &CityRuntime{
			cfg:               &config.City{Daemon: config.DaemonConfig{ShutdownTimeout: shutdownTimeout}},
			sp:                runtime.NewFake(),
			rec:               events.Discard,
			stdout:            io.Discard,
			stderr:            io.Discard,
			forceStopShutdown: &atomic.Bool{},
			cs:                &controllerState{cityBeadStore: store},
		},
	}
}

func TestQuiesceCityStoreWorkWaitsForTheReconcilePassInFlight(t *testing.T) {
	backing := newListBlockingStore(t)
	cached, cancel := reconcilingCachingStore(t, backing)
	cancel()
	mc := managedCityOverStore("quiesce", wrapStoreWithBeadPolicies(cached, nil), cancel, nil, "")

	returned := make(chan error, 1)
	go func() {
		returned <- quiesceCityStoreWork(mc, io.Discard)
	}()
	select {
	case <-returned:
		t.Fatal("quiesceCityStoreWork returned while a reconcile pass was still reading")
	case <-time.After(time.Second):
	}
	backing.unblock()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("quiesceCityStoreWork: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("quiesceCityStoreWork did not return once the pass finished")
	}
}

func TestQuiesceCityStoreWorkGivesUpAtItsBound(t *testing.T) {
	previous := cityStoreQuiesceTimeout
	cityStoreQuiesceTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cityStoreQuiesceTimeout = previous })
	backing := newListBlockingStore(t)
	cached, cancel := reconcilingCachingStore(t, backing)
	cancel()
	mc := managedCityOverStore("stuck", cached, cancel, nil, "")

	var stderr bytes.Buffer
	started := time.Now()
	err := quiesceCityStoreWork(mc, &stderr)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("quiesceCityStoreWork took %s against a %s bound", elapsed, cityStoreQuiesceTimeout)
	}
	if err == nil {
		t.Fatal("quiesceCityStoreWork error = nil, want quiesce timeout")
	}
	if !strings.Contains(err.Error(), `city "stuck": bead store reconcile still running after 300ms`) {
		t.Fatalf("error = %q, want the stuck work details", err.Error())
	}
	if !strings.Contains(stderr.String(), `city "stuck": bead store reconcile still running after 300ms`) ||
		!strings.Contains(stderr.String(), "leaving the bead store provider running") {
		t.Fatalf("stderr = %q, want the incomplete-stop warning", stderr.String())
	}
}

// TestStopManagedCityStopsTheProviderOnlyAfterStoreWorkEnds pins the order on
// both of stopManagedCity's exits: the provider's stop (bd dolt stop, through
// the native provider and the recording fake bd) must not run while the city's
// reconcile pass is still reading, because on a bd-owned scope that read
// starts the server again.
func TestStopManagedCityStopsTheProviderOnlyAfterStoreWorkEnds(t *testing.T) {
	for _, tc := range []struct {
		name            string
		exited          bool
		shutdownTimeout string
	}{
		{name: "after the city exits", exited: true, shutdownTimeout: "5s"},
		{name: "after a forced stop", exited: false, shutdownTimeout: "20ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newNativeBdTest(t)
			warmNativeFakeBd(t, n)
			if err := os.WriteFile(filepath.Join(n.city, "city.toml"), []byte("[workspace]\nname = \"owned\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(n.city, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(n.city, ".beads", "metadata.json"), contract.MetadataState{
				Database:     "dolt",
				Backend:      "dolt",
				DoltMode:     "proxied-server",
				DoltDatabase: "hq",
			}); err != nil {
				t.Fatal(err)
			}
			if owned, err := cityScopeProviderOwned(n.city); err != nil || !owned {
				t.Fatalf("fixture is not provider-owned: (%t, %v)", owned, err)
			}
			t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(n.city))
			t.Setenv("BD_BIN", n.bdBin)
			t.Setenv(nativeFakeBdLogEnv, n.log)

			backing := newListBlockingStore(t)
			cached, cancel := reconcilingCachingStore(t, backing)
			done := make(chan struct{})
			if tc.exited {
				close(done)
			}
			mc := managedCityOverStore("owned", cached, cancel, done, tc.shutdownTimeout)

			stopped := make(chan struct{})
			var stderr bytes.Buffer
			go func() {
				_ = stopManagedCity(mc, n.city, &stderr)
				close(stopped)
			}()
			time.Sleep(3 * time.Second)
			n.requireArgs()
			backing.unblock()
			select {
			case <-stopped:
			case <-time.After(60 * time.Second):
				t.Fatal("stopManagedCity did not return once the pass finished")
			}
			calls := n.requireArgs([]string{"dolt", "stop"})
			if passEnded := time.Unix(0, backing.releasedAt.Load()); !calls[0].At.After(passEnded) {
				t.Fatalf("bd dolt stop ran at %s, before the reconcile pass went on at %s", calls[0].At.Format(time.RFC3339Nano), passEnded.Format(time.RFC3339Nano))
			}
		})
	}
}

// warmNativeFakeBd runs the fake bd once and waits for it, so the first run a
// test times is not slowed by a scan of the freshly copied executable.
func warmNativeFakeBd(t *testing.T, n *nativeBdTest) {
	t.Helper()
	cmd := exec.Command(n.bdBin, "version")
	cmd.Env = append(os.Environ(), nativeFakeBdLogEnv+"="+filepath.Join(t.TempDir(), "warm.jsonl"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("warming the fake bd: %v: %s", err, out)
	}
}
