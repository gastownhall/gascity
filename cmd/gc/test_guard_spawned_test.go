package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// spawnedArgv0 is a real argv[0] observed on a gc binary the integration
// harness built and exec'd: no ".test" in it.
const spawnedArgv0 = "/var/tmp/gc-int-env-243249634/bin/gc-drift"

func withArgv0(t *testing.T, argv0 string) {
	t.Helper()
	prev := os.Args
	os.Args = append([]string{argv0}, prev[1:]...)
	t.Cleanup(func() { os.Args = prev })
}

// isTestBinary also gates behavior (ambient city discovery, GC_BIN
// resolution, managed-dolt test mode), so the marker must not flip it.
func TestIsTestBinary_UnchangedForSpawnedBinary(t *testing.T) {
	withArgv0(t, spawnedArgv0)
	t.Setenv(proctable.TestSpawnedEnv, "1")
	if isTestBinary() {
		t.Fatal("isTestBinary() = true for a spawned gc binary; it gates behavior and must stay argv-based")
	}
}

func TestHostGuardedProcess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		argv0  string
		marker string
		want   bool
	}{
		{"spawned gc with marker", spawnedArgv0, "1", true},
		{"go test binary", "/tmp/go-build123/b001/gc.test", "", true},
		{"production gc", "/usr/local/bin/gc", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withArgv0(t, tc.argv0)
			t.Setenv(proctable.TestSpawnedEnv, tc.marker)
			if got := hostGuardedProcess(); got != tc.want {
				t.Fatalf("hostGuardedProcess() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGuardSupervisorSocketDir_PanicsInSpawnedBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	withArgv0(t, spawnedArgv0)
	t.Setenv(proctable.TestSpawnedEnv, "1")
	defer func() {
		if recover() == nil {
			t.Fatal("guardSupervisorSocketDir did not panic for the host socket dir in a spawned binary")
		}
	}()
	guardSupervisorSocketDir(filepath.Join(home, ".gc", "run"))
}

func TestGuardSupervisorSocketDir_NoPanicInProduction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	withArgv0(t, "/usr/local/bin/gc")
	t.Setenv(proctable.TestSpawnedEnv, "")
	guardSupervisorSocketDir(filepath.Join(home, ".gc", "run"))
}

func TestGuardDelegatedSystemctlPath_PanicsInSpawnedBinary(t *testing.T) {
	withArgv0(t, spawnedArgv0)
	t.Setenv(proctable.TestSpawnedEnv, "1")
	defer func() {
		if recover() == nil {
			t.Fatal("guardDelegatedSystemctlPath did not panic for host systemctl in a spawned binary")
		}
	}()
	guardDelegatedSystemctlPath("/usr/bin/systemctl")
}
