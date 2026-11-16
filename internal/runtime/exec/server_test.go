package exec

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// serverPack is the golden reference, which declares server-lifecycle, with
// bodies overriding its ops, behind a wrapper that logs every op it is sent.
type serverPack struct {
	sp    *seamBackedProvider
	opLog string
}

func newServerPack(t *testing.T, bodies map[string]string) serverPack {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	all := reference.Bodies()
	all["exec"] = `cat >/dev/null`
	for op, body := range bodies {
		all[op] = body
	}
	golden := filepath.Join(dir, "golden")
	if err := os.WriteFile(golden, []byte(reference.Script(stateDir, all)), 0o755); err != nil {
		t.Fatal(err)
	}
	opLog := filepath.Join(dir, "ops.log")
	wrapper := writeScript(t, dir, opLogPrelude(opLog)+"exec '"+golden+"' \"$@\"\n")
	return serverPack{sp: seamBacked(NewProviderForCity(wrapper, "server-city", "/server-city")), opLog: opLog}
}

// undeclaredServerProtocol is the golden reference's handshake without
// server-lifecycle.
const undeclaredServerProtocol = `echo '{"version":0,"capabilities":["observe.v1","report-attachment","report-activity","conformance-faults","city-scoped"]}'`

// serverStartConfig starts a box that needs no startup dialog read.
func serverStartConfig(t *testing.T) runtime.Config {
	return runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", ProcessNames: []string{"sleep"}, AcceptStartupDialogs: new(bool)}
}

// The production constructor serves the server lifecycle, so gc stop and the
// controller's shutdown reach it.
func TestSeamBackedExecIsAServerLifecycleProvider(t *testing.T) {
	if _, ok := NewSeamBacked("/nonexistent").(runtime.ServerLifecycleProvider); !ok {
		t.Fatal("the seam-backed exec provider does not implement runtime.ServerLifecycleProvider")
	}
}

// Kills a ConfigureServer that sends an op: gc never calls it at boot, and a
// pack brings its server up itself, so it is a no-op that spawns nothing,
// not even the handshake.
func TestConfigureServerIsANoOp(t *testing.T) {
	p := newServerPack(t, nil)
	for range 2 {
		if err := p.sp.ConfigureServer(); err != nil {
			t.Fatalf("ConfigureServer = %v, want nil", err)
		}
	}
	if ops := readOpLog(t, p.opLog); len(ops) != 0 {
		t.Fatalf("ConfigureServer spawned %v, want nothing", ops)
	}
}

// Kills a TeardownServer that sends server-stop to a pack that did not
// declare server-lifecycle: an undeclared pack keeps the behavior it had before the token, no
// op and nil, even when it happens to answer server-stop.
func TestTeardownServerNilWithoutToken(t *testing.T) {
	t.Run("handshake without the token", func(t *testing.T) {
		p := newServerPack(t, map[string]string{"protocol": undeclaredServerProtocol, "server-stop": `echo "must not run" >&2; exit 1`})
		if err := p.sp.TeardownServer(); err != nil {
			t.Fatalf("TeardownServer = %v, want nil", err)
		}
		if ops := readOpLog(t, p.opLog); slices.Contains(ops, "server-stop") {
			t.Fatalf("TeardownServer spawned %v, want no server-stop", ops)
		}
	})
	t.Run("v0 pack", func(t *testing.T) {
		f := newV0PackFixture(t)
		if err := f.sp.TeardownServer(); err != nil {
			t.Fatalf("TeardownServer = %v, want nil", err)
		}
		if ops := readOpLog(t, f.opLog); !slices.Equal(ops, []string{"protocol"}) {
			t.Fatalf("TeardownServer spawned %v, want only the handshake", ops)
		}
	})
}

// Kills a TeardownServer that does not run server-stop for a declared pack,
// runs it more than once, or leaves a session running.
func TestTeardownServerRunsServerStop(t *testing.T) {
	p := newServerPack(t, nil)
	for _, name := range []string{"a", "b"} {
		if err := p.sp.Start(t.Context(), name, serverStartConfig(t)); err != nil {
			t.Fatalf("Start %s: %v", name, err)
		}
	}
	readOpLog(t, p.opLog)
	if err := p.sp.TeardownServer(); err != nil {
		t.Fatalf("TeardownServer = %v, want nil", err)
	}
	ops := readOpLog(t, p.opLog)
	if n := strings.Count(strings.Join(ops, " "), "server-stop"); n != 1 {
		t.Fatalf("TeardownServer spawned %v, want server-stop once", ops)
	}
	for _, name := range []string{"a", "b"} {
		if p.sp.IsRunning(name) {
			t.Errorf("%s still running after TeardownServer", name)
		}
	}
}

// Kills a declared server-stop's failure read as success: exit 2 from a
// declared op is a failure, not the unknown-op no-op, and exit 1 carries the
// pack's account of what may survive.
func TestTeardownServerReportsFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"exit 2 from a declared op", `exit 2`, "server-stop"},
		{"exit 1 naming a survivor", `echo "session a may survive: backend wedged" >&2; exit 1`, "session a may survive: backend wedged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newServerPack(t, map[string]string{"server-stop": tc.body})
			err := p.sp.TeardownServer()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("TeardownServer = %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// Kills a TeardownServer that reads a failed handshake as an undeclared
// pack: the provider keeps the failure for handshakeRetryAfter, so a stop
// soon after it would leave a declared pack's server, and its sessions,
// running with no word. Teardown runs one fresh handshake and tears down
// the server the pack declares; a handshake that fails again is an error
// naming it, and no server-stop is sent.
func TestTeardownServerAfterFailedHandshake(t *testing.T) {
	declared := reference.Bodies()["protocol"]
	t.Run("transient failure", func(t *testing.T) {
		up := filepath.Join(t.TempDir(), "backend-up")
		p := newServerPack(t, map[string]string{"protocol": `if [ ! -f '` + up + `' ]; then echo "backend starting" >&2; exit 1; fi; ` + declared})
		if err := p.sp.Start(t.Context(), "a", serverStartConfig(t)); err != nil {
			t.Fatalf("Start a: %v", err)
		}
		if _, err := p.sp.raw.Protocol(); err == nil {
			t.Fatal("the handshake answered before the backend was up; the test proves nothing")
		}
		if err := os.WriteFile(up, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		readOpLog(t, p.opLog)
		if err := p.sp.TeardownServer(); err != nil {
			t.Fatalf("TeardownServer = %v, want nil once the handshake answers", err)
		}
		if ops := readOpLog(t, p.opLog); !slices.Equal(ops, []string{"protocol", "server-stop"}) {
			t.Fatalf("TeardownServer spawned %v, want one fresh handshake, then server-stop", ops)
		}
		if p.sp.IsRunning("a") {
			t.Error("a still running after TeardownServer")
		}
	})
	t.Run("handshake keeps failing", func(t *testing.T) {
		p := newServerPack(t, map[string]string{"protocol": `echo "backend down" >&2; exit 1`})
		err := p.sp.TeardownServer()
		if err == nil || !strings.Contains(err.Error(), "backend down") {
			t.Fatalf("TeardownServer = %v, want an error carrying the handshake's failure", err)
		}
		if ops := readOpLog(t, p.opLog); slices.Contains(ops, "server-stop") {
			t.Fatalf("TeardownServer spawned %v, want no server-stop for a pack whose handshake failed", ops)
		}
	})
}

// TestExecServerTeardownContract runs RUNTIME-CONTRACT-023 against the
// production seam-backed exec provider over the golden reference: a box
// the server takes with it, a second teardown, and a pack whose server-stop
// reports a box it cannot end.
func TestExecServerTeardownContract(t *testing.T) {
	runtimetest.RunServerTeardownContract(t, runtimetest.ServerTeardownFixtures{
		Serving: func(t *testing.T) runtime.Provider { return newServerPack(t, nil).sp },
		Unstoppable: func(t *testing.T) (runtime.Provider, string) {
			p := newServerPack(t, map[string]string{"server-stop": `be || exit 1; echo "survivor: box would not stop" >&2; exit 1`})
			if err := p.sp.Start(t.Context(), "survivor", serverStartConfig(t)); err != nil {
				t.Fatal(err)
			}
			return p.sp, "survivor"
		},
		StartConfig: serverStartConfig,
	})
}
