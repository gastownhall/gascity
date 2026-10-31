//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestShellWorkQueryRunsThroughPowerShellOnWindows pins native shell site 8 on
// Windows: a work query runs through PowerShell, in the directory and with the
// environment its caller hands it.
func TestShellWorkQueryRunsThroughPowerShellOnWindows(t *testing.T) {
	dir := t.TempDir()
	env := append(os.Environ(), "GC_WORK_QUERY_PROBE=probe-env")
	out, err := shellWorkQueryWithEnv(`"$($PSVersionTable.PSEdition)|$env:GC_WORK_QUERY_PROBE|$((Get-Location).Path)"`, dir, env)
	if err != nil {
		t.Fatalf("shellWorkQueryWithEnv: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 3 {
		t.Fatalf("output = %q, want edition|env|dir", out)
	}
	if fields[0] != "Core" && fields[0] != "Desktop" {
		t.Errorf("PSEdition = %q, want the query run by PowerShell", fields[0])
	}
	if fields[1] != "probe-env" {
		t.Errorf("GC_WORK_QUERY_PROBE = %q, want the caller's environment", fields[1])
	}
	if !strings.EqualFold(filepath.Clean(fields[2]), filepath.Clean(dir)) {
		t.Errorf("working directory = %q, want %q", fields[2], dir)
	}
}

// TestShellWorkQueryRefusesPosixShellsOnWindows pins the gate on site 8: a
// work query whose program is a POSIX shell starts nothing. The read programs
// gc generates itself are the exception, run in its embedded interpreter
// (TestGeneratedWorkQueryRunsInProcessOnWindows); a program that only resembles
// one is still refused. A stand-in for each shell sits first on PATH and
// records that it ran, and the control shows that PowerShell would start it.
func TestShellWorkQueryRefusesPosixShellsOnWindows(t *testing.T) {
	stubs := t.TempDir()
	marker := filepath.Join(stubs, "started")
	for _, name := range []string{"sh.cmd", "bash.cmd"} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("@echo %~n0> \"%~dp0started\"\r\n"), 0o644); err != nil {
			t.Fatalf("write stand-in %s: %v", name, err)
		}
	}
	env := append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	generated := (&config.Agent{Name: "worker"}).EffectiveWorkQuery()
	queries := map[string]string{
		"sh":                            `sh -c 'printf "[]"'`,
		"bash":                          `bash -c 'printf "[]"'`,
		"the generated default, edited": strings.Replace(generated, `printf "[]"`, `printf "[ ]"`, 1),
	}
	if queries["the generated default, edited"] == generated {
		t.Fatal("the edit did not change the generated default")
	}
	for name, query := range queries {
		out, err := shellWorkQueryWithEnv(query, stubs, env)
		if err == nil || !strings.Contains(err.Error(), "work query refused") || !strings.Contains(err.Error(), "POSIX shell") {
			t.Errorf("%s: output %q, error %v; want the query refused", name, out, err)
		}
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("a refused work query started the %s stand-in", strings.TrimSpace(string(data)))
	}

	cmd, err := runtime.ShellCommandContext(context.Background(), `sh -c 'printf "[]"'`)
	if err != nil {
		t.Fatalf("ShellCommandContext: %v", err)
	}
	cmd.Dir = stubs
	cmd.Env = env
	_ = cmd.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: PowerShell did not start the sh stand-in (%v), so the refusals above prove nothing", err)
	}
}

// TestWorkQueryTimeoutReapsTheQuerysTreeOnWindows pins on Windows what the
// work query's process-group kill does on Unix: a query that times out takes
// every process it started with it. Without that, a bd the query started keeps
// its store connection after the query has been given up on.
func TestWorkQueryTimeoutReapsTheQuerysTreeOnWindows(t *testing.T) {
	previous := hookWorkQueryTimeout
	hookWorkQueryTimeout = 6 * time.Second
	t.Cleanup(func() { hookWorkQueryTimeout = previous })

	record := filepath.Join(t.TempDir(), "descendant")
	if _, err := shellWorkQueryWithEnv(processgrouptest.DescendantCommand(record), "", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shellWorkQueryWithEnv: %v; want the query's timeout", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if !d.WaitGone(10 * time.Second) {
		t.Fatal("the work query's descendant survived the timeout")
	}
}

// TestFinishedWorkQueryLeavesItsDescendantRunningOnWindows pins the other half
// of the Unix contract: a process group outlives its leader, so only a timeout
// reaps a query's tree, and a query that finishes leaves what it started
// running.
func TestFinishedWorkQueryLeavesItsDescendantRunningOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	// The descendant inherits the query's output pipe, so the run ends in
	// exec.ErrWaitDelay once the query exits. It must not end in the timeout,
	// which reaps the tree on purpose.
	_, err := shellWorkQueryWithEnv(processgrouptest.LeaveDescendantCommand(record), "", nil)
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("shellWorkQueryWithEnv: %v; want the query to finish on its own", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if d.WaitGone(2 * time.Second) {
		t.Fatal("the descendant died with the query that finished; releasing the query's tree must not kill it")
	}
}

// TestControlReadyScopeLegRunsBdThroughPowerShellOnWindows pins the work query
// gc composes that is not a POSIX program: the control-ready scope leg's
// `bd ready` runs through PowerShell in the scope, every argument reaches bd
// intact, and bd's rows come back byte for byte, a title outside ASCII
// included.
func TestControlReadyScopeLegRunsBdThroughPowerShellOnWindows(t *testing.T) {
	const title = "ready wörk ✓ 準備"
	n := newNativeBdTest(t)
	n.replies["--readonly --sandbox ready"] = nativeFakeBdReply{Stdout: `[{"id":"ga-ready","title":"` + title + `","status":"open"}]`}
	replies, err := json.Marshal(n.replies)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"PATH":                 filepath.Dir(n.bdBin) + string(os.PathListSeparator) + os.Getenv("PATH"),
		nativeFakeBdLogEnv:     n.log,
		nativeFakeBdRepliesEnv: string(replies),
	}

	got, err := controlReadyScopeShellReady(n.city, env, true)
	if err != nil {
		t.Fatalf("controlReadyScopeShellReady: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ga-ready" || got[0].Title != title {
		t.Fatalf("rows = %+v, want the one bd returned, titled %q", got, title)
	}
	calls := n.requireArgs([]string{
		"--readonly", "--sandbox", "ready", "--json",
		"--exclude-type=" + controlReadyExcludeType,
		fmt.Sprintf("--limit=%d", controlReadyFallbackLimit),
		"--include-ephemeral",
	})
	requireCallDir(t, calls[0], n.city)
}
