//go:build windows

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// TestBuiltinProviderOwnedOpIsNativeOnlyForTheCitysOwnEntrypoints pins which
// scripts run natively: the city's gc-beads-bd entrypoints, current and
// legacy, and nothing else.
func TestBuiltinProviderOwnedOpIsNativeOnlyForTheCitysOwnEntrypoints(t *testing.T) {
	city := t.TempDir()
	environ := []string{"GC_CITY_PATH=" + city}
	for _, script := range []string{gcBeadsBdScriptPath(city), legacySystemPacksGcBeadsBdScriptPath(city)} {
		if builtinProviderOwnedOp(script, environ) == nil {
			t.Errorf("%s is the city's own provider and must run natively", script)
		}
	}
	elsewhere := filepath.Join(t.TempDir(), ".gc", "scripts", "gc-beads-bd.sh")
	if builtinProviderOwnedOp(elsewhere, environ) != nil {
		t.Errorf("%s is outside the city: a custom provider, which runs as itself", elsewhere)
	}
	if builtinProviderOwnedOp(gcBeadsBdScriptPath(city), []string{"GC_CITY_PATH=" + t.TempDir()}) != nil {
		t.Errorf("another city's entrypoint must not run natively for this one")
	}
	if builtinProviderOwnedOp(gcBeadsBdScriptPath(city), nil) != nil {
		t.Errorf("without GC_CITY_PATH no city owns the script")
	}
}

// TestRunProviderOwnedOpStrictRunsTheCityProviderNatively pins the strict
// runner over the native provider: the entrypoint itself is never needed, bd's
// own failure keeps the text and the mark the script's exit gave it, the
// script's own refusal stays unmarked, and gc's deadline still ends the op.
func TestRunProviderOwnedOpStrictRunsTheCityProviderNatively(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	script := gcBeadsBdScriptPath(n.city)
	if _, err := os.Stat(script); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("entrypoint %s exists (stat err %v); this test needs it absent", script, err)
	}
	ctx := context.Background()

	if err := runProviderOwnedOpStrict(ctx, 30*time.Second, script, n.environ(scope), "health"); err != nil {
		t.Fatalf("health: %v", err)
	}
	n.requireArgs([]string{"ping"})

	n.reset()
	n.replies["ping"] = nativeFakeBdReply{Exit: 1, Stderr: "Error: cannot reach the dolt server\n"}
	err := runProviderOwnedOpStrict(ctx, 30*time.Second, script, n.environ(scope), "health")
	if err == nil || err.Error() != "provider-owned beads health: Error: cannot reach the dolt server" {
		t.Fatalf("health error = %v, want bd's own diagnostics", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("health error = %v (%T), want bd's exit status 1 as its cause", err, err)
	}
	if !errors.Is(markProviderReportedFailure(err), beads.ErrProviderReportedFailure) {
		t.Fatalf("bd's own failure must read as the provider's answer")
	}

	n.reset()
	delete(n.replies, "ping")
	err = runProviderOwnedOpStrict(ctx, 30*time.Second, script, n.environ(scope, "GC_DOLT=skip"), "health")
	if err == nil || err.Error() != "provider-owned beads health: exit status 2" {
		t.Fatalf("health under GC_DOLT=skip = %v, want the script's not-needed status", err)
	}
	if errors.Is(markProviderReportedFailure(err), beads.ErrProviderReportedFailure) {
		t.Fatalf("the script's own refusal is not bd's answer")
	}
	n.requireArgs()

	n.reset()
	n.replies["ping"] = nativeFakeBdReply{SleepMS: 60_000}
	started := time.Now()
	err = runProviderOwnedOpStrict(ctx, time.Second, script, n.environ(scope), "health")
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "provider-owned beads health: context deadline exceeded" {
		t.Fatalf("health past its deadline = %v, want gc's deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("health took %s past a 1s deadline; bd was not ended with it", elapsed)
	}
}

func TestNativeProviderTimeoutReapsBdTreeOnWindows(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	record := filepath.Join(t.TempDir(), "descendant")
	stubs := bdStandIn(t, processgrouptest.DescendantCommand(record))

	err := runProviderOwnedOpStrict(context.Background(), 6*time.Second, gcBeadsBdScriptPath(n.city), n.environ(scope, "BD_BIN="+filepath.Join(stubs, "bd.cmd")), "health")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("health = %v, want gc's deadline", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if !d.WaitGone(10 * time.Second) {
		t.Fatal("the native provider killed bd but left its descendant alive")
	}
}

func TestNativeProviderInitTimeoutReapsBdTreeOnWindows(t *testing.T) {
	n := newNativeBdTest(t)
	scope := filepath.Join(n.city, "new-scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "descendant")
	stubs := bdStandIn(t, processgrouptest.DescendantCommand(record))
	env := n.environ(scope,
		"BD_BIN="+filepath.Join(stubs, "bd.cmd"),
		"GC_BEADS_TRANSPORT=direct",
		"GC_BEADS_TARGET=local",
	)

	err := runProviderOwnedOpStrict(context.Background(), 6*time.Second, gcBeadsBdScriptPath(n.city), env, "init", scope, "gt")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("init = %v, want gc's deadline", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if !d.WaitGone(10 * time.Second) {
		t.Fatal("the native provider init killed bd but left its descendant alive")
	}
}

func TestFinishedNativeProviderOpLeavesBdDescendantRunningOnWindows(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	record := filepath.Join(t.TempDir(), "descendant")
	stubs := bdStandIn(t, processgrouptest.LeaveDetachedDescendantCommand(record))

	if err := runProviderOwnedOpStrict(context.Background(), 30*time.Second, gcBeadsBdScriptPath(n.city), n.environ(scope, "BD_BIN="+filepath.Join(stubs, "bd.cmd")), "health"); err != nil {
		t.Fatalf("health: %v", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if d.WaitGone(2 * time.Second) {
		t.Fatal("a successful native provider op killed bd's detached descendant")
	}
}

func TestNativeProviderRecoverTimeoutSparesEarlierDetachedBdDescendantOnWindows(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	record := filepath.Join(t.TempDir(), "descendant")
	script := "$record = '" + strings.ReplaceAll(record, "'", "''") + "'; " +
		"if (-not (Test-Path -LiteralPath $record)) { " +
		processgrouptest.LeaveDetachedDescendantCommand(record) + "; exit 0 }; " +
		"Set-Content -LiteralPath ($record + '.later') -Value 'second bd is running'; " +
		"Start-Sleep -Seconds 300"
	stubs := bdStandIn(t, script)

	err := runProviderOwnedOpStrict(context.Background(), 6*time.Second, gcBeadsBdScriptPath(n.city), n.environ(scope, "BD_BIN="+filepath.Join(stubs, "bd.cmd")), "recover")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recover = %v, want gc's deadline", err)
	}
	if _, err := os.Stat(record + ".later"); err != nil {
		t.Fatalf("the later bd did not start; the test did not reach the intended schedule: %v", err)
	}
	d := processgrouptest.ReadDescendant(t, record)
	if d.WaitGone(2 * time.Second) {
		t.Fatal("the provider op timeout killed the completed first recovery bd's detached descendant")
	}
	d.Terminate()
	if !d.WaitGone(5 * time.Second) {
		t.Fatal("the completed first recovery bd's detached descendant did not exit during test cleanup")
	}
}

// TestRunProviderOwnedOpStrictLeavesACustomProviderAlone pins the other side:
// a provider script that is not the city's entrypoint runs as itself, so on
// Windows a POSIX one still cannot run, and the native provider never stands
// in for it.
func TestRunProviderOwnedOpStrictLeavesACustomProviderAlone(t *testing.T) {
	n := newNativeBdTest(t)
	scope := n.scope("", nativeDirectLocalScope)
	custom := filepath.Join(n.root, "custom", "gc-beads-bd.sh")
	if err := os.MkdirAll(filepath.Dir(custom), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(custom, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runProviderOwnedOpStrict(context.Background(), 30*time.Second, custom, n.environ(scope), "health"); err == nil {
		t.Fatalf("a POSIX custom provider ran on Windows")
	}
	n.requireArgs()
}
