package runtimecontract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeScript writes an executable RPP shell script with the given op
// bodies and returns its path. Unlisted ops exit 2.
func writeScript(t *testing.T, stateDir string, bodies map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gc-runtime-golden")
	if err := os.WriteFile(path, []byte(ReferenceScript(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGolden(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	return writeScript(t, state, ReferenceBodies())
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
	mutants := []struct {
		target       Code
		op           string
		body         string
		whatItBreaks string
	}{
		{ReqProtocolHandshake, "protocol", `echo 'not json at all'`, "malformed handshake"},
		{ReqLifecycleStartRunning, "is-running", `echo false`, "session never reports running"},
		{ReqLifecycleDuplicateErr, "start", `cat >/dev/null; mkdir -p "$box"; for f in id created cmd0 procs; do : > "$box/$f"; done`, "duplicate start silently succeeds"},
		{ReqLifecycleStopNotRunning, "stop", `:`, "stop does not actually stop"},
		{ReqLifecycleStopIdempotent, "stop", `if [ -d "$box" ]; then rm -rf "$box"; else exit 1; fi`, "stop errors on a missing session"},
		{ReqLifecycleUnknownNotRunning, "is-running", `echo true`, "unknown session reports running"},
		{ReqConnectionExec, "exec", `cat >/dev/null; echo wrong`, "exec ignores the command and emits fixed output"},
		{ReqProvisionBoxWithoutAgent, "provision", `cat >/dev/null; mkdir -p "$box"`, "provision launches the agent (is-running true) instead of just the box"},

		{ReqObserveDeclaredOpsRespond, "observe", `exit 2`, "observe exits 2"},
		{ReqObserveAbsentNeverStarted, "observe", strings.Replace(goldenObserve, `echo '{"state":"absent"}'`, `echo '{"state":"unknown"}'`, 1), "a never-started name reads unknown"},
		{ReqObserveRunningWithIdentity, "observe", strings.Replace(goldenObserve, goldenObserveIdentity+";", `if [ -f "$box/seen" ]; then ident; else : > "$box/seen"; printf null; fi;`, 1), "identity written after the box"},
		{ReqObserveAbsentAfterStop, "stop", `:`, "stop leaves the box"},
		{ReqObserveObjectStable, "observe", strings.Replace(goldenObserve, `"$(cat "$box/id")"`, `"$(date +%s%N)"`, 1), "object derived from the time"},
		{ReqObserveAgentLiveness, "observe", strings.Replace(goldenObserve, `agent=dead;`, `agent=alive;`, 1), "agent always alive"},
		{ReqObserveInventoryComplete, "inventory", strings.Replace(goldenInventory, `sep=','; done;`, `sep=','; break; done;`, 1), "inventory drops every entry but the first"},
		{ReqObserveMetaIsIdentity, "set-meta", `v=$(cat); if [ -d "$box" ]; then printf %s "$v" > "$box/xmeta.$3"; fi`, "set-meta writes a store observe does not read"},
		{ReqObserveFreshOnlyRefuses, "start", strings.Replace(goldenStart, `if [ -n "$fresh" ]; then exit 73; fi; exit 1;`, `if [ -n "$fresh" ]; then rm -rf "$box"; else exit 1; fi;`, 1), "fresh_only recreates the box"},
		{ReqObserveKillObjectExact, "kill-object", `cat >/dev/null; rm -rf "$box"; echo killed`, "kill-object kills by name"},
		{ReqObserveAttachedIsStrict, "is-attached", `echo no`, "is-attached prints no"},
		{ReqObserveFaultsReadUnknown, "observe", strings.Replace(goldenObserve, faultGuard, `if [ "${GC_RPP_FAULT:-}" = unavailable ] && [ ! -d "$box" ]; then exit 1; fi; `, 1), "a fault reads the backend anyway"},
		{ReqObserveFaultsReadUnknown, "observe", strings.Replace(goldenObserve, faultGuard, `if [ "${GC_RPP_FAULT:-}" = unavailable ]; then echo '{"state":"absent"}'; exit 0; fi; `, 1), "a fault reads absent"},
		{ReqObserveWithinBound, "observe", `sleep 5; ` + goldenObserve, "observe sleeps 5s"},
		{ReqObserveCityScoped, "inventory", strings.Replace(goldenInventory, `for box in "$B"/*;`, `for box in "$S"/city*/*;`, 1), "inventory ignores the city"},
		{ReqObserveInventoryHasActivity, "inventory", strings.Replace(goldenInventory, `"last_activity":"2026-01-01T00:00:00Z"`, `"last_activity":null`, 1), "inventory reports no activity"},
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
		t.Run(string(m.target), func(t *testing.T) {
			state := t.TempDir()
			bodies := ReferenceBodies()
			if _, ok := bodies[m.op]; !ok {
				t.Fatalf("mutant op %q is not a golden op", m.op)
			}
			if m.body == bodies[m.op] {
				t.Fatalf("mutant (%s) is the golden body: its replacement did not apply", m.whatItBreaks)
			}
			bodies[m.op] = m.body
			path := writeScript(t, state, bodies)

			report, err := Run(context.Background(), path, Options{only: m.target})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			res := resultFor(t, report, m.target)
			if res.Status != StatusFail {
				t.Errorf("mutant (%s) did not fail %s: status=%s detail=%q — the check is vacuous",
					m.whatItBreaks, m.target, res.Status, res.Detail)
			}
		})
	}
}

// A pack that declares proc.provision cannot answer provision with exit 2:
// the exec provider then fails the provision, so conformance must fail it too
// rather than skip it as an optional op left unimplemented.
func TestDeclaredProvisionExit2Fails(t *testing.T) {
	bodies := ReferenceBodies()
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
		bodies := ReferenceBodies()
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
