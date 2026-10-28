//go:build windows

package main

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestGeneratedOnDeathRunsInProcessOnWindows pins a pool instance's generated
// on_death on Windows, run as the reconciler runs it (shellRunHook over
// computePoolDeathHandlers): in gc's embedded interpreter, in the pool's
// directory and the controller's environment, starting no shell, awk, xargs or
// jq. It releases the instance's in-progress work, durable and ephemeral,
// backfills a route where a bead has none, leaves another instance's work
// alone, and reports a release bd refuses with the gc-recovery marker.
func TestGeneratedOnDeathRunsInProcessOnWindows(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
	log := installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{
		"list --assignee=worker-1 --status=in_progress --json": {Stdout: `[{"id":"ga-routed","metadata":{"gc.routed_to":"worker"}},{"id":"ga-bare","metadata":{}}]`},
		"query":          {Stdout: `[{"id":"ga-ephemeral","assignee":"worker-1","metadata":{"gc.run_target":"worker"}},{"id":"ga-other","assignee":"worker-2","metadata":{}}]`},
		"update ga-bare": {Exit: 1, Stderr: "store unavailable"},
		"update":         {},
		"":               {Stdout: "[]"},
	})
	stubs, marker := posixShellStandIns(t)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_HOOK_PROBE", "probe-env")

	var stderr bytes.Buffer
	handlers := computePoolDeathHandlers(cfg, "test", cityPath, runtime.NewFake(), &stderr)
	info, ok := handlers["worker-1"]
	if !ok || len(handlers) != 2 || stderr.Len() != 0 {
		t.Fatalf("on_death handlers %v, stderr %q; want one for each instance and nothing reported", handlerKeys(handlers), stderr.String())
	}
	out, err := shellRunHook(info.Command, info.Dir, info.Env)
	if err != nil {
		t.Fatalf("shellRunHook: %v", err)
	}
	if want := "gc-recovery: on_death release failed for ga-bare: store unavailable\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	calls := readFakeBdCalls(t, log)
	var updates [][]string
	for _, call := range calls {
		requireCallDir(t, call, cityPath)
		requireCallEnv(t, call, "GC_HOOK_PROBE", "probe-env")
		if call.Args[0] == "update" {
			updates = append(updates, call.Args)
		}
	}
	if len(calls) != 5 {
		t.Errorf("bd ran %d times, want the two reads and three releases: %+v", len(calls), calls)
	}
	want := [][]string{
		{"update", "ga-routed", "--assignee", "", "--status", "open"},
		{"update", "ga-bare", "--assignee", "", "--status", "open", "--set-metadata", "gc.run_target=worker"},
		{"update", "ga-ephemeral", "--assignee", "", "--status", "open"},
	}
	if !slices.EqualFunc(updates, want, slices.Equal[[]string]) {
		t.Errorf("releases = %q, want %q", updates, want)
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the on_death hook started the %s stand-in", strings.TrimSpace(string(data)))
	}
}

// TestGeneratedOnBootRunsInProcessOnWindows pins a pool's generated on_boot on
// Windows, run as the controller runs it at startup (runPoolOnBoot with
// shellRunHook): in gc's embedded interpreter, in the pool's directory,
// starting no shell, awk, xargs or jq. It reopens the pool's ownerless
// in-progress work from all three of its reads, once each and in the order
// read, skips work routed elsewhere or owned, and reports a reopen bd refuses
// with the gc-recovery marker.
func TestGeneratedOnBootRunsInProcessOnWindows(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
	log := installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{
		"list --metadata-field gc.routed_to=worker":  {Stdout: `[{"id":"ga-shared"},{"id":"ga-routed"}]`},
		"list --metadata-field gc.run_target=worker": {Stdout: `[{"id":"ga-shared","metadata":{}},{"id":"ga-legacy","metadata":{}},{"id":"ga-moved","metadata":{"gc.routed_to":"elsewhere"}}]`},
		"query":            {Stdout: `[{"id":"ga-ephemeral","metadata":{"gc.routed_to":"worker"}},{"id":"ga-owned","assignee":"worker-2","metadata":{"gc.routed_to":"worker"}}]`},
		"update ga-routed": {Exit: 1, Stderr: "store unavailable"},
		"update":           {},
		"":                 {Stdout: "[]"},
	})
	stubs, marker := posixShellStandIns(t)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, cityPath, shellRunHook, &stderr)
	if want := "on_boot worker: gc-recovery: on_boot reopen failed for ga-routed: store unavailable\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}

	var reads, reopens [][]string
	for _, call := range readFakeBdCalls(t, log) {
		requireCallDir(t, call, cityPath)
		if call.Args[0] == "update" {
			reopens = append(reopens, call.Args)
		} else {
			reads = append(reads, call.Args)
		}
	}
	wantReads := [][]string{
		{"list", "--metadata-field", "gc.routed_to=worker", "--status=in_progress", "--no-assignee", "--json"},
		{"list", "--metadata-field", "gc.run_target=worker", "--metadata-field", "gc.kind=workflow", "--status=in_progress", "--no-assignee", "--json"},
		{"query", "--json", "ephemeral=true AND status=in_progress", "--limit=0"},
	}
	if !slices.EqualFunc(reads, wantReads, slices.Equal[[]string]) {
		t.Errorf("reads = %q, want %q", reads, wantReads)
	}
	var wantReopens [][]string
	for _, id := range []string{"ga-shared", "ga-routed", "ga-legacy", "ga-ephemeral"} {
		wantReopens = append(wantReopens, []string{"update", id, "--status", "open"})
	}
	if !slices.EqualFunc(reopens, wantReopens, slices.Equal[[]string]) {
		t.Errorf("reopens = %q, want %q", reopens, wantReopens)
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the on_boot hook started the %s stand-in", strings.TrimSpace(string(data)))
	}
}
