package main

import (
	"context"
	"strings"
	"testing"
	"time"

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
// and a city whose config does not resolve to v2 is not checked: another
// runtime, an exec runtime that is not v2-ready, a legacy pin, an unset key
// that falls back to legacy, or an explicit v2 the latch refuses.
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
	check := func(provider, reconciler string, mutate func(*config.City)) *doctor.CheckResult {
		cfg := v2DefaultCity(func(c *config.City) {
			c.Session.Provider, c.Daemon.SessionReconciler = provider, reconciler
			if mutate != nil {
				mutate(c)
			}
		})
		c := newExecRuntimeIdentityCheck(cfg, t.TempDir(), nil, nil)
		c.openLegs = func() []v2SessionLeg { return []v2SessionLeg{{ref: "city", store: rows}} }
		c.provider = func() (runtime.Provider, error) {
			return sessionexec.NewSeamBacked(strings.TrimPrefix(provider, "exec:")), nil
		}
		return c.Run(&doctor.CheckContext{})
	}
	r := check("exec:"+ready, "", nil)
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
	dependsOn := func(c *config.City) { c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}}} }
	for _, c := range []struct {
		name, provider, reconciler string
		mutate                     func(*config.City)
		why                        string // in the message: why v2 does not run
	}{
		{"another runtime", "tmux", "", nil, "not an exec runtime"},
		{"not v2-ready", "exec:" + gapped, "", nil, "legacy (default fallback"},
		{"pinned legacy", "exec:" + ready, "legacy", nil, "legacy (set by [daemon] session_reconciler)"},
		{"unset key, default fallback", "exec:" + ready, "", dependsOn, "legacy (default fallback"},
		{"explicit v2 the latch refuses", "exec:" + ready, "v2", dependsOn, "is refused"},
	} {
		if r := check(c.provider, c.reconciler, c.mutate); r.Status != doctor.StatusOK || !strings.Contains(r.Message, c.why) {
			t.Errorf("%s: %v %q, want OK naming %q (v2 does not run on it)", c.name, r.Status, r.Message, c.why)
		}
	}
}

// acpInventoryLeg is a composite's ACP leg serving an inventory of its own:
// every listed name has an empty identity, which would read Ownerless.
type acpInventoryLeg struct {
	runtime.Provider
	err error // the inventory's failure, nil here
}

func (l acpInventoryLeg) RuntimeInventory(context.Context) (map[string]runtime.InventoryEntry, error) {
	return map[string]runtime.InventoryEntry{
		"s-gc-1": {Incarnation: "acp:1", IdentityKnown: true, Identity: map[string]string{}},
		"s-gc-2": {Incarnation: "acp:2", IdentityKnown: true, Identity: map[string]string{}},
	}, l.err
}

func (acpInventoryLeg) InventoryAvailable() bool { return true }

// composite is a provider whose backends are given in order.
type composite struct {
	runtime.Provider
	backends []runtime.Backend
}

func (c composite) Backends() []runtime.Backend { return c.backends }

// exec identity fixture: an exec pack whose inventory lists s-gc-1 (the row's
// token: Current) and s-gc-2 (identity null, and observe answers null:
// Unknown).
func identityFixture(t *testing.T) (script string, rows func() []v2SessionLeg) {
	t.Helper()
	obj := func(id string) string { return `"object":{"id":"` + id + `","created":"t"}` }
	script = writeRPPScript(t, `case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  inventory) echo '{"complete":true,"sessions":[{"name":"s-gc-1","state":"running",`+obj("a")+`,"identity":{"GC_SESSION_ID":"gc-1","GC_INSTANCE_TOKEN":"tok-1"}},{"name":"s-gc-2","state":"running",`+obj("b")+`,"identity":null},{"name":"s-gc-3","state":"running",`+obj("c")+`,"identity":null}]}' ;;
  observe) cat >/dev/null; echo '{"state":"running",`+obj("x")+`,"identity":null}' ;;
  *) exit 2 ;;
esac
`)
	store := censusStore(
		poolRow("gc-1", "worker", 1, "active", "instance_token", "tok-1"),
		poolRow("gc-2", "worker", 2, "active", "instance_token", "tok-2"),
		poolRow("gc-3", "worker", 3, "active", "instance_token", "tok-3"),
	)
	return script, func() []v2SessionLeg { return []v2SessionLeg{{ref: "city", store: store}} }
}

// execIdentityCity is a city on script whose unset session_reconciler resolves
// to v2, so the identity check runs.
func execIdentityCity(script string) *config.City {
	return v2DefaultCity(func(c *config.City) { c.Session.Provider = "exec:" + script })
}

// Kills (review SA8) an identity check that reads the first leg with an
// inventory: under a composite whose ACP leg comes first, the exec leg's
// inventory is the one judged (s-gc-1 Current, s-gc-2 Unknown), not the ACP
// leg's (which would read both Ownerless).
func TestExecRuntimeIdentityCheckReadsTheExecLeg(t *testing.T) {
	script, legs := identityFixture(t)
	execLeg := sessionexec.NewSeamBacked(script)
	sp := composite{Provider: execLeg, backends: []runtime.Backend{{Label: "acp", Provider: acpInventoryLeg{Provider: runtime.NewFake()}}, {Label: "default", Provider: execLeg}}}
	c := newExecRuntimeIdentityCheck(execIdentityCity(script), t.TempDir(), sp, nil)
	c.openLegs = legs
	r := c.Run(&doctor.CheckContext{})
	details := strings.Join(r.Details, "\n")
	if !strings.Contains(details, "identity unknown: s-gc-2 (row gc-2)") || strings.Contains(details, "Ownerless") || strings.Contains(details, "s-gc-1 ") {
		t.Fatalf("check %q %q; want the exec leg judged: s-gc-2 Unknown, nothing Ownerless", r.Message, details)
	}
}

// Kills (review item 2) an unbounded per-box observe: once the observe
// budget is spent, the boxes whose batched identity is null are reported not
// checked, and the check warns; boxes judged on their batch still are.
func TestExecRuntimeIdentityCheckBoundsItsObserves(t *testing.T) {
	saved := execIdentityObserveBudget
	t.Cleanup(func() { execIdentityObserveBudget = saved })
	execIdentityObserveBudget = 0
	script, legs := identityFixture(t)
	start := time.Now()
	c := newExecRuntimeIdentityCheck(execIdentityCity(script), t.TempDir(), sessionexec.NewSeamBacked(script), nil)
	c.openLegs = legs
	r := c.Run(&doctor.CheckContext{})
	details := strings.Join(r.Details, "\n")
	if r.Status != doctor.StatusWarning || !strings.Contains(details, "not checked (observe budget spent): s-gc-2 (row gc-2)") ||
		!strings.Contains(details, "not checked (observe budget spent): s-gc-3 (row gc-3)") || strings.Contains(details, "identity unknown") ||
		!strings.Contains(r.Message, "2 box(es) not checked") {
		t.Fatalf("check %v %q %q; want s-gc-2 and s-gc-3 not checked and a warning", r.Status, r.Message, details)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the check took %s with its budget spent", time.Since(start))
	}
	// Without a provider, nothing is checked, and doctor says why.
	none := newExecRuntimeIdentityCheck(execIdentityCity(script), t.TempDir(), nil, errSimBackendDown)
	if r := none.Run(&doctor.CheckContext{}); r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "not checked") {
		t.Fatalf("no provider: %v %q, want not checked", r.Status, r.Message)
	}
}
