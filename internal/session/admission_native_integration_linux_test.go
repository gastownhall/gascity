//go:build linux

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

type nativeIntegrationEntryHost struct{ nativeCapacityHost }

func (h nativeIntegrationEntryHost) Command(unit string) (runtime.CommandScope, error) {
	scope, err := h.nativeCapacityHost.Command(unit)
	if err != nil {
		return scope, err
	}
	exe, err := os.Executable()
	if err != nil {
		return scope, err
	}
	scope.EntryCommand = []string{"/usr/bin/env", "GC_NATIVE_SCOPE_ENTRY_HELPER=1", exe, "-test.run=^TestOwnedNativeScopeEntryHelper$", "--"}
	return scope, nil
}

// This child executes inside the real scope, using the same domain entry
// command as gc. The parent test binary is the fixture's immutable executable.
func TestOwnedNativeScopeEntryHelper(t *testing.T) {
	if os.Getenv("GC_NATIVE_SCOPE_ENTRY_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) < 9 || args[0] != "session" || args[1] != "capacity-enter" || args[6] != "--" {
		t.Fatal("invalid entry helper arguments")
	}
	if err := EnterNativeCapacityScope(context.Background(), args[2], args[3], args[4], args[5]); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Exec("/bin/sh", args[7:], os.Environ()); err != nil {
		t.Fatal(err)
	}
}

// This opt-in operational test uses one isolated tmux socket, a temporary
// MemStore/city and an owned sleep process. It never opens a real city DB,
// calls an LLM, touches the shared tmux server or runs the global observer.
func TestOwnedNativeScopeLinuxIntegration(t *testing.T) {
	if os.Getenv("GC_NATIVE_SCOPE_INTEGRATION") != "1" {
		t.Skip("requires explicit owned systemd-user scope integration")
	}
	city, c, _, _, store := nativeTestFixture(t, 0)
	host := nativeIntegrationEntryHost{newNativeCapacityHost()}
	identity, err := host.Identity()
	if err != nil {
		t.Fatal(err)
	}
	ledger := nativeCapacityLedger{Schema: nativeCapacitySchema, CityPath: city, Identity: identity, Seats: map[string]nativeCapacitySeat{}}
	data, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeCapacityPath(city), data, 0o600); err != nil {
		t.Fatal(err)
	}
	config := tmux.DefaultConfig()
	config.SocketName = fmt.Sprintf("gc142-owned-native-%d-%d", os.Getpid(), time.Now().UnixNano())
	sp := tmux.NewSeamBackedWithConfig(config)
	defer func() {
		if lifecycle, ok := sp.(runtime.ServerLifecycleProvider); ok {
			_ = lifecycle.TeardownServer()
		}
	}()
	mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(city))
	// The model stand-in exits after one second; its setsid child outlives
	// both the main command and tmux pane but remains inside the owned cgroup.
	command := "sh -c '/usr/bin/setsid /usr/bin/sleep 4 >/dev/null 2>&1 </dev/null & /usr/bin/sleep 1'"
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: command, WorkDir: city, Provider: "owned-sleep-test", BeadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitted, allowed, reason := admitCapacityStartWithHost(context.Background(), city, "worker", info.ID, time.Now(), sp, NewStore(beads.SessionStore{Store: store}), host)
	if !allowed {
		t.Fatal(reason)
	}
	if err := mgr.startRuntime(admitted, info.SessionName, runtime.Config{Command: command, WorkDir: city, Env: map[string]string{"GC_SESSION_ID": info.ID, "GC_TEMPLATE": "worker"}}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sp.Stop(info.SessionName) }()
	var seen nativeCapacitySeat
	// Poll is the existing bounded live-boundary condition waiter. It does
	// not invoke herdr; this fixture still owns only its isolated tmux scope.
	herdrtest.Poll(t, "actual scope witness", 2*time.Second, func() (bool, string) {
		if err := withNativeCapacityLedger(context.Background(), city, c, host, func(l *nativeCapacityLedger) error {
			refreshNativeSeats(l, host, time.Now())
			seen = l.Seats[info.ID]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return seen.State == "active" && seen.CgroupPath != "", fmt.Sprintf("state=%s cgroup=%q", seen.State, seen.CgroupPath)
	})
	if seen.State != "active" || !seen.LaunchFinalized || seen.CgroupPath == "" {
		t.Fatalf("actual scope never witnessed: state=%s finalized=%v", seen.State, seen.LaunchFinalized)
	}
	if observed, err := host.Observe(seen); err != nil || !observed.Populated {
		t.Fatalf("actual owned model not contained: populated=%v err=%v", observed.Populated, err)
	}
	herdrtest.Poll(t, "owned model stand-in exit", 2500*time.Millisecond, func() (bool, string) {
		running := sp.IsRunning(info.SessionName)
		return !running, fmt.Sprintf("pane running=%v", running)
	})
	if observed, err := host.Observe(seen); err != nil || !observed.Populated {
		t.Fatalf("detached child disappeared from owned accounting: populated=%v err=%v", observed.Populated, err)
	}
	// A lost provider row is not the release proof. Wait for the actual exact
	// cgroup to empty/remove; its durable seat is retired only by that proof.
	herdrtest.Poll(t, "completed actual model scope retirement", 7*time.Second, func() (bool, string) {
		retired := false
		if err := withNativeCapacityLedger(context.Background(), city, c, host, func(l *nativeCapacityLedger) error {
			refreshNativeSeats(l, host, time.Now())
			_, present := l.Seats[info.ID]
			retired = !present
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return retired, fmt.Sprintf("seat retired=%v", retired)
	})
	final := readNativeTestLedger(t, city)
	if len(final.Seats) != 0 || len(final.Retired) != 1 || final.Retired[0].SessionID != info.ID {
		t.Fatal("actual scope retirement missing")
	}
	// Prove the fixture did not bind to an external store path.
	if filepath.Dir(nativeCapacityPath(city)) != filepath.Join(city, ".gc/runtime/pool-capacity-admission") {
		t.Fatal("unexpected runtime path")
	}
	if _, ok := any(store).(*beads.MemStore); !ok {
		t.Fatal("non-memory store")
	}
}
