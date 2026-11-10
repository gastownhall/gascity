package runtimecontract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
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

// writeGolden writes the full golden reference: the host-filesystem
// variant, which declares every token the catalog gates, so it exercises
// every requirement.
func writeGolden(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	return writeScript(t, state, reference.HostFSBodies())
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
	// hedit is edit over the host-filesystem golden, the base of the fshost
	// group's mutants.
	h := reference.HostFSBodies()
	hedit := func(op, from, to string) map[string]string {
		if !strings.Contains(h[op], from) {
			t.Fatalf("mutant of %s: %q is not in the host-filesystem golden body", op, from)
		}
		return map[string]string{op: strings.Replace(h[op], from, to, 1)}
	}
	type mutant struct {
		target       Code
		bodies       map[string]string
		whatItBreaks string
	}
	mutants := []mutant{
		{ReqProtocolHandshake, set("protocol", `echo 'not json at all'`), "malformed handshake"},
		{ReqLifecycleStartRunning, set("is-running", `echo false`), "session never reports running"},
		{ReqLifecycleDuplicateErr, set("start", `cat >/dev/null; mkdir -p "$box"; for f in id created cmd0 procs; do : > "$box/$f"; done`), "duplicate start silently succeeds"},
		{ReqLifecycleStopNotRunning, set("stop", `:`), "stop does not actually stop"},
		{ReqLifecycleStopIdempotent, set("stop", `if [ -d "$box" ]; then rm -rf "$box"; else exit 1; fi`), "stop errors on a missing session"},
		{ReqLifecycleUnknownNotRunning, set("is-running", `echo true`), "unknown session reports running"},
		{ReqConnectionExec, set("exec", `cat >/dev/null; echo wrong`), "exec ignores the command and emits fixed output"},
		{ReqConnectionIdleBoundary, edit("exec", `has-session) [ -d "$box" ] ;;`, `has-session) return 1 ;;`), "idle-boundary declared but the box has no tmux session main"},
		{ReqConnectionIdleBoundary, edit("exec", `capture-pane) [ -d "$box" ] && cat "$box/cmd0" ;;`, `capture-pane) return 1 ;;`), "idle-boundary declared but the pane cannot be captured"},
		{ReqConnectionIdleBoundary, set("exec", `exit 2`), "idle-boundary declared but exec is unimplemented"},
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
		{ReqObserveAttachedUnattachedFalse, set("is-attached", `be || exit 1; if [ -d "$box" ]; then echo true; else echo false; fi`), "a running box with no client reads attached"},
		{ReqObserveAttachedMissingFalse, edit("is-attached", `echo false`, `if [ ! -d "$box" ]; then echo "no such session" >&2; exit 1; fi; echo false`), "is-attached fails for a name no box holds"},
		{ReqObserveAttachedMissingFalse, edit("is-attached", `echo false`, `if [ -d "$box" ]; then echo false; fi`), "is-attached answers nothing for a name no box holds"},

		{ReqMetaRoundTrip, set("set-meta", `cat >/dev/null`), "set-meta drops the value"},
		{ReqMetaUnsetKeyEmpty, edit("get-meta", `cat "$box/meta.$3"; fi`, `cat "$box/meta.$3"; else echo "no such key" >&2; exit 1; fi`), "get-meta of an unset key fails"},
		{ReqMetaUnsetKeyEmpty, edit("get-meta", `cat "$box/meta.$3"; fi`, `cat "$box/meta.$3"; else echo unset; fi`), "get-meta of an unset key prints a placeholder"},
		{ReqMetaRemoveThenEmpty, set("remove-meta", `:`), "remove-meta removes nothing"},
		{ReqMetaEmptyValueOverwrites, edit("set-meta", `if [ -d "$box" ];`, `if [ -n "$v" ] && [ -d "$box" ];`), "set-meta skips an empty value"},
		{ReqMetaOverwritesPrevious, edit("set-meta", `if [ -d "$box" ];`, `if [ -d "$box" ] && [ ! -f "$box/meta.$3" ];`), "set-meta keeps the first value"},
		{ReqMetaMultipleKeysIndependent, edit("set-meta", `printf %s "$v" > "$box/meta.$3"`, `rm -f "$box"/meta.gc-rpp-*; printf %s "$v" > "$box/meta.$3"`), "set-meta of one key drops the others"},

		{ReqDiscoveryFindsSessions, edit("list-running", `echo "$n" ;; esac; done`, `echo "$n" ;; esac; break; done`), "list-running stops after the first box"},
		{ReqDiscoveryPrefixFiltering, edit("list-running", `case "$n" in "$name"*) echo "$n" ;; esac;`, `echo "$n";`), "list-running ignores the prefix"},
		{ReqDiscoveryExcludesStopped, map[string]string{
			"start":        g["start"] + `; echo "$name" >> "$S/started"`,
			"list-running": `grep "^$name" "$S/started" 2>/dev/null; true`,
		}, "list-running lists every name ever started"},
		{ReqDiscoveryEmptyPrefix, edit("list-running", `be || exit 1; `, `be || exit 1; [ -n "$name" ] || exit 0; `), "list-running with an empty prefix lists nothing"},
		{ReqDiscoveryFailureIsNotAbsence, edit("list-running", `be || exit 1;`, `be || exit 0;`), "a failed listing reads as an empty one"},
		{ReqDiscoveryFailureIsNotAbsence, edit("list-running", `be || exit 1;`, `be || exit 2;`), "a failed listing answers exit 2 (unknown op)"},

		{ReqProcessEmptyNamesAlive, edit("process-alive", `elif [ -z "$names" ]; then echo true;`, `elif [ -z "$names" ]; then echo false;`), "no process names reads dead"},
		{ReqProcessFalseAfterStop, edit("process-alive", `if [ ! -d "$box" ]; then echo false;`, `if [ ! -d "$box" ]; then echo true;`), "a stopped session reads alive"},
		{ReqProcessFalseAfterStop, edit("process-alive", `if [ ! -d "$box" ]; then echo false;`, `if [ ! -d "$box" ]; then :;`), "a stopped session answers empty, which gc reads as alive"},

		{ReqConcurrencyStart, set("start", exclusive(g["start"])), "start refuses while another start runs"},
		{ReqConcurrencyStop, set("stop", exclusive(g["stop"])), "stop refuses while another stop runs"},
		{ReqConcurrencyIsRunning, set("is-running", exclusive(g["is-running"])), "is-running refuses while another runs"},
		{ReqConcurrencyProcessAlive, set("process-alive", exclusive(g["process-alive"])), "process-alive refuses while another runs"},
		{ReqConcurrencyListRunning, set("list-running", exclusive(g["list-running"])), "list-running refuses while another runs"},

		{ReqServerStopIdempotent, set("server-stop", `if [ -f "$S/server-stopped" ]; then exit 1; fi; : > "$S/server-stopped"; rm -rf "$B"`), "server-stop exits 1 the second time"},
		{ReqServerStopIdempotent, edit("server-stop", `rm -rf "$B"`, `if [ ! -d "$B" ] || [ -z "$(ls "$B")" ]; then echo "no server running" >&2; exit 1; fi; rm -rf "$B"`), "server-stop fails when no server runs"},
		{ReqServerStopIdempotent, set("server-stop", `exit 2`), "declared server-stop is not implemented"},
		{ReqServerStartAfterStop, edit("server-stop", `rm -rf "$B"`, `rm -rf "$B"; : > "$B"`), "server-stop leaves the city unable to start a box"},
		{ReqServerStopEndsSession, set("server-stop", `:`), "server-stop leaves every box and exits 0"},
		{ReqServerStopEndsSession, set("server-stop", `rm -rf "$(ls -d "$B"/* | head -n 1)"`), "server-stop ends only one box and exits 0"},
		{ReqServerStopSparesOtherCities, edit("server-stop", `rm -rf "$B"`, `rm -rf "$S"/city*`), "server-stop ends every city's sessions"},
	}
	// hostFSMutants edit HostFSBodies, the base of the fshost group.
	hostFSMutants := []mutant{
		{ReqFSHostSharedView, hedit("start", `if [ -n "$cmd" ]; then`, `if [ -n "$cmd" ]; then mkdir -p "$S/private"; cp -R "$wd"/. "$S/private"/; cmd=$(printf %s "$cmd" | sed "s|$wd|$S/private|g"); `), "runs the command in a private copy of the work dir"},
		{ReqFSHostSharedView, hedit("start", `if [ -n "$cmd" ]; then`, `if false; then`), "never runs the command"},
		{ReqFSHostSameUser, hedit("start", `if [ -n "$cmd" ]; then`, `if [ -n "$cmd" ]; then cmd=$(printf %s "$cmd" | sed 's/id -u/echo 4242/'); `), "records another uid"},
		{ReqFSHostLocal, set("protocol", `echo '{"version":0,"capabilities":["observe.v1","report-attachment","fs.host"],"locality":"remote"}'`), "declares fs.host with a remote locality"},
		{ReqFSHostLocal, set("protocol", `echo '{"version":0,"capabilities":["observe.v1","report-attachment","fs.host"]}'`), "declares fs.host with no locality"},
		{ReqFSHostRunsWhatItCarries, hedit("start", `if [ -n "$od" ] && [ -n "$wd" ]; then`, `if false; then`), "skips the overlay_dir its config carries"},
		{ReqFSHostRunsWhatItCarries, hedit("start", `if [ -n "$ps" ]; then`, `if false; then`), "skips the pre_start its config carries"},
		{ReqFSHostRunsWhatItCarries, hedit("start", `|| { rm -rf "$box.tmp"; echo "pre_start failed" >&2; exit 1; }`, `|| true`), "swallows a failing pre_start"},
		{ReqFSHostRunsWhatItCarries, hedit("start", `ps=$(printf %s "$cfg" | sed -n 's/.*"pre_start":\["\([^"]*\)"\].*/\1/p'); `, `ps=$(printf %s "$cfg" | sed -n 's/.*"pre_start":\["\([^"]*\)"\].*/\1/p'); if [ -n "$ps" ]; then printf %s "$ps" > "$S/last-pre-start"; elif [ -f "$S/last-pre-start" ]; then ps=$(cat "$S/last-pre-start"); fi; `), "runs a remembered pre_start its config does not carry"},
		{ReqFSHostRunsWhatItCarries, hedit("start", `if [ -n "$od" ] && [ -n "$wd" ]; then`, `if [ -n "$od" ]; then printf %s "$od" > "$S/last-overlay"; elif [ -f "$S/last-overlay" ]; then od=$(cat "$S/last-overlay"); fi; if [ -n "$od" ] && [ -n "$wd" ]; then`), "stages a remembered overlay_dir its config does not carry"},
		{ReqFSHostAgentPIDOnHost, hedit("observe", `"$(cat "$box/pid")"`, `999999999`), "reports a pid absent on the host (another pid namespace)"},
		{ReqFSHostAgentPIDOnHost, hedit("observe", `if [ -f "$box/pid" ]; then printf ',"agent_pid"`, `if false; then printf ',"agent_pid"`), "reports no agent_pid"},
		{ReqFSHostHomeView, hedit("start", `if [ -n "$cmd" ]; then`, `if [ -n "$cmd" ]; then mkdir -p "$S/home"; cmd=$(printf %s "$cmd" | sed "s|$HOME|$S/home|g"); `), "runs the command with a home directory of its own at the caller's HOME (a container bind-mounting only the work dir)"},
	}
	// Every catalog requirement must have a mutant — otherwise a new
	// requirement could ship ungated.
	type gated struct {
		mutant
		hostFS bool
	}
	var all []gated
	for _, m := range mutants {
		all = append(all, gated{m, false})
	}
	for _, m := range hostFSMutants {
		all = append(all, gated{m, true})
	}
	covered := map[Code]bool{}
	for _, m := range all {
		covered[m.target] = true
	}
	for _, req := range catalog {
		if !covered[req.Code] {
			t.Errorf("requirement %s has no negative-gating mutant", req.Code)
		}
	}

	for _, m := range all {
		t.Run(string(m.target)+"/"+m.whatItBreaks, func(t *testing.T) {
			bodies := reference.Bodies()
			if m.hostFS {
				bodies = reference.HostFSBodies()
			}
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

// exclusive is body behind a script-wide lock it refuses to wait for,
// held a second so concurrent calls overlap: an op that fails instead of
// serving distinct names at once, the concurrency group's mutant.
func exclusive(body string) string {
	return `mkdir "$S/excl" 2>/dev/null || { echo "busy" >&2; exit 1; }; trap 'rmdir "$S/excl"' EXIT; sleep 1; ` + body
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
// ops are all there, so only the gates can make these skip. The
// error-bearing attachment answer (RPP-OBSERVE-017..018) is gated by
// report-attachment alone, so it runs without observe.v1.
func TestObserveGroupSkipsWhatIsNotDeclared(t *testing.T) {
	attachOnly := map[Code]bool{ReqObserveAttachedUnattachedFalse: true, ReqObserveAttachedMissingFalse: true}
	allButAttach := map[Code]bool{}
	for _, req := range catalog {
		if req.Group == GroupObserve && !attachOnly[req.Code] {
			allButAttach[req.Code] = true
		}
	}
	for protocol, wantSkip := range map[string]map[Code]bool{
		`{"version":0,"capabilities":["report-attachment","report-activity","conformance-faults","city-scoped"]}`: allButAttach,
		`{"version":0,"capabilities":["observe.v1"]}`: {
			ReqObserveAttachedIsStrict: true, ReqObserveFaultsReadUnknown: true,
			ReqObserveCityScoped: true, ReqObserveInventoryHasActivity: true,
			ReqObserveAttachedUnattachedFalse: true, ReqObserveAttachedMissingFalse: true,
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
			skip := wantSkip[res.Code]
			if (res.Status == StatusSkip) != skip {
				t.Errorf("handshake %s: %s = %s (%s), want skip %v", protocol, res.Code, res.Status, res.Detail, skip)
			}
		}
	}
}

// RPP-CONN-002 gates on the declaration: a script that does not declare
// idle-boundary SKIPs even when its box has no tmux session, and one that
// declares it without the capabilities it takes effect with still has its
// box checked, and is told the declaration has no effect.
// Kills: a probe gated on behavior (a failing box read as undeclared), and a
// declared idle-boundary passed without reading the box.
func TestIdleBoundaryGatesOnTheDeclaration(t *testing.T) {
	noSession := func(bodies map[string]string) {
		const from = `has-session) [ -d "$box" ] ;;`
		if !strings.Contains(bodies["exec"], from) {
			t.Fatalf("golden exec body has no %q to break", from)
		}
		bodies["exec"] = strings.Replace(bodies["exec"], from, `has-session) return 1 ;;`, 1)
	}

	bodies := reference.Bodies()
	noSession(bodies)
	bodies["protocol"] = `echo '{"version":0,"capabilities":["observe.v1","report-attachment","report-activity","conformance-faults","city-scoped"]}'`
	report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res := resultFor(t, report, ReqConnectionIdleBoundary); res.Status != StatusSkip {
		t.Errorf("undeclared idle-boundary over a box with no tmux session: %s (%s), want SKIP", res.Status, res.Detail)
	}

	bodies = reference.Bodies()
	report, err = Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
	if err != nil {
		t.Fatal(err)
	}
	res := resultFor(t, report, ReqConnectionIdleBoundary)
	if res.Status != StatusPass || !strings.Contains(res.Detail, "takes effect only with proc.exec") {
		t.Errorf("golden (idle-boundary without proc.exec): %s (%s), want PASS naming the missing proc.exec", res.Status, res.Detail)
	}

	bodies = reference.Bodies()
	noSession(bodies)
	bodies["protocol"] = `echo '{"version":0,"capabilities":["idle-boundary"]}'`
	report, err = Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res := resultFor(t, report, ReqConnectionIdleBoundary); res.Status != StatusFail {
		t.Errorf("idle-boundary alone over a box with no tmux session: %s (%s), want FAIL", res.Status, res.Detail)
	}
}

// The idle-boundary probe sends each command exactly as the exec provider
// sends it on the exec op's stdin, so a pack that handles gc's own commands
// is the one that passes.
// Kills: a probe quoting that drifts from the exec provider's.
func TestIdleBoundaryCommandsMatchTheExecProvider(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "stdin")
	script := filepath.Join(dir, "pack")
	body := "#!/bin/sh\ncase \"$1\" in\n  protocol) echo '{\"version\":0,\"capabilities\":[\"proc.exec\"]}' ;;\n  exec) cat > '" + got + "' ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	argvs := [][]string{
		{"tmux", "has-session", "-t", "main"},
		{"tmux", "capture-pane", "-t", "main", "-p", "-S", "-120"},
	}
	if len(argvs) != len(idleBoundaryCommands) {
		t.Fatalf("%d idle-boundary commands, want %d", len(idleBoundaryCommands), len(argvs))
	}
	p := sessionexec.NewProvider(script)
	for i, argv := range argvs {
		if _, _, err := p.Exec(context.Background(), "s", argv); err != nil {
			t.Fatalf("Exec(%q): %v", argv, err)
		}
		sent, err := os.ReadFile(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(sent) != idleBoundaryCommands[i] {
			t.Errorf("probe command %d = %q, exec provider sends %q", i, idleBoundaryCommands[i], sent)
		}
	}
}

// Kills a ported group whose gate is wrong: without observe.v1 the
// concurrency group skips while meta, discovery and process still run on
// the golden's ops; an op that answers exit 2 skips its group, except a
// metadata op under observe.v1, whose exit 2 is a missing store, even when
// start itself is broken; and RPP-DISCOVERY-005 skips without
// conformance-faults.
func TestPortedGroupsGateOnWhatIsDeclared(t *testing.T) {
	ported := func(g Group) bool {
		return g == GroupMeta || g == GroupDiscovery || g == GroupProcess || g == GroupConcurrency
	}
	for _, tc := range []struct {
		name     string
		protocol string
		absent   []string
		want     func(req Result) Status
	}{
		{
			name:     "no observe.v1, every op present",
			protocol: `{"version":0,"capabilities":["conformance-faults"]}`,
			want: func(req Result) Status {
				if req.Group == GroupConcurrency {
					return StatusSkip
				}
				return StatusPass
			},
		},
		{
			name:     "no observe.v1, no conformance-faults",
			protocol: `{"version":0,"capabilities":[]}`,
			want: func(req Result) Status {
				if req.Group == GroupConcurrency || req.Code == ReqDiscoveryFailureIsNotAbsence {
					return StatusSkip
				}
				return StatusPass
			},
		},
		{
			name:     "no observe.v1, optional ops absent",
			protocol: `{"version":0,"capabilities":["conformance-faults"]}`,
			absent:   []string{"set-meta", "get-meta", "remove-meta", "list-running", "process-alive"},
			want:     func(Result) Status { return StatusSkip },
		},
		{
			// An absent op is told before any session starts, so a
			// broken start fails the lifecycle group, not these.
			name:     "no observe.v1, start and optional ops absent",
			protocol: `{"version":0,"capabilities":["conformance-faults"]}`,
			absent:   []string{"start", "set-meta", "get-meta", "remove-meta", "list-running", "process-alive"},
			want:     func(Result) Status { return StatusSkip },
		},
		{
			name:     "observe.v1, optional ops absent",
			protocol: `{"version":0,"capabilities":["observe.v1","conformance-faults"]}`,
			absent:   []string{"set-meta", "get-meta", "remove-meta", "list-running", "process-alive"},
			want: func(req Result) Status {
				switch req.Group {
				case GroupMeta:
					return StatusFail
				case GroupConcurrency:
					if req.Code == ReqConcurrencyStart || req.Code == ReqConcurrencyStop || req.Code == ReqConcurrencyIsRunning {
						return StatusPass
					}
				}
				return StatusSkip
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := reference.Bodies()
			bodies["protocol"] = `echo '` + tc.protocol + `'`
			for _, op := range tc.absent {
				delete(bodies, op)
			}
			report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
			if err != nil {
				t.Fatal(err)
			}
			for _, res := range report.Results {
				if !ported(res.Group) {
					continue
				}
				if want := tc.want(res); res.Status != want {
					t.Errorf("%s = %s (%s), want %s", res.Code, res.Status, res.Detail, want)
				}
			}
		})
	}
}

// Kills a token group that runs for a script not declaring its token: the
// golden answers every op the group sends, so only the gate can make the
// group skip, and an undeclared pack is never sent the token's op.
func TestEveryTokenGroupSkipsWhenUndeclared(t *testing.T) {
	for group, token := range map[Group]string{
		GroupServer: runtime.ProtocolCapabilityServerLifecycle,
	} {
		bodies := reference.Bodies()
		var caps []string
		for _, c := range []string{"observe.v1", "report-attachment", "report-activity", "conformance-faults", "city-scoped", runtime.ProtocolCapabilityServerLifecycle} {
			if c != token {
				caps = append(caps, `"`+c+`"`)
			}
		}
		bodies["protocol"] = `echo '{"version":0,"capabilities":[` + strings.Join(caps, ",") + `]}'`
		report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, res := range report.Results {
			if res.Group != group {
				continue
			}
			seen++
			if res.Status != StatusSkip {
				t.Errorf("%s undeclared: %s = %s (%s), want SKIP", token, res.Code, res.Status, res.Detail)
			}
		}
		if seen == 0 {
			t.Errorf("group %s has no requirement in the report", group)
		}
	}
}

// Kills an RPP-SERVER-004 that runs for a script not declaring city-scoped:
// a script that does not scope its boxes by city is not held to sparing
// another city's, so the requirement skips even for a server-stop that ends
// every city's sessions.
func TestServerCityScopeSkipsWithoutCityScoped(t *testing.T) {
	bodies := reference.Bodies()
	bodies["protocol"] = `echo '{"version":0,"capabilities":["server-lifecycle"]}'`
	bodies["server-stop"] = `rm -rf "$S"/city*`
	report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res := resultFor(t, report, ReqServerStopSparesOtherCities); res.Status != StatusSkip {
		t.Errorf("city-scoped undeclared: %s = %s (%s), want SKIP", res.Code, res.Status, res.Detail)
	}
}

// Kills a server group that sends server-stop in the city the run inherited:
// gc runtime conformance run from a city must leave that city's sessions
// running, though the golden's server-stop, like a city-scoped pack's, ends
// every session of the city it runs in.
func TestServerGroupSparesTheCallersCity(t *testing.T) {
	path := writeGolden(t)
	t.Setenv("GC_EXEC_CITY_NAME", "live")
	t.Setenv("GC_EXEC_CITY_PATH", "/live/city")
	const sentinel = "live-city-sentinel"
	if out, err := goldenOp(path, `{"command":"sleep 300"}`, "start", sentinel); err != nil {
		t.Fatalf("starting %s in the caller's city: %v (%s)", sentinel, err, out)
	}
	report := runGolden(t, path)
	for _, res := range report.Results {
		if res.Group == GroupServer && res.Status != StatusPass {
			t.Errorf("%s = %s (%s), want PASS: the group must run server-stop", res.Code, res.Status, res.Detail)
		}
	}
	if out, err := goldenOp(path, "", "is-running", sentinel); err != nil || out != "true" {
		t.Fatalf("%s in the caller's city: is-running = %q (%v) after the run, want true: the run's server-stop ended a session of the city it was run from", sentinel, out, err)
	}
}

// goldenOp runs one op of the script at path in the test's environment, the
// city a run started from it inherits.
func goldenOp(path, stdin string, args ...string) (string, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
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

// Kills an fshost group that runs for a script not declaring fs.host, or an
// agent_pid check that runs without observe.v1: the host-filesystem golden's
// ops are all there, so only the gates can make these skip.
func TestFSHostGroupSkipsWhenUndeclared(t *testing.T) {
	for protocol, wantSkip := range map[string]map[Code]bool{
		`{"version":0,"capabilities":["observe.v1","report-attachment"],"locality":"local"}`: nil,
		`{"version":0,"capabilities":["fs.host"],"locality":"local"}`:                        {ReqFSHostAgentPIDOnHost: true},
	} {
		bodies := reference.HostFSBodies()
		bodies["protocol"] = `echo '` + protocol + `'`
		report, err := Run(context.Background(), writeScript(t, t.TempDir(), bodies), Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range report.Results {
			if res.Group != GroupFSHost {
				continue
			}
			skip := wantSkip == nil || wantSkip[res.Code]
			if (res.Status == StatusSkip) != skip {
				t.Errorf("handshake %s: %s = %s (%s), want skip %v", protocol, res.Code, res.Status, res.Detail, skip)
			}
		}
	}
}

// The base golden, which does not declare fs.host, fails nothing and skips
// exactly the fshost group: fs.host adds a promise, it changes no other.
func TestBaseGoldenSkipsOnlyTheFSHostGroup(t *testing.T) {
	report := runGolden(t, writeScript(t, t.TempDir(), reference.Bodies()))
	for _, res := range report.Results {
		switch {
		case res.Status == StatusFail:
			t.Errorf("base golden failed %s: %s", res.Code, res.Detail)
		case (res.Status == StatusSkip) != (res.Group == GroupFSHost):
			t.Errorf("base golden: %s (%s) = %s, want skip only for the fshost group", res.Code, res.Group, res.Status)
		}
	}
}
