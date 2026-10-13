package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gcBinGuardScriptPath returns the bd pack provider script under test.
func gcBinGuardScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
}

// TestRequireExecutableGCBin exercises the require_executable_gc_bin guard in
// gc-beads-bd.sh. The regression it guards: a long-running supervisor launched
// from a gc binary that was later removed (e.g. `brew unlink`) keeps exporting
// its original path as GC_BIN. The script used to hand that path to every gc
// helper call and died deep inside an operation with a bare "No such file or
// directory", after having already fallen back to legacy layout derivation.
func TestRequireExecutableGCBin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell-function test")
	}
	scriptBytes, err := os.ReadFile(gcBinGuardScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "require_executable_gc_bin")

	dir := t.TempDir()
	executable := filepath.Join(dir, "gc")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExecutable := filepath.Join(dir, "gc-not-exec")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "removed", "gc")

	cases := []struct {
		name  string
		op    string
		gcBin string
		// wantGCBin is GC_BIN after the guard returns; the guard refuses
		// the operation when it is nil.
		wantGCBin *string
	}{
		{name: "unset", op: "start", gcBin: "", wantGCBin: ptr("")},
		{name: "executable", op: "start", gcBin: executable, wantGCBin: ptr(executable)},
		{name: "missing", op: "start", gcBin: missing},
		{name: "not_executable", op: "start", gcBin: notExecutable},
		{name: "directory", op: "start", gcBin: dir},
		{name: "missing_health", op: "health", gcBin: missing},
		{name: "executable_stop", op: "stop", gcBin: executable, wantGCBin: ptr(executable)},
		{name: "missing_stop", op: "stop", gcBin: missing, wantGCBin: ptr("")},
		{name: "not_executable_shutdown", op: "shutdown", gcBin: notExecutable, wantGCBin: ptr("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"require_executable_gc_bin \"$1\"\n" +
				"echo \"reached GC_BIN=[$GC_BIN]\"\n"
			env := append(os.Environ(), "GC_BIN="+tc.gcBin)
			stdout, stderr, runErr := runGCBeadsBdCommand(t, env, "sh", "-c", script, "guard", tc.op)
			if tc.wantGCBin == nil {
				if runErr == nil || strings.Contains(stdout, "reached") {
					t.Fatalf("guard accepted unusable GC_BIN=%q for %s: stdout=%q stderr=%q", tc.gcBin, tc.op, stdout, stderr)
				}
				assertGCBinGuardMessage(t, stderr, tc.gcBin)
				return
			}
			if want := "reached GC_BIN=[" + *tc.wantGCBin + "]"; runErr != nil || !strings.Contains(stdout, want) {
				t.Fatalf("guard with GC_BIN=%q for %s: err=%v stdout=%q stderr=%q, want %q", tc.gcBin, tc.op, runErr, stdout, stderr, want)
			}
			if tc.gcBin == *tc.wantGCBin {
				if stderr != "" {
					t.Fatalf("guard warned about usable GC_BIN=%q: stderr=%q", tc.gcBin, stderr)
				}
				return
			}
			// Teardown continues without the stale gc, but still says why.
			assertGCBinGuardMessage(t, stderr, tc.gcBin)
		})
	}
}

// TestGCBeadsBDFailsClosedOnStaleGCBin runs the whole provider script with a
// GC_BIN that no longer exists and asserts it stops before touching the city:
// no silent fallback to legacy layout derivation, no state directories, and no
// PATH-resolved gc of an unknown version standing in for the missing one. A
// provider-owned scope and a proxied scope's bridge operations refuse the same
// way.
func TestGCBeadsBDFailsClosedOnStaleGCBin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell script test")
	}
	for _, tc := range []struct {
		name     string
		op       string
		metadata string
		extraEnv []string
	}{
		{name: "start", op: "start"},
		{name: "init", op: "init"},
		{name: "health", op: "health"},
		{name: "recover", op: "recover"},
		{name: "provider_owned_start", op: "start", extraEnv: []string{"GC_BEADS_PROVIDER_OWNED=1"}},
		{name: "proxied_list", op: "list", metadata: gcBinGuardProxiedMetadata},
		{name: "proxied_init", op: "init", metadata: gcBinGuardProxiedMetadata},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runGCBeadsBDWithStaleGCBin(t, tc.op, tc.metadata, tc.extraEnv...)
			if r.exitCode != 1 {
				t.Fatalf("gc-beads-bd.sh %s with stale GC_BIN: exit %d, want 1; stderr=%q", tc.op, r.exitCode, r.stderr)
			}
			assertGCBinGuardMessage(t, r.stderr, r.staleGCBin)
			r.assertCityUntouched(t)
			r.assertPathGCUnused(t)
		})
	}
}

// TestGCBeadsBDStopsOnStaleGCBin covers teardown under a stale GC_BIN: a
// supervisor that outlived its gc binary still runs stop when the city stops.
// Refusing would leave the Dolt server it started running, so stop and
// shutdown warn and finish on the shell-native fallbacks an unset GC_BIN
// selects. In this empty city nothing is running, so the script records a
// stopped server and exits 2, the provider's "nothing to do" status. A
// provider-owned stop proceeds the same way, and a proxied scope's lifecycle
// operations are no-ops that exit 2 without the gc, so a stale one is not
// refused there either.
func TestGCBeadsBDStopsOnStaleGCBin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell script test")
	}
	for _, op := range []string{"stop", "shutdown"} {
		t.Run(op, func(t *testing.T) {
			r := runGCBeadsBDWithStaleGCBin(t, op, "")
			if r.exitCode != 2 {
				t.Fatalf("gc-beads-bd.sh %s with stale GC_BIN: exit %d, want 2; stderr=%q", op, r.exitCode, r.stderr)
			}
			assertGCBinGuardMessage(t, r.stderr, r.staleGCBin)
			if strings.Count(r.stderr, "GC_BIN="+r.staleGCBin) != 1 {
				t.Fatalf("stderr = %q, want the stale GC_BIN warning exactly once", r.stderr)
			}
			statePath := filepath.Join(r.cityDir, ".gc", "runtime", "packs", "dolt", "dolt-provider-state.json")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("gc-beads-bd.sh %s did not reach the shell-native stop: %v; stderr=%q", op, err, r.stderr)
			}
			if !strings.Contains(string(state), `"running":false`) {
				t.Fatalf("provider state = %q, want the shell-native stop's running:false record", state)
			}
			r.assertPathGCUnused(t)
		})
	}
	t.Run("provider_owned_stop", func(t *testing.T) {
		beadsDir := filepath.Join(t.TempDir(), "rig", ".beads")
		r := runGCBeadsBDWithStaleGCBin(t, "stop", "", "GC_BEADS_PROVIDER_OWNED=1", "BEADS_DIR="+beadsDir)
		if r.exitCode != 0 || !strings.Contains(r.stderr, "nothing to retire") {
			t.Fatalf("provider-owned gc-beads-bd.sh stop with stale GC_BIN: exit %d stderr=%q, want the provider-owned stop's exit 0", r.exitCode, r.stderr)
		}
		assertGCBinGuardMessage(t, r.stderr, r.staleGCBin)
		r.assertCityUntouched(t)
		r.assertPathGCUnused(t)
	})
	for _, op := range []string{"start", "health", "stop"} {
		t.Run("proxied_"+op, func(t *testing.T) {
			r := runGCBeadsBDWithStaleGCBin(t, op, gcBinGuardProxiedMetadata)
			if r.exitCode != 2 || r.stderr != "" {
				t.Fatalf("gc-beads-bd.sh %s in a proxied scope with stale GC_BIN: exit %d stderr=%q, want a silent exit 2", op, r.exitCode, r.stderr)
			}
			r.assertCityUntouched(t)
			r.assertPathGCUnused(t)
		})
	}
}

// gcBinGuardProxiedMetadata is .beads/metadata.json for a scope that reaches
// its Dolt server through the proxy.
const gcBinGuardProxiedMetadata = `{"backend":"dolt","dolt_mode":"proxied-server"}`

// staleGCBinRun is one gc-beads-bd.sh run under a GC_BIN that does not exist.
type staleGCBinRun struct {
	op         string
	cityDir    string
	metadata   string
	staleGCBin string
	pathGCLog  string
	exitCode   int
	stderr     string
}

// runGCBeadsBDWithStaleGCBin runs gc-beads-bd.sh op in a fresh city whose
// .beads/metadata.json is metadata (none when empty), with GC_BIN naming a
// removed gc and a gc on PATH that logs any invocation.
func runGCBeadsBDWithStaleGCBin(t *testing.T, op, metadata string, extraEnv ...string) staleGCBinRun {
	t.Helper()
	r := staleGCBinRun{
		op:         op,
		cityDir:    t.TempDir(),
		metadata:   metadata,
		staleGCBin: filepath.Join(t.TempDir(), "opt", "homebrew", "bin", "gc"),
	}
	if metadata != "" {
		if err := os.MkdirAll(filepath.Join(r.cityDir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.cityDir, ".beads", "metadata.json"), []byte(metadata), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pathDir := t.TempDir()
	r.pathGCLog = filepath.Join(pathDir, "gc-invoked.log")
	fakeGC := "#!/bin/sh\necho \"$@\" >>" + r.pathGCLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(pathDir, "gc"), []byte(fakeGC), 0o755); err != nil {
		t.Fatal(err)
	}
	env := sanitizedBaseEnv(append([]string{
		"GC_BIN=" + r.staleGCBin,
		"GC_CITY_PATH=" + r.cityDir,
		"PATH=" + pathDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}, extraEnv...)...)
	_, stderr, runErr := runGCBeadsBdCommand(t, env, "sh", gcBinGuardScriptPath(t), op)
	r.stderr = stderr
	if runErr != nil {
		exitErr := &exec.ExitError{}
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("gc-beads-bd.sh %s: %v", op, runErr)
		}
		r.exitCode = exitErr.ExitCode()
	}
	return r
}

// assertCityUntouched fails if the run created state in the city: no .gc,
// and no .beads beyond the metadata the run started with.
func (r staleGCBinRun) assertCityUntouched(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(r.cityDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("gc-beads-bd.sh %s created .gc (stat err=%v)", r.op, err)
	}
	entries, err := os.ReadDir(filepath.Join(r.cityDir, ".beads"))
	if r.metadata == "" {
		if !os.IsNotExist(err) {
			t.Fatalf("gc-beads-bd.sh %s created .beads (read err=%v)", r.op, err)
		}
		return
	}
	if err != nil || len(entries) != 1 || entries[0].Name() != "metadata.json" {
		t.Fatalf("gc-beads-bd.sh %s changed .beads: entries=%v err=%v, want only metadata.json", r.op, entries, err)
	}
}

// assertPathGCUnused fails if the script ran the gc on PATH, a build of
// unknown version, in place of the missing GC_BIN.
func (r staleGCBinRun) assertPathGCUnused(t *testing.T) {
	t.Helper()
	if logged, err := os.ReadFile(r.pathGCLog); !os.IsNotExist(err) {
		t.Fatalf("gc-beads-bd.sh %s fell back to gc on PATH: %q (read err=%v)", r.op, logged, err)
	}
}

// assertGCBinGuardMessage checks the guard's die text. The restart command
// depends on the deployment (a GC_SUPERVISOR_SYSTEMD_UNIT-delegated supervisor
// refuses 'gc supervisor install'), so the text defers to the
// supervisor-binary doctor check instead of naming one.
func assertGCBinGuardMessage(t *testing.T, stderr, gcBin string) {
	t.Helper()
	for _, want := range []string{
		"GC_BIN=" + gcBin,
		"removed or replaced",
		"Restart the supervisor",
		"gc doctor",
		"supervisor-binary",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "gc supervisor install") {
		t.Fatalf("stderr = %q names 'gc supervisor install', which a delegated supervisor refuses", stderr)
	}
}
