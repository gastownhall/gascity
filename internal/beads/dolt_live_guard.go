package beads

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// inheritedGCDoltPort and inheritedBeadsDoltPort capture GC_DOLT_PORT /
// BEADS_DOLT_SERVER_PORT as present in the process environment at package init
// — i.e. the values the surrounding agent shell leaked in, BEFORE any test
// calls t.Setenv. liveDoltGuard compares the port bd is about to use against
// these startup snapshots so a test that deliberately points bd at a Dolt
// server it started itself (via an explicit env override) is never blocked,
// while a bare `go test` that merely inherited the shared city port is.
//
// Why this guard exists (ga-w2kh1r / city outage gm-phoxf): the gascity test
// suite forks the bd CLI, which resolves its Dolt server port from
// GC_DOLT_PORT / BEADS_DOLT_SERVER_PORT and otherwise derives an ephemeral
// per-city-path port (see examples/bd/assets/scripts/gc-beads-bd.sh
// allocate_port). When an agent runs a bare `go test ./...` from its own shell
// — instead of `make test` / scripts/test-local-parallel, which wrap go test
// in `env -i` that drops these vars — every bd fork inherits
// GC_DOLT_PORT=<live city port> and writes to the shared PRODUCTION Dolt
// server. 18+ parallel test workers pegged that server and stalled bd writes
// across every rig until the session was killed. Refusing the fork under
// `go test` closes the hole; the sanctioned harness is unaffected because
// env -i leaves these vars empty, so the snapshots are empty and the guard is
// inert. Modeled on internal/runtime/proctable.liveScanGuard (#2839), which
// refuses to scan the live /proc under go test for the same class of reason.
var (
	inheritedGCDoltPort    = strings.TrimSpace(os.Getenv("GC_DOLT_PORT"))
	inheritedBeadsDoltPort = strings.TrimSpace(os.Getenv("BEADS_DOLT_SERVER_PORT"))
)

// allowInheritedDoltEnvVar opts a test out of the live-Dolt guard. A test that
// genuinely owns the Dolt server named by an inherited GC_DOLT_PORT sets this
// (and is then responsible for that server's contents).
const allowInheritedDoltEnvVar = "GC_TEST_ALLOW_INHERITED_DOLT"

// liveDoltGuard returns an error when a `go test` run is about to fork a
// Dolt-backed command (bd or dolt) against a Dolt server port that was
// inherited from the surrounding agent shell rather than chosen by the test.
// finalEnv is the fully-resolved environment the command will run with; name
// is the executable being run. It is a no-op outside `go test`, for non-Dolt
// commands, and whenever no Dolt port was inherited at startup — which is the
// case for every CI and `make`/script run.
func liveDoltGuard(name string, finalEnv []string) error {
	if !testing.Testing() {
		return nil
	}
	if name != "bd" && name != "dolt" {
		return nil
	}
	if inheritedGCDoltPort == "" && inheritedBeadsDoltPort == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv(allowInheritedDoltEnvVar)) != "" {
		return nil
	}

	effective := envSliceValue(finalEnv, "GC_DOLT_PORT")
	if effective == "" {
		effective = envSliceValue(finalEnv, "BEADS_DOLT_SERVER_PORT")
	}
	if effective == "" {
		// No port in the resolved env: bd derives an ephemeral, per-city-path
		// port. Isolated and safe.
		return nil
	}
	if effective != inheritedGCDoltPort && effective != inheritedBeadsDoltPort {
		// The test overrode the port to a server it chose, not the leaked one.
		return nil
	}

	return fmt.Errorf("beads: refusing to exec %q under `go test` against Dolt "+
		"port %s inherited from the surrounding agent shell — this is the live "+
		"shared city Dolt server and tests must never write to it (ga-w2kh1r). "+
		"Run tests via `make test` / `scripts/test-local-parallel` (which drop "+
		"the port via env -i), unset GC_DOLT_PORT and BEADS_DOLT_SERVER_PORT, or "+
		"set %s=1 if this test genuinely owns the server",
		name, effective, allowInheritedDoltEnvVar)
}

// envSliceValue returns the value of key in an environ-style "K=V" slice, or ""
// if absent. The last matching entry wins, mirroring os/exec env semantics.
func envSliceValue(environ []string, key string) string {
	prefix := key + "="
	val := ""
	for _, e := range environ {
		if strings.HasPrefix(e, prefix) {
			val = e[len(prefix):]
		}
	}
	return strings.TrimSpace(val)
}

// setInheritedDoltPortsForTesting overrides the startup snapshots so the
// guard's own unit tests can exercise both the inherited-leak and
// test-owned-override paths. It returns a restore func. Mirrors proctable's
// SetScanRootForTesting.
func setInheritedDoltPortsForTesting(gcPort, beadsPort string) (restore func()) {
	prevGC, prevBeads := inheritedGCDoltPort, inheritedBeadsDoltPort
	inheritedGCDoltPort = strings.TrimSpace(gcPort)
	inheritedBeadsDoltPort = strings.TrimSpace(beadsPort)
	return func() {
		inheritedGCDoltPort = prevGC
		inheritedBeadsDoltPort = prevBeads
	}
}
