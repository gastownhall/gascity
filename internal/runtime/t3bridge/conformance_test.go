package t3bridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// t3BridgeConformanceSessionSeq disambiguates session names across separate
// newSession(t) calls that share the identical *testing.T — several
// runtimetest conformance cases (e.g. ListRunning_PrefixFiltering, the
// *_ConcurrentDistinctSessions family) call the factory 2-3 times from one
// subtest to obtain distinct sessions, so a name derived from t.Name() alone
// would collide. newSession is never invoked concurrently anywhere in the
// conformance suite (no t.Parallel(), no goroutines around factory calls), so
// a package-level counter needs no more than atomic increment/load to stay
// race-free across those sequential calls.
var t3BridgeConformanceSessionSeq int64

// t3BridgeConformanceNextSessionName advances the shared sequence and derives
// a new session name from it. Call exactly once per factory invocation, to
// build the startup envelope embedded in the returned runtime.Config.
func t3BridgeConformanceNextSessionName(t *testing.T) string {
	t.Helper()
	seq := atomic.AddInt64(&t3BridgeConformanceSessionSeq, 1)
	return t3BridgeConformanceSessionNameForSeq(t, seq)
}

// t3BridgeConformanceCurrentSessionName reads the sequence value without
// advancing it, reproducing the name t3BridgeConformanceNextSessionName most
// recently produced. providerledger's proof validator allows only one
// statement (a return) inside the inline conformance factory, so no
// intermediate variable can carry one generated name into both the startup
// envelope and the returned session name; Go evaluates a return statement's
// expressions left to right, so calling Next first (building the config) then
// Current (producing the returned name) yields the same string in both
// positions without one.
func t3BridgeConformanceCurrentSessionName(t *testing.T) string {
	t.Helper()
	seq := atomic.LoadInt64(&t3BridgeConformanceSessionSeq)
	return t3BridgeConformanceSessionNameForSeq(t, seq)
}

func t3BridgeConformanceSessionNameForSeq(t *testing.T, seq int64) string {
	t.Helper()
	replacer := strings.NewReplacer("/", "--", " ", "-")
	return fmt.Sprintf("gc-t3bridge-%s-%d", strings.ToLower(replacer.Replace(t.Name())), seq)
}

// t3BridgeConformanceConfig starts a fresh stateful t3BridgeTestServer and
// points t3bridge.NewSeamBacked at it via a hand-built StartupEnvelope
// supplied through GC_STARTUP_ENVELOPE.
//
// Assignment.BeadID is populated (so gc.bead metadata is non-empty and
// Stop's isPersistentAgent check takes the normal, non-persistent-agent
// branch) while Resume is left zero-valued and GC_BEAD is left unset (so
// Start's legacyNamed auto-defaulting sets Resume.AllowThreadReuse=true).
// Together these let a second Start for the same session name hit
// DecideThreadReuse's Reuse branch instead of erroring, matching t3bridge's
// intentional durable-reconnect design.
func t3BridgeConformanceConfig(t *testing.T, name string) runtime.Config {
	t.Helper()
	resetBridgeAuthCacheForTest(t)
	oldDefaults := defaultWSURLCandidates
	defaultWSURLCandidates = nil
	t.Cleanup(func() { defaultWSURLCandidates = oldDefaults })

	server := newStatefulT3BridgeTestServer(t)
	t.Cleanup(server.Close)

	t.Setenv("GC_EXEC_STATE_DIR", t.TempDir())
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_HOME", t.TempDir())
	t.Setenv("T3_WS_URL", server.wsURL())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())

	workDir := t.TempDir()
	envelope := StartupEnvelope{
		GC: GCSection{
			Agent:       "conformance-agent",
			Template:    "conformance-template",
			SessionName: name,
		},
		Runtime: RuntimeSection{
			WorkDir: workDir,
		},
		Assignment: AssignmentSection{
			BeadID: "conformance-bead-" + name,
		},
	}
	envJSON, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal startup envelope: %v", err)
	}

	return runtime.Config{
		WorkDir: workDir,
		Env: map[string]string{
			"GC_STARTUP_ENVELOPE": string(envJSON),
		},
	}
}

func TestT3Bridge_RunProviderConformance(t *testing.T) {
	runtimetest.RunProviderTestsWithOptions(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBacked(),
			t3BridgeConformanceConfig(caseT, t3BridgeConformanceNextSessionName(caseT)),
			t3BridgeConformanceCurrentSessionName(caseT)
	}, runtimetest.Options{
		DuplicateStartReconnects: true,
	})
}
