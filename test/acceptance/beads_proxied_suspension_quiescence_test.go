//go:build acceptance_a

// Suspension is quiescence: a suspended rig, and every scope of a suspended
// city, gets no bd call from gc at all — no cache scan, no order sweep, no
// health ping, no maintenance order — and its bd-owned proxy and Dolt child are
// stopped on suspend, the same `bd dolt stop` gc stop runs last. Measured
// through a BD_BIN shim that records every fork with the scope it targeted,
// over a window long enough for every periodic backstop to come round.
package acceptance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// quiescenceWindow is long enough for every periodic backstop that touches a
// scope on a running city (the 30s beads-health, the 1m order-tracking sweep,
// the controller's reconcile and demand passes) to come round several times.
const quiescenceWindow = 3 * time.Minute

// suspendDrainWait bounds how long a suspend takes to reach quiescence: the
// controller stops the suspended scope's sessions over its next ticks, then
// stops the scope's bd pair.
const suspendDrainWait = 3 * time.Minute

// scopeRecordingBD is a BD_BIN shim that records, per fork, the scope it
// targets (BEADS_DIR and the working directory) and its argv, then execs the
// real bd.
type scopeRecordingBD struct {
	path    string
	logPath string
}

func newScopeRecordingBD(t *testing.T, realBD string) *scopeRecordingBD {
	t.Helper()
	dir := filepath.Join(helpers.TempDir(t), "scope-recording-bd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &scopeRecordingBD{path: filepath.Join(dir, "bd"), logPath: filepath.Join(dir, "calls.log")}
	script := fmt.Sprintf(`#!/bin/sh
record="beads_dir=${BEADS_DIR:-} pwd=$(pwd) argv=$*"
printf '%%s\n' "$record" >>%s 2>/dev/null || true
exec %s "$@"
`, shellQuoteArg(r.logPath), shellQuoteArg(realBD))
	if err := os.WriteFile(r.path, []byte(script), 0o755); err != nil { //nolint:gosec // the shim must be executable
		t.Fatal(err)
	}
	return r
}

func shellQuoteArg(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// reset forgets every call recorded so far.
func (r *scopeRecordingBD) reset(t *testing.T) {
	t.Helper()
	if err := os.Remove(r.logPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// callsUnder returns the recorded calls that targeted a scope under root.
func (r *scopeRecordingBD) callsUnder(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(r.logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && strings.Contains(line, root) {
			hits = append(hits, line)
		}
	}
	return hits
}

func TestProxiedSuspensionIsQuiescence(t *testing.T) {
	// Two multi-minute quiescence windows over a running city: not Tier A
	// smoke material. The proxied-native acceptance job runs it.
	helpers.RequireTopologyMatrix(t)
	bdPath, doltPath := requireProxiedTooling(t)
	env, wrappedBD := proxiedEnvWithBD(t, bdPath, doltPath)
	shim := newScopeRecordingBD(t, wrappedBD)
	env = env.With("BD_BIN", shim.path)

	city := helpers.NewCity(t, env)
	cityRoot := city.Dir
	rigDir := createGitRig(t)
	t.Cleanup(func() {
		helpers.RunGC(env, cityRoot, "stop", cityRoot)         //nolint:errcheck // best effort
		helpers.RunGC(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best effort
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, 20*time.Second); len(leaked) > 0 {
				t.Errorf("processes under %s outlived the test:\n%s", root, strings.Join(leaked, "\n"))
			}
		}
	})
	city.Init("claude")
	city.RigAdd(rigDir, "")
	const rigName = "testrig"
	if len(doltProcessesUnder(t, rigDir)) == 0 {
		if out, err := city.GC("bd", "--rig", rigName, "list", "--json"); err != nil {
			t.Fatalf("gc bd --rig %s list: %v\n%s", rigName, err, out)
		}
	}

	t.Run("suspended-rig", func(t *testing.T) {
		if out, err := city.GC("rig", "suspend", rigName); err != nil {
			t.Fatalf("gc rig suspend: %v\n%s", err, out)
		}
		if leaked := waitForNoDoltProcesses(t, rigDir, suspendDrainWait); len(leaked) > 0 {
			t.Fatalf("the suspended rig's pair is still running:\n%s", strings.Join(leaked, "\n"))
		}
		shim.reset(t)
		time.Sleep(quiescenceWindow)
		if calls := shim.callsUnder(t, rigDir); len(calls) > 0 {
			t.Fatalf("gc touched the suspended rig %d time(s) in %s:\n%s", len(calls), quiescenceWindow, strings.Join(calls, "\n"))
		}
		if started := doltProcessesUnder(t, rigDir); len(started) > 0 {
			t.Fatalf("the suspended rig's pair came back:\n%s", strings.Join(started, "\n"))
		}
		if out, err := city.GC("rig", "resume", rigName); err != nil {
			t.Fatalf("gc rig resume: %v\n%s", err, out)
		}
		if out, err := city.GC("bd", "--rig", rigName, "list", "--json"); err != nil {
			t.Fatalf("gc bd --rig %s list after resume: %v\n%s", rigName, err, out)
		}
	})

	t.Run("suspended-city", func(t *testing.T) {
		if out, err := city.GC("suspend"); err != nil {
			t.Fatalf("gc suspend: %v\n%s", err, out)
		}
		// The controller stops the city's sessions first and only then the
		// pairs, so this waits for the whole drain.
		for _, root := range []string{cityRoot, rigDir} {
			if leaked := waitForNoDoltProcesses(t, root, suspendDrainWait); len(leaked) > 0 {
				sessions, _ := city.GC("session", "list")
				t.Fatalf("a suspended city's pair under %s is still running:\n%s\nsessions:\n%s", root, strings.Join(leaked, "\n"), sessions)
			}
		}
		shim.reset(t)
		time.Sleep(quiescenceWindow)
		for _, root := range []string{cityRoot, rigDir} {
			if calls := shim.callsUnder(t, root); len(calls) > 0 {
				t.Errorf("gc touched %s %d time(s) in %s while the city was suspended:\n%s", root, len(calls), quiescenceWindow, strings.Join(calls, "\n"))
			}
			if started := doltProcessesUnder(t, root); len(started) > 0 {
				t.Errorf("a pair under %s came back while the city was suspended:\n%s", root, strings.Join(started, "\n"))
			}
		}
		if out, err := city.GC("resume"); err != nil {
			t.Fatalf("gc resume: %v\n%s", err, out)
		}
		if out, err := city.GC("bd", "list", "--json"); err != nil {
			t.Fatalf("gc bd list after resume: %v\n%s", err, out)
		}
	})
}
