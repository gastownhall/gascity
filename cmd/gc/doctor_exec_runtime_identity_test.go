package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// Kills (PAR-EXEC §5 item 8, §12.2; review S-A) an exec-runtime-identity
// check that judges a box otherwise than v2 does: each running box under an
// open session row is judged by compareIdentity, on its batched identity or,
// when that is null, its observed one, and a box v2 holds (Unknown,
// Ownerless) is named; a Current box (the row's token, with or without a
// session id, batched or observed) and another row's box (Foreign) are not;
// and a city on another runtime, an exec runtime that is not v2-ready, or a
// city pinned to legacy is not checked.
func TestExecRuntimeIdentityCheckJudgesBoxesAsV2Does(t *testing.T) {
	obj := func(id string) string { return `"object":{"id":"` + id + `","created":"t"}` }
	inventory := `echo '{"complete":true,"sessions":[` +
		`{"name":"s-gc-1","state":"running",` + obj("a") + `,"identity":null},` + // observe answers the row's token: Current
		`{"name":"s-gc-2","state":"running",` + obj("b") + `,"identity":null},` + // observe answers null too: Unknown
		`{"name":"s-gc-3","state":"running",` + obj("c") + `,"identity":{"GC_INSTANCE_TOKEN":"tok-3"}},` + // the row's token, no session id: Current
		`{"name":"s-gc-4","state":"running",` + obj("d") + `,"identity":{"GC_SESSION_ID":"gc-4"}},` + // its session id, no token: Unknown
		`{"name":"s-gc-5","state":"running",` + obj("e") + `,"identity":{}},` + // no identity: Ownerless
		`{"name":"s-gc-6","state":"running",` + obj("f") + `,"identity":{"GC_SESSION_ID":"gc-9","GC_INSTANCE_TOKEN":"tok-9"}},` + // another row's: Foreign
		`{"name":"s-stray","state":"running",` + obj("g") + `,"identity":null}]}'`
	ready := writeRPPScript(t, `case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  inventory) `+inventory+` ;;
  observe) cat >/dev/null
    case "$2" in
      s-gc-1) echo '{"state":"running",`+obj("a")+`,"identity":{"GC_SESSION_ID":"gc-1","GC_INSTANCE_TOKEN":"tok-1"}}' ;;
      *) echo '{"state":"running",`+obj("x")+`,"identity":null}' ;;
    esac ;;
  *) exit 2 ;;
esac
`)
	rows := censusStore(
		poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"),
		poolRow("gc-2", "worker", 2, "active", "instance_token", "tok-2"),
		poolRow("gc-3", "worker", 3, "active", "instance_token", "tok-3"),
		poolRow("gc-4", "worker", 4, "active", "instance_token", "tok-4"),
		poolRow("gc-5", "worker", 5, "active", "instance_token", "tok-5"),
		poolRow("gc-6", "worker", 6, "active", "instance_token", "tok-6"),
	)
	check := func(provider, reconciler string) *doctor.CheckResult {
		cfg := &config.City{Session: config.SessionConfig{Provider: provider}, Daemon: config.DaemonConfig{SessionReconciler: reconciler}}
		c := newExecRuntimeIdentityCheck(cfg, t.TempDir())
		c.openLegs = func() []v2SessionLeg { return []v2SessionLeg{{ref: "city", store: rows}} }
		c.provider = func() (runtime.Provider, error) {
			return sessionexec.NewSeamBacked(strings.TrimPrefix(provider, "exec:")), nil
		}
		return c.Run(&doctor.CheckContext{})
	}
	r := check("exec:"+ready, "")
	details := strings.Join(r.Details, "\n")
	if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "3 running box(es)") ||
		!strings.Contains(details, "identity unknown: s-gc-2 (row gc-2)") || !strings.Contains(details, "identity unknown: s-gc-4 (row gc-4)") ||
		!strings.Contains(details, "Ownerless: s-gc-5 (row gc-5)") {
		t.Fatalf("check = %v %q %q; want s-gc-2 and s-gc-4 (Unknown) and s-gc-5 (Ownerless)", r.Status, r.Message, details)
	}
	for _, current := range []string{"s-gc-1", "s-gc-3", "s-gc-6", "s-stray"} {
		if strings.Contains(details, current+" ") {
			t.Errorf("details %q name %s, which v2 does not hold", details, current)
		}
	}
	if !strings.Contains(r.FixHint, "stop those sessions") {
		t.Fatalf("fix hint %q, want the §12.2 step", r.FixHint)
	}
	gapped := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1"]}'; exit 0; }; exit 2
`)
	for _, c := range []struct{ provider, reconciler string }{{"tmux", ""}, {"exec:" + gapped, ""}, {"exec:" + ready, "legacy"}} {
		if r := check(c.provider, c.reconciler); r.Status != doctor.StatusOK {
			t.Errorf("%s (session_reconciler %q): %v %q, want OK (v2 does not run on it)", c.provider, c.reconciler, r.Status, r.Message)
		}
	}
}
