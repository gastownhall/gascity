//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/genshell"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestGeneratedWorkQueryRunsInProcessOnWindows pins the generated default work
// query on Windows: it runs in gc's embedded interpreter, exactly as generated,
// and asks bd what the program asks, with every argument intact, in the
// caller's directory and environment. Stand-ins for sh and bash sit first on
// PATH and record any start; none may happen. bd's row comes back with its
// title intact, nothing HTML-escaped.
func TestGeneratedWorkQueryRunsInProcessOnWindows(t *testing.T) {
	const title = "routed <&> wörk ✓"
	query := (&config.Agent{Name: "worker"}).EffectiveWorkQuery()
	target := generatedTarget(t, query)
	log := installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{
		"ready --metadata-field gc.routed_to=" + target: {Stdout: `[{"id":"ga-routed","title":"` + title + `","metadata":{"gc.routed_to":"` + target + `"}}]`},
		"": {Stdout: "[]"},
	})
	stubs, marker := posixShellStandIns(t)
	dir := t.TempDir()
	env := sessionlessQueryEnv("PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"), "GC_WORK_QUERY_PROBE=probe-env")

	out, err := shellWorkQueryWithEnv(query, dir, env)
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv: %v", err)
	}
	want := `[{"id":"ga-routed","metadata":{"gc.routed_to":"` + target + `"},"title":"` + title + `"}]`
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}

	calls := readFakeBdCalls(t, log)
	if len(calls) != 1 {
		t.Fatalf("bd ran %d times (%+v), want once: only the routed tier has anything to ask", len(calls), calls)
	}
	args := calls[0].Args
	if !slices.Equal(args[:3], []string{"ready", "--metadata-field", "gc.routed_to=" + target}) {
		t.Fatalf("bd ran %q, want the routed ready tier", args)
	}
	for _, arg := range []string{"--unassigned", "--exclude-type=epic", "hold:mayor", "hold:external", "--json"} {
		if !slices.Contains(args, arg) {
			t.Errorf("bd ran %q, want it among the arguments: %q", args, arg)
		}
	}
	requireCallDir(t, calls[0], dir)
	requireCallEnv(t, calls[0], "GC_WORK_QUERY_PROBE", "probe-env")
	metricsName, metricsValue, _ := strings.Cut(execenv.UsageMetricsDisabledEntry, "=")
	requireCallEnv(t, calls[0], metricsName, metricsValue)
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the generated work query started the %s stand-in", strings.TrimSpace(string(data)))
	}
}

// TestGeneratedPoolDemandCheckRunsInProcessOnWindows pins the generated
// default scale_check on Windows: its three reads run in the embedded
// interpreter, in the check's environment, and its jq count deduplicates them;
// a failing read fails the check with bd's status.
func TestGeneratedPoolDemandCheckRunsInProcessOnWindows(t *testing.T) {
	check := (&config.Agent{Name: "worker"}).EffectivePoolDemandQuery()
	target := generatedTarget(t, check)
	routed := "ready --metadata-field gc.routed_to=" + target
	log := installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{
		routed: {Stdout: `[{"id":"a"},{"id":"b"}]`},
		"ready --metadata-field gc.run_target=" + target: {Stdout: `[{"id":"b"},{"id":"c"},{"id":"d","metadata":{"gc.routed_to":"elsewhere"}}]`},
		"query": {Stdout: `[{"id":"e","assignee":"","metadata":{"gc.routed_to":"` + target + `"}}]`},
		"":      {Stdout: "[]"},
	})

	out, err := shellScaleCheck(check, t.TempDir(), map[string]string{"GC_SCALE_CHECK_PROBE": "probe-env"})
	if err != nil {
		t.Fatalf("shellScaleCheck: %v", err)
	}
	if strings.TrimSpace(out) != "4" {
		t.Fatalf("demand = %q, want 4: a and b routed, c legacy, e ephemeral; b counted once, d routed elsewhere", out)
	}
	calls := readFakeBdCalls(t, log)
	if len(calls) != 3 {
		t.Fatalf("bd ran %d times (%+v), want the check's three reads", len(calls), calls)
	}
	for _, call := range calls {
		requireCallEnv(t, call, "GC_SCALE_CHECK_PROBE", "probe-env")
	}

	installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{
		routed: {Exit: 3, Stderr: "store unavailable"},
		"":     {Stdout: "[]"},
	})
	if out, err := shellScaleCheck(check, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("shellScaleCheck over a failing bd = %q, %v; want bd's exit status 3", out, err)
	}
}

// TestGeneratedWorkQueryTimeoutReapsBdsTreeOnWindows pins what killing the
// query's process group does on Unix: a generated query that times out takes
// every process its bd started with it. The bd here is a stand-in that starts
// a long-running descendant and waits. The timeout leaves Windows PowerShell,
// slow to start under load, time to record the descendant first.
func TestGeneratedWorkQueryTimeoutReapsBdsTreeOnWindows(t *testing.T) {
	previous := hookWorkQueryTimeout
	hookWorkQueryTimeout = 20 * time.Second
	t.Cleanup(func() { hookWorkQueryTimeout = previous })

	record := filepath.Join(t.TempDir(), "descendant")
	stubs := bdStandIn(t, processgrouptest.DescendantCommand(record))
	env := sessionlessQueryEnv("PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"))

	started := time.Now()
	_, err := shellWorkQueryWithEnv((&config.Agent{Name: "worker"}).EffectiveWorkQuery(), "", env)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shellWorkQueryWithEnv: %v; want the query's timeout", err)
	}
	if elapsed := time.Since(started); elapsed > hookWorkQueryTimeout+10*time.Second {
		t.Errorf("the query returned %s after starting, want soon after its %s timeout", elapsed, hookWorkQueryTimeout)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if !d.WaitGone(10 * time.Second) {
		t.Fatal("bd's descendant survived the query's timeout")
	}
}

// TestGeneratedProgramTimeoutSparesEarlierDetachedBdDescendantOnWindows covers
// the whole generated program, not just the currently running bd: an earlier bd
// can exit after deliberately detaching a descendant, and a later bd can be the
// one that times out.
func TestGeneratedProgramTimeoutSparesEarlierDetachedBdDescendantOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	script := "$record = '" + strings.ReplaceAll(record, "'", "''") + "'; " +
		"if (-not (Test-Path -LiteralPath $record)) { " +
		processgrouptest.LeaveDetachedDescendantCommand(record) + "; [Console]::Out.Write('[]'); exit 0 }; " +
		"Set-Content -LiteralPath ($record + '.later') -Value 'second bd is running'; " +
		"Start-Sleep -Seconds 300"
	stubs := bdStandIn(t, script)
	env := map[string]string{
		"PATH":              stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PATHEXT":           ".CMD;.EXE",
		"GC_SESSION_ID":     "",
		"GC_SESSION_NAME":   "",
		"GC_ALIAS":          "",
		"GC_SESSION_ORIGIN": "",
	}

	_, err := runShellCommand((&config.Agent{Name: "worker"}).EffectivePoolDemandQuery(), t.TempDir(), 6*time.Second, env, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runShellCommand: %v; want the generated program's timeout", err)
	}
	if _, err := os.Stat(record + ".later"); err != nil {
		t.Fatalf("the later bd did not start; the test did not reach the intended schedule: %v", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if d.WaitGone(2 * time.Second) {
		t.Fatal("the generated program timeout killed the completed first bd's detached descendant")
	}
	d.Terminate()
	if !d.WaitGone(5 * time.Second) {
		t.Fatal("the completed first bd's detached descendant did not exit during test cleanup")
	}
}

// TestFinishedGeneratedWorkQueryLeavesBdsDescendantRunningOnWindows pins the
// other half: a process group outlives its leader, so a bd that finishes
// leaves what it started running, and the query goes on.
func TestFinishedGeneratedWorkQueryLeavesBdsDescendantRunningOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	// Each bd the query runs is this stand-in; only the first starts a
	// descendant, so the one recorded is the only one there is.
	stubs := bdStandIn(t, "if (-not (Test-Path -LiteralPath '"+strings.ReplaceAll(record, "'", "''")+"')) { "+
		processgrouptest.LeaveDetachedDescendantCommand(record)+" }")
	env := sessionlessQueryEnv("PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"))

	out, err := shellWorkQueryWithEnv((&config.Agent{Name: "worker"}).EffectiveWorkQuery(), "", env)
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv: %v", err)
	}
	if out != "[]" {
		t.Fatalf("output = %q, want %q: bd answered nothing", out, "[]")
	}
	d := processgrouptest.ReadDescendant(t, record)
	if d.WaitGone(2 * time.Second) {
		t.Fatal("bd's descendant died when bd finished; releasing bd's tree must not kill it")
	}
}

// TestGeneratedProgramRecognizesOnlyGcsOwnPrograms pins which commands run in
// the embedded interpreter: each read program this process generated, with the
// exact generated arguments and assignments, and each recovery hook it
// generated, exactly as generated; and nothing else. A custom query, an edited
// hook, the hook of an agent gc generated none for, and a program merely
// resembling a generated one keep the platform's interpreter.
func TestGeneratedProgramRecognizesOnlyGcsOwnPrograms(t *testing.T) {
	a := &config.Agent{Name: "worker"}
	query := a.EffectiveWorkQuery()
	prog, err := genshell.Parse(query)
	if err != nil {
		t.Fatalf("the generated work query does not parse: %v", err)
	}
	onBoot := a.EffectiveOnBoot()
	onDeath := a.EffectiveOnDeath()
	bd105 := config.QueryTopology{Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}}
	instance := &config.Agent{Name: "worker-1", PoolName: "worker"}

	for name, command := range map[string]string{
		"work query":                    query,
		"assigned in-progress query":    a.EffectiveAssignedInProgressQuery(),
		"assigned ready query":          a.EffectiveAssignedReadyQuery(),
		"routed pool query":             a.EffectiveRoutedPoolQuery(),
		"pool demand check":             a.EffectivePoolDemandQuery(),
		"federated work query":          a.EffectiveWorkQueryFor(config.QueryTopology{FederatedReady: true}),
		"control dispatcher work query": (&config.Agent{Name: config.ControlDispatcherAgentName}).EffectiveWorkQuery(),
		"on_boot":                       onBoot,
		"on_death":                      onDeath,
		"bd 1.0.5 on_boot":              a.EffectiveOnBootFor(bd105),
		"pool instance on_death":        instance.EffectiveOnDeath(),
	} {
		if generatedProgram(command) == nil {
			t.Errorf("%s: not run in the embedded interpreter", name)
		}
	}

	for name, command := range map[string]string{
		"custom work_query":          (&config.Agent{Name: "worker", WorkQuery: `sh -c 'bd ready --json'`}).EffectiveWorkQuery(),
		"custom on_boot":             (&config.Agent{Name: "worker", OnBoot: `bd list --json`}).EffectiveOnBoot(),
		"altered script":             shellquote.Join([]string{"sh", "-c", prog.Script + " ", "--", "worker"}),
		"another target":             shellquote.Join([]string{"sh", "-c", prog.Script, "--", "rig2/other"}),
		"prefix assignments":         "GC_DOLT_PORT=" + shellquote.Quote("3307") + " " + query,
		"bash":                       shellquote.Join([]string{"bash", "-c", prog.Script, "--", "worker"}),
		"in a pipeline":              query + " | cat",
		"followed by a command":      query + "; rm -rf city",
		"expanded target":            shellquote.Join([]string{"sh", "-c", prog.Script, "--"}) + " $TARGET",
		"PowerShell":                 `Write-Output '[]'`,
		"generated script as one":    prog.Script,
		"edited on_boot":             strings.Replace(onBoot, "awk 'NF && !seen[$0]++'", "awk '!seen[$0]++'", 1),
		"on_death and a command":     onDeath + "; rm -rf city",
		"on_boot with a blank":       onBoot + " ",
		"on_boot under sh -c":        shellquote.Join([]string{"sh", "-c", onBoot}),
		"on_death of an unseen pool": strings.ReplaceAll(onDeath, "worker", "never-generated-pool"),
	} {
		if generatedProgram(command) != nil {
			t.Errorf("%s: run in the embedded interpreter, want the platform's", name)
		}
	}
}

// generatedTarget returns the first target a generated program is run for.
func generatedTarget(t *testing.T, command string) string {
	t.Helper()
	prog, err := genshell.Parse(command)
	if err != nil || len(prog.Args) == 0 {
		t.Fatalf("generated program %q has no target (%v)", command, err)
	}
	return prog.Args[0]
}

// sessionlessQueryEnv is this process's environment with the session identity
// the generated work query reads left empty, as the reconciler runs it, and
// extra after it.
func sessionlessQueryEnv(extra ...string) []string {
	env := append(os.Environ(), "GC_SESSION_ID=", "GC_SESSION_NAME=", "GC_ALIAS=", "GC_SESSION_ORIGIN=")
	return append(env, extra...)
}

// posixShellStandIns writes stand-ins for sh and bash, and for the awk, xargs
// and jq a generated program runs, that record their start in marker, and
// returns their directory and the marker.
func posixShellStandIns(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	for _, name := range []string{"sh.cmd", "bash.cmd", "awk.cmd", "xargs.cmd", "jq.cmd"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("@echo %~n0> \"%~dp0started\"\r\n"), 0o644); err != nil {
			t.Fatalf("write stand-in %s: %v", name, err)
		}
	}
	return dir, filepath.Join(dir, "started")
}

// bdStandIn writes a bd.cmd that runs command as a Windows PowerShell script,
// and returns its directory.
func bdStandIn(t *testing.T, command string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "bd-stand-in.ps1")
	if err := os.WriteFile(script, []byte(command+"\r\n"), 0o644); err != nil {
		t.Fatalf("write bd stand-in script: %v", err)
	}
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	body := "@\"" + powershell + "\" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File \"" + script + "\"\r\n"
	if err := os.WriteFile(filepath.Join(dir, "bd.cmd"), []byte(body), 0o644); err != nil {
		t.Fatalf("write bd stand-in: %v", err)
	}
	return dir
}

// readFakeBdCalls returns the runs the native fake bd recorded at path, in
// order.
func readFakeBdCalls(t *testing.T, path string) []nativeFakeBdCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []nativeFakeBdCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var call nativeFakeBdCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("fake bd record %q: %v", line, err)
		}
		calls = append(calls, call)
	}
	return calls
}
