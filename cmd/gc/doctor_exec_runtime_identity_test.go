package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// Kills (PAR-EXEC §5 item 8, §12.2) an exec-runtime-identity check that
// misses a box v2 would hold, or flags one it adopts: on a v2-ready exec
// runtime, a running box under an open session row whose identity is null,
// or carries no session id, is counted and named; a box with its identity,
// a box under no open row, and a city on another runtime or an exec runtime
// that is not v2-ready are not.
func TestExecRuntimeIdentityCheckCountsBoxesV2WouldHold(t *testing.T) {
	inventory := `echo '{"complete":true,"sessions":[` +
		`{"name":"s-gc-1","state":"running","object":{"id":"a","created":"t"},"identity":null},` +
		`{"name":"s-gc-2","state":"running","object":{"id":"b","created":"t"},"identity":{"GC_INSTANCE_TOKEN":"tok"}},` +
		`{"name":"s-gc-3","state":"running","object":{"id":"c","created":"t"},"identity":{"GC_SESSION_ID":"gc-3"}},` +
		`{"name":"s-stray","state":"running","object":{"id":"d","created":"t"},"identity":null}]}'`
	ready := writeRPPScript(t, `case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  inventory) `+inventory+` ;;
  *) exit 2 ;;
esac
`)
	rows := censusStore(poolRow("gc-1", "worker", 1, "active"), poolRow("gc-2", "worker", 2, "active"), poolRow("gc-3", "worker", 3, "active"))
	check := func(provider string) *doctor.CheckResult {
		cfg := &config.City{Session: config.SessionConfig{Provider: provider}}
		c := newExecRuntimeIdentityCheck(cfg, t.TempDir())
		c.openLegs = func() []v2SessionLeg { return []v2SessionLeg{{ref: "city", store: rows}} }
		c.provider = func() (runtime.Provider, error) {
			return sessionexec.NewSeamBacked(strings.TrimPrefix(provider, "exec:")), nil
		}
		return c.Run(&doctor.CheckContext{})
	}
	r := check("exec:" + ready)
	details := strings.Join(r.Details, "\n")
	if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "2 running box(es)") ||
		!strings.Contains(details, "identity null: s-gc-1 (row gc-1)") || !strings.Contains(details, "no session id (Ownerless): s-gc-2 (row gc-2)") ||
		strings.Contains(details, "s-gc-3") || strings.Contains(details, "s-stray") {
		t.Fatalf("check = %v %q %q; want s-gc-1 (identity null) and s-gc-2 (Ownerless) only", r.Status, r.Message, details)
	}
	if !strings.Contains(r.FixHint, "stop those sessions") {
		t.Fatalf("fix hint %q, want the §12.2 step", r.FixHint)
	}
	gapped := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1"]}'; exit 0; }; exit 2
`)
	for _, provider := range []string{"tmux", "exec:" + gapped} {
		if r := check(provider); r.Status != doctor.StatusOK {
			t.Errorf("%s: %v %q, want OK (v2 does not run on it)", provider, r.Status, r.Message)
		}
	}
}
