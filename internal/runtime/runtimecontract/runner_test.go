package runtimecontract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

// writeScript writes an executable RPP shell script with the given op
// bodies and returns its path. Unlisted ops exit 2.
func writeScript(t *testing.T, stateDir string, bodies map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gc-runtime-golden")
	if err := os.WriteFile(path, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGolden(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	return writeScript(t, state, reference.Bodies())
}

func runGolden(t *testing.T, path string) Report {
	t.Helper()
	report, err := Run(context.Background(), path, Options{})
	if err != nil {
		t.Fatalf("Run(%s): %v", path, err)
	}
	return report
}

func TestProbesCoverCatalogExactly(t *testing.T) {
	catalogCodes := map[Code]bool{}
	for _, req := range catalog {
		catalogCodes[req.Code] = true
		if _, ok := probes[req.Code]; !ok {
			t.Errorf("catalog requirement %s has no probe", req.Code)
		}
	}
	for code := range probes {
		if !catalogCodes[code] {
			t.Errorf("probe %s has no catalog requirement", code)
		}
	}
}

func TestCatalogCodesAreUnique(t *testing.T) {
	seen := map[Code]bool{}
	for _, req := range catalog {
		if seen[req.Code] {
			t.Errorf("duplicate catalog code %s", req.Code)
		}
		seen[req.Code] = true
		if req.Title == "" {
			t.Errorf("%s has no Title", req.Code)
		}
	}
}

func TestRunCoversEveryCatalogRequirementInOrder(t *testing.T) {
	report := runGolden(t, writeGolden(t))
	if len(report.Results) != len(catalog) {
		t.Fatalf("got %d results, want one per catalog requirement (%d)", len(report.Results), len(catalog))
	}
	for i, req := range catalog {
		if report.Results[i].Code != req.Code {
			t.Errorf("result[%d].Code = %s, want %s (catalog order)", i, report.Results[i].Code, req.Code)
		}
	}
}

func TestGoldenReferenceIsFullyConformant(t *testing.T) {
	report := runGolden(t, writeGolden(t))
	if report.Failed() {
		for _, res := range report.Results {
			if res.Status == StatusFail {
				t.Errorf("golden failed %s: %s", res.Code, res.Detail)
			}
		}
		t.Fatalf("golden reference must pass every requirement; summary=%+v", report.Summary)
	}
	if report.Summary.Passed != len(catalog) {
		t.Errorf("golden passed %d/%d (skipped %d) — the golden must exercise every required behavior",
			report.Summary.Passed, len(catalog), report.Summary.Skipped)
	}
}

// TestEveryRequirementIsGated is the negative-gating proof: for each
// requirement, a reference that violates exactly that behavior must FAIL
// exactly that requirement's check. This is what upgrades "probed" to
// "guaranteed" — a vacuous probe would not catch its mutant.
func TestEveryRequirementIsGated(t *testing.T) {
	g := reference.Bodies()
	// edit is golden op's body with from replaced by to; a replacement that
	// does not apply fails the test, so no mutant is silently the golden.
	edit := func(op, from, to string) map[string]string {
		if !strings.Contains(g[op], from) {
			t.Fatalf("mutant of %s: %q is not in the golden body", op, from)
		}
		return map[string]string{op: strings.Replace(g[op], from, to, 1)}
	}
	set := func(op, body string) map[string]string { return map[string]string{op: body} }
	mutants := []struct {
		target       Code
		bodies       map[string]string
		whatItBreaks string
	}{
		{ReqProtocolHandshake, set("protocol", `echo 'not json at all'`), "malformed handshake"},
		{ReqLifecycleStartRunning, set("is-running", `echo false`), "session never reports running"},
		{ReqLifecycleDuplicateErr, set("start", `cat >/dev/null; mkdir -p "$box"; for f in id created cmd0 procs; do : > "$box/$f"; done`), "duplicate start silently succeeds"},
		{ReqLifecycleStopNotRunning, set("stop", `:`), "stop does not actually stop"},
		{ReqLifecycleStopIdempotent, set("stop", `if [ -d "$box" ]; then rm -rf "$box"; else exit 1; fi`), "stop errors on a missing session"},
		{ReqLifecycleUnknownNotRunning, set("is-running", `echo true`), "unknown session reports running"},
		{ReqConnectionExec, set("exec", `cat >/dev/null; echo wrong`), "exec ignores the command and emits fixed output"},
		{ReqProvisionBoxWithoutAgent, set("provision", `cat >/dev/null; mkdir -p "$box"`), "provision launches the agent (is-running true) instead of just the box"},

		{ReqObserveDeclaredOpsRespond, set("observe", `exit 2`), "observe exits 2"},
		{ReqObserveAbsentNeverStarted, edit("observe", `echo '{"state":"absent"}'`, `echo '{"state":"unknown"}'`), "a never-started name reads unknown"},
		{
			ReqObserveRunningWithIdentity, set("start", strings.Replace(strings.Replace(g["start"],
				`printf %s "$v" > "$box.tmp/meta.$k"; fi; done; `, `printf %s "$v" > "$S/pending.$k"; fi; done; `, 1),
				`mv "$box.tmp" "$box"`, `mv "$box.tmp" "$box"; sleep 1; for f in "$S"/pending.*; do [ -f "$f" ] && mv "$f" "$box/meta.${f##*pending.}"; done`, 1)),
			"identity written after the box, before start returns (the design's mutant)",
		},
		{ReqObserveRunningWithIdentity, edit("observe", `printf ',"identity":'; ident;`, `printf ',"identity":{}';`), "an empty identity (observe)"},
		{ReqObserveAbsentAfterStop, set("stop", `:`), "stop leaves the box"},
		{ReqObserveObjectStable, edit("observe", `"$(cat "$box/id")"`, `"$(date +%s%N)"`), "object derived from the time"},
		{ReqObserveObjectStable, edit("observe", `printf '{"state":"running","object":{"id":"%s","created":"%s"}' "$(cat "$box/id")" "$(cat "$box/created")";`, `printf '{"state":"running"';`), "a running box without its object"},
		{ReqObserveObjectStable, edit("observe", `echo '}'`, `echo '} {}'`), "trailing bytes after the answer"},
		{ReqObserveAgentLiveness, edit("observe", `agent=dead;`, `agent=alive;`), "agent always alive"},
		{ReqObserveAgentLiveness, edit("observe", `then agent=dead;`, `then agent="";`), "agent omitted for a process the box does not run"},
		{ReqObserveInventoryComplete, edit("inventory", `sep=','; done;`, `sep=','; break; done;`), "inventory drops every entry but the first"},
		{ReqObserveInventoryComplete, edit("inventory", `ident; printf '}';`, `printf '{}'; printf '}';`), "inventory identity disagrees with observe's"},
		{ReqObserveInventoryComplete, map[string]string{
			"stop":       `if [ -d "$box" ]; then : > "$box/stopped"; fi`,
			"is-running": `if [ -d "$box" ] && [ ! -f "$box/stopped" ]; then echo true; else echo false; fi`,
			"observe":    `if [ -f "$box/stopped" ]; then cat >/dev/null; echo '{"state":"absent"}'; exit 0; fi; ` + g["observe"],
		}, "a stopped box is still listed"},
		{ReqObserveMetaIsIdentity, set("set-meta", `v=$(cat); if [ -d "$box" ]; then printf %s "$v" > "$box/xmeta.$3"; fi`), "set-meta writes a store observe does not read"},
		{ReqObserveFreshOnlyRefuses, edit("start", `if [ -n "$fresh" ]; then exit 73; fi; exit 1;`, `if [ -n "$fresh" ]; then rm -rf "$box"; else exit 1; fi;`), "fresh_only recreates the box"},
		{ReqObserveFreshOnlyRefuses, edit("start", `if [ -n "$fresh" ]; then exit 73; fi;`, `if [ -n "$fresh" ]; then printf 'obj-x' > "$box/id"; exit 73; fi;`), "a refused fresh_only start changes the box"},
		{ReqObserveFreshOnlyRefuses, edit("start", `if [ -n "$fresh" ]; then exit 73; fi;`, `if [ -n "$fresh" ]; then printf %s "$cfg" | sed -n 's/.*"GC_INSTANCE_TOKEN":"\([^"]*\)".*/\1/p' > "$box/meta.GC_INSTANCE_TOKEN"; exit 73; fi;`), "a refused fresh_only start writes its token to the box"},
		{ReqObserveKillObjectExact, set("kill-object", `cat >/dev/null; rm -rf "$box"; echo killed`), "kill-object kills by name"},
		{ReqObserveKillObjectExact, edit("kill-object", ` || [ "$(cat "$box/created")" != "$cr" ]`, ``), "kill-object ignores the object's creation time"},
		{ReqObserveKillObjectExact, edit("kill-object", `corpse) echo live ;;`, `corpse) rm -rf "$box"; echo killed ;;`), "corpse mode kills a running box"},
		{ReqObserveKillObjectExact, edit("kill-object", `if alive "$box"; then echo changed; else`, `if false; then echo changed; else`), "zombie mode kills a box whose agent lives"},
		{ReqObserveKillObjectExact, edit("kill-object", `rm -rf "$box"; echo killed;`, `echo changed;`), "zombie mode never kills a dead agent's box"},
		{ReqObserveAttachedIsStrict, set("is-attached", `echo no`), "is-attached prints no"},
		{ReqObserveFaultsReadUnknown, edit("observe", `be || exit 1;`, `be || true;`), "observe swallows the failed backend read"},
		{ReqObserveFaultsReadUnknown, edit("observe", `be || exit 1;`, `be || { echo '{"state":"absent"}'; exit 0; };`), "a fault reads absent"},
		{ReqObserveFaultsReadUnknown, edit("inventory", `be || exit 1;`, `be || { echo '{"complete":true,"sessions":[]}'; exit 0; };`), "a fault reads as a complete empty inventory"},
		{ReqObserveFaultsReadUnknown, edit("is-attached", `be || exit 1;`, `be || echo false;`), "is-attached answers false when the backend fails (|| echo false)"},
		{ReqObserveWithinBound, set("observe", `sleep 5; `+g["observe"]), "observe sleeps 5s"},
		{ReqObserveCityScoped, edit("inventory", `for box in "$B"/*;`, `for box in "$S"/city*/*;`), "inventory ignores the city"},
		{ReqObserveCityScoped, edit("inventory", `for box in "$B"/*;`, `for box in "$B"/none*;`), "inventory omits its own city's box"},
		{ReqObserveInventoryHasActivity, edit("inventory", `"last_activity":"2026-01-01T00:00:00Z"`, `"last_activity":null`), "inventory reports no activity"},
		{ReqObservePeekNoScreen, edit("peek", `exit 66;`, `exit 0;`), "peek answers no box with an empty success"},
		{ReqObservePeekNoScreen, edit("peek", `exit 66;`, `exit 1;`), "peek fails for no box instead of answering no screen"},
		{ReqObservePeekNoScreen, set("peek", `exit 66`), "peek answers no screen for a running box"},
		{ReqObservePeekNoScreen, set("peek", `exit 2`), "peek is not implemented"},
	}
	// Every catalog requirement must have a mutant — otherwise a new
	// requirement could ship ungated.
	covered := map[Code]bool{}
	for _, m := range mutants {
		covered[m.target] = true
	}
	for _, req := range catalog {
		if !covered[req.Code] {
			t.Errorf("requirement %s has no negative-gating mutant", req.Code)
		}
	}

	for _, m := range mutants {
		t.Run(string(m.target)+"/"+m.whatItBreaks, func(t *testing.T) {
			bodies := reference.Bodies()
			for op, body := range m.bodies {
				if _, ok := bodies[op]; !ok {
					t.Fatalf("mutant op %q is not a golden op", op)
				}
				bodies[op] = body
			}
			path := writeScript(t, t.TempDir(), bodies)
			report, err := run(context.Background(), path, Options{}, func(c Code) bool { return c == m.target })
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			res := resultFor(t, report, m.target)
			if res.Status != StatusFail {
				t.Errorf("mutant (%s) did not fail %s: status=%s detail=%q — the check is vacuous",
					m.whatItBreaks, m.target, res.Status, res.Detail)
			}
			if report.V2Ready {
				t.Errorf("mutant (%s) failed %s yet reads v2-ready", m.whatItBreaks, m.target)
			}
		})
	}
}

// A pack that declares proc.provision cannot answer provision with exit 2:
// the exec provider then fails the provision, so conformance must fail it too
// rather than skip it as an optional op left unimplemented.
func TestDeclaredProvisionExit2Fails(t *testing.T) {
	bodies := reference.Bodies()
	bodies["protocol"] = `echo '{"version":0,"capabilities":["proc.exec","proc.provision"]}'`
	bodies["provision"] = `cat >/dev/null; exit 2`
	report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := resultFor(t, report, ReqProvisionBoxWithoutAgent); res.Status != StatusFail {
		t.Errorf("declared provision exiting 2: status=%s detail=%q, want FAIL", res.Status, res.Detail)
	}

	delete(bodies, "provision")
	bodies["protocol"] = `echo '{"version":0,"capabilities":[]}'`
	report, err = Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := resultFor(t, report, ReqProvisionBoxWithoutAgent); res.Status != StatusSkip {
		t.Errorf("undeclared provision exiting 2: status=%s detail=%q, want SKIP", res.Status, res.Detail)
	}
}

func resultFor(t *testing.T, report Report, code Code) Result {
	t.Helper()
	for _, res := range report.Results {
		if res.Code == code {
			return res
		}
	}
	t.Fatalf("no result for %s", code)
	return Result{}
}

func TestRunErrorsOnMissingExecutable(t *testing.T) {
	if _, err := Run(context.Background(), filepath.Join(t.TempDir(), "nope"), Options{}); err == nil {
		t.Fatal("Run must error when the executable cannot be resolved")
	}
}

// Kills an observe group that runs for a script not declaring observe.v1,
// or a token-gated requirement that runs without its token: the golden's
// ops are all there, so only the gates can make these skip.
func TestObserveGroupSkipsWhatIsNotDeclared(t *testing.T) {
	for protocol, wantSkip := range map[string]map[Code]bool{
		`{"version":0,"capabilities":["report-attachment","report-activity","conformance-faults","city-scoped"]}`: nil,
		`{"version":0,"capabilities":["observe.v1"]}`: {
			ReqObserveAttachedIsStrict: true, ReqObserveFaultsReadUnknown: true,
			ReqObserveCityScoped: true, ReqObserveInventoryHasActivity: true,
		},
	} {
		bodies := reference.Bodies()
		bodies["protocol"] = `echo '` + protocol + `'`
		report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range report.Results {
			if res.Group != GroupObserve {
				continue
			}
			skip := wantSkip == nil || wantSkip[res.Code]
			if (res.Status == StatusSkip) != skip {
				t.Errorf("handshake %s: %s = %s (%s), want skip %v", protocol, res.Code, res.Status, res.Detail, skip)
			}
		}
	}
}

// Kills (SHOULD 3) a v2-ready verdict read from the handshake alone, or
// gaps named wrongly: only the golden, which declares both capabilities
// and fails nothing, reads v2-ready.
func TestV2ReadinessInTheReport(t *testing.T) {
	for _, tc := range []struct {
		name, protocol string
		ready          bool
		missing        []string
	}{
		{"golden", "", true, nil},
		{"v0 (no protocol op)", `exit 2`, false, []string{"observe.v1", "report-attachment"}},
		{"observe.v1 only", `echo '{"version":0,"capabilities":["observe.v1","conformance-faults","city-scoped"]}'`, false, []string{"report-attachment"}},
		{"report-attachment only", `echo '{"version":0,"capabilities":["report-attachment"]}'`, false, []string{"observe.v1"}},
	} {
		bodies := reference.Bodies()
		if tc.protocol != "" {
			bodies["protocol"] = tc.protocol
		}
		report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
		if err != nil {
			t.Fatal(err)
		}
		if report.V2Ready != tc.ready || strings.Join(report.V2Missing, ",") != strings.Join(tc.missing, ",") || len(report.V2Failed) != 0 {
			t.Errorf("%s: v2-ready %v, missing %v, failed %v; want %v, %v, none", tc.name, report.V2Ready, report.V2Missing, report.V2Failed, tc.ready, tc.missing)
		}
	}
}
