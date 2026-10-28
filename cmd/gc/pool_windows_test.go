//go:build windows

package main

import (
	"bytes"
	"errors"
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

// TestShellScaleCheckRunsThroughPowerShell pins native shell site 6 on
// Windows: a scale_check runs through PowerShell, in the directory and with
// the environment the probe hands it, and its count reaches evaluatePool.
func TestShellScaleCheckRunsThroughPowerShell(t *testing.T) {
	dir := t.TempDir()
	out, err := shellScaleCheck(`"$($PSVersionTable.PSEdition)|$env:GC_SCALE_PROBE|$((Get-Location).Path)"`, dir, map[string]string{"GC_SCALE_PROBE": "probe-env"})
	if err != nil {
		t.Fatalf("shellScaleCheck: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 3 {
		t.Fatalf("output = %q, want edition|env|dir", out)
	}
	if fields[0] != "Core" && fields[0] != "Desktop" {
		t.Errorf("PSEdition = %q, want the check run by PowerShell", fields[0])
	}
	if fields[1] != "probe-env" {
		t.Errorf("GC_SCALE_PROBE = %q, want the probe's environment", fields[1])
	}
	if !strings.EqualFold(filepath.Clean(fields[2]), filepath.Clean(dir)) {
		t.Errorf("working directory = %q, want %q", fields[2], dir)
	}

	got, err := evaluatePool("worker", scaleParams{Min: 0, Max: 5, Check: "Write-Output 3"}, dir, nil, shellScaleCheck)
	if err != nil || got != 3 {
		t.Fatalf("evaluatePool = %d, %v; want 3", got, err)
	}
}

// TestReconcilerProbesRefusePosixShellsOnWindows pins the gate on site 6: a
// probe or hook whose program is a POSIX shell starts nothing. The read
// programs gc generates itself are the exception, run in its embedded
// interpreter (TestGeneratedPoolDemandCheckRunsInProcessOnWindows); an edited
// generated scale_check is still refused, and its pool falls back to its
// minimum. Git's sh is on PATH under the test runner, so a refused command
// that ran anyway would succeed.
func TestReconcilerProbesRefusePosixShellsOnWindows(t *testing.T) {
	runners := map[string]ScaleCheckRunner{"scale_check": shellScaleCheck, "hook": shellRunHook}
	for name, run := range runners {
		out, err := run(`sh -c 'echo 3'`, "", nil)
		if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "POSIX shell") {
			t.Errorf("%s: output %q, error %v; want sh refused", name, out, err)
		}
	}

	agent := &config.Agent{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}
	sp := scaleParamsFor(agent)
	edited := strings.Replace(sp.Check, `target="$1";`, `target="$1" ;`, 1)
	if edited == sp.Check {
		t.Fatal("the edit did not change the generated scale_check")
	}
	sp.Check = edited
	got, err := evaluatePool("worker", sp, t.TempDir(), nil, shellScaleCheck)
	if err == nil || !strings.Contains(err.Error(), "POSIX shell") {
		t.Fatalf("evaluatePool with an edited generated scale_check: error %v, want a POSIX shell refusal", err)
	}
	if got != 1 {
		t.Fatalf("evaluatePool = %d, want the minimum, 1", got)
	}
}

// TestPrefixShellEnvLeavesTheCommandToTheGateOnWindows pins that the probe
// environment prefix is not rendered on Windows, where PowerShell has no
// POSIX assignment prefix. A prefix in any form would put something other
// than the command's program first, and hide a POSIX shell from
// runtime.ShellCommandUnavailable.
func TestPrefixShellEnvLeavesTheCommandToTheGateOnWindows(t *testing.T) {
	env := map[string]string{"GC_DOLT_HOST": "127.0.0.1", "GC_DOLT_PORT": "3307"}
	if got := prefixShellEnv(env, "  Write-Output 3  "); got != "Write-Output 3" {
		t.Errorf("prefixShellEnv = %q, want the trimmed command alone", got)
	}
	generated := scaleParamsFor(&config.Agent{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3)}).Check
	if check := prefixShellEnv(env, generated); runtime.ShellCommandUnavailable(check) == "" {
		t.Fatalf("the generated scale_check, prefixed, is not refused: %q", check)
	}
}

// TestGeneratedRecoveryHooksArePlannedOnWindows pins that each pool agent's
// generated on_boot and on_death are planned on Windows exactly as generated,
// beside an agent's own hooks, with nothing reported: they run in gc's
// embedded interpreter (TestGeneratedOnBootRunsInProcessOnWindows,
// TestGeneratedOnDeathRunsInProcessOnWindows), and an agent's own in
// PowerShell.
func TestGeneratedRecoveryHooksArePlannedOnWindows(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents: []config.Agent{
			{Name: "generated", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
			{Name: "custom", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2), OnBoot: "Write-Output boot", OnDeath: "Write-Output death"},
		},
	}
	topo := config.QueryTopology{Beads: cfg.Beads}

	var stderr bytes.Buffer
	hooks := planPoolOnBootHooks(cfg, cityPath, &stderr)
	if len(hooks) != 2 || hooks[0].agent != "generated" || hooks[0].command != cfg.Agents[0].EffectiveOnBootFor(topo) ||
		hooks[1].agent != "custom" || hooks[1].command != "Write-Output boot" {
		t.Fatalf("planned on_boot hooks = %+v, want generated's default and custom's own", hooks)
	}
	if generatedProgram(hooks[0].command) == nil || generatedProgram(hooks[1].command) != nil {
		t.Errorf("want generated's on_boot alone run in the embedded interpreter")
	}

	handlers := computePoolDeathHandlers(cfg, "test", cityPath, runtime.NewFake(), &stderr)
	if len(handlers) != 4 {
		t.Fatalf("on_death handlers %v, want one for each instance of each pool", handlerKeys(handlers))
	}
	for sessionName, info := range handlers {
		switch {
		case strings.HasPrefix(sessionName, "custom-"):
			if info.Command != "Write-Output death" {
				t.Errorf("handler[%s] = %+v, want custom's own on_death", sessionName, info)
			}
		case strings.HasPrefix(sessionName, "generated-"):
			instance := deepCopyAgent(&cfg.Agents[0], sessionName, cfg.Agents[0].Dir)
			if info.Command != instance.EffectiveOnDeathFor(topo) || generatedProgram(info.Command) == nil {
				t.Errorf("handler[%s] = %+v, want the instance's generated on_death, run in the embedded interpreter", sessionName, info)
			}
		default:
			t.Errorf("unexpected handler %s", sessionName)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing reported", stderr.String())
	}
}

// TestEditedGeneratedRecoveryHooksAreNotRunOnWindows pins the guard on the
// generated hooks: one that no longer reads as gc generated it, here because
// template expansion rewrote the agent name inside it, is neither planned nor
// handed to PowerShell, which would run part of it and start awk and xargs
// from PATH with it. Each kind is reported once for the agent.
func TestEditedGeneratedRecoveryHooksAreNotRunOnWindows(t *testing.T) {
	const name = "edited-hook-probe{{.Rig}}"
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: name, MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
	}
	if expanded := expandAgentCommandTemplate(cityPath, "test", &cfg.Agents[0], nil, "on_boot", cfg.Agents[0].EffectiveOnBoot(), nil); !strings.Contains(expanded, "'edited-hook-probe'") {
		t.Fatalf("template expansion left the generated on_boot as %q, so this test proves nothing", expanded)
	}
	log := installFakeBdOnPath(t, "", map[string]nativeFakeBdReply{"": {Stdout: "[]"}})
	stubs, marker := posixShellStandIns(t)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, cityPath, shellRunHook, &stderr)
	if want := "on_boot " + name + ": not run: the default hook is a POSIX shell program"; strings.Count(stderr.String(), want) != 1 {
		t.Errorf("stderr = %q, want the on_boot reported once", stderr.String())
	}

	stderr.Reset()
	if handlers := computePoolDeathHandlers(cfg, "test", cityPath, runtime.NewFake(), &stderr); len(handlers) != 0 {
		t.Errorf("on_death handlers %v, want none", handlerKeys(handlers))
	}
	if want := "on_death " + name + ": not run: the default hook is a POSIX shell program"; strings.Count(stderr.String(), want) != 1 {
		t.Errorf("stderr = %q, want the on_death reported once for the agent, not once for each instance", stderr.String())
	}

	if calls := readFakeBdCalls(t, log); len(calls) != 0 {
		t.Errorf("bd ran %+v, want nothing", calls)
	}
	if data, err := os.ReadFile(marker); err == nil {
		t.Fatalf("an edited hook started the %s stand-in", strings.TrimSpace(string(data)))
	}
}

// TestOnBootOverrideRunsThroughPowerShell pins the production hook runner on
// Windows end to end: an agent's own on_boot runs under PowerShell, in its
// pool's directory.
func TestOnBootOverrideRunsThroughPowerShell(t *testing.T) {
	cityPath := t.TempDir()
	marker := filepath.Join(t.TempDir(), "booted.txt")
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(2),
			OnBoot:            "Set-Content -LiteralPath " + psLiteral(marker) + " -Value (Get-Location).Path",
		}},
	}

	var stderr bytes.Buffer
	runPoolOnBoot(cfg, cityPath, shellRunHook, &stderr)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("on_boot did not run (%v); stderr: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(string(data)); !strings.EqualFold(got, cityPath) {
		t.Errorf("on_boot ran in %q, want the city root %q", got, cityPath)
	}
}

// descendantAfterTimeout runs a command that starts a long-running descendant
// and then outwaits its timeout, and returns the descendant the command
// recorded. prepare is runShellCommand's.
func descendantAfterTimeout(t *testing.T, prepare func(*exec.Cmd)) processgrouptest.Descendant {
	t.Helper()
	record := filepath.Join(t.TempDir(), "descendant")
	if _, err := runShellCommand(processgrouptest.DescendantCommand(record), "", 6*time.Second, nil, prepare); err == nil {
		t.Fatal("command returned without error; it was supposed to hit its timeout")
	}
	return processgrouptest.ReadDescendant(t, record)
}

// TestHookTimeoutReapsTheHooksTreeOnWindows pins on Windows the containment
// TestHookTimeoutKillsTheWholeProcessGroup pins on Unix: a hook that times out
// takes every process it started with it, because it runs in a Job Object
// that owns its tree. The control is the point, as it is there: the same
// fixture without the hook's cleanup must leave the descendant running.
func TestHookTimeoutReapsTheHooksTreeOnWindows(t *testing.T) {
	t.Run("the hook's tree is reaped", func(t *testing.T) {
		d := descendantAfterTimeout(t, hookProcessGroupCleanup)
		if !d.WaitGone(10 * time.Second) {
			t.Fatal("the hook's descendant survived the timeout; a freed pool slot does not mean the work stopped")
		}
	})

	t.Run("control: without it the descendant survives", func(t *testing.T) {
		d := descendantAfterTimeout(t, nil)
		if d.WaitGone(2 * time.Second) {
			t.Fatal("the descendant died without the hook's cleanup, so the row above proves nothing about it")
		}
	})
}

// TestFinishedHookLeavesItsDescendantRunningOnWindows pins the other half of
// the Unix contract: a process group outlives its leader, so only a timeout
// reaps a hook's tree, and a hook that finishes leaves what it started
// running. On Windows that is the difference between releasing the Job Object
// and killing it on release.
func TestFinishedHookLeavesItsDescendantRunningOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	// The descendant inherits the hook's output pipe, so the run ends in
	// exec.ErrWaitDelay once the hook exits. It must not end in the timeout,
	// whose cleanup reaps the tree on purpose.
	_, err := runShellCommand(processgrouptest.LeaveDescendantCommand(record), "", 30*time.Second, nil, hookProcessGroupCleanup)
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("runShellCommand: %v; want the hook to finish on its own", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if d.WaitGone(2 * time.Second) {
		t.Fatal("the descendant died with the hook that finished; releasing the hook's tree must not kill it")
	}
}
