//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// The startup sweep (sweepStaleCmdGCTestDoltProcesses) reaps every dolt
// sql-server whose --config the classifier calls stale, and it judges the
// path from its own namespaces. A Bazel linux-sandbox test runs in a private
// /tmp and PID namespace, so a live sandboxed server looked stale to any
// unsandboxed sweeper (ga-dhgw89). These tests pin the predicate against real
// victims in bwrap sandboxes, which stand in for those actions.
//
// They call the predicate only. NEVER call sweepStaleCmdGCTestDoltProcesses
// from a test: it signals every eligible dolt server on the host.

// mntnsFixtureParent is the temp parent the victims' configs live under. It
// sits inside the victim's private /tmp, and its first component matches no
// sweeper prefix (Test, gct, gcx or the config allowlist), so a concurrent
// cmd/gc test binary on this host cannot classify the deliberately stale
// fixtures and reap them mid-test.
const mntnsFixtureParent = "/tmp/dolt-mntns-fixture"

// requireBwrapSandbox skips unless bwrap can build the victim sandbox.
func requireBwrapSandbox(t *testing.T, unsharePID bool) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap not available: %v", err)
	}
	args := []string{"--dev-bind", "/", "/", "--tmpfs", "/tmp"}
	if unsharePID {
		args = append(args, "--unshare-pid")
	}
	args = append(args, "--", "true")
	if _, stderr, err := runGCBeadsBdCommand(t, os.Environ(), "bwrap", args...); err != nil {
		t.Skipf("bwrap cannot build a private-/tmp sandbox here (unshare-pid=%v): %v: %s", unsharePID, err, strings.TrimSpace(stderr))
	}
}

// findFakeDoltPID returns the PID, as this process sees it, of the fake dolt
// sql-server whose --config is cfg.
func findFakeDoltPID(t *testing.T, cfg string) int {
	t.Helper()
	pid := 0
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 20 * time.Millisecond
	bo.MaxElapsedTime = 15 * time.Second
	err := backoff.Retry(func() error {
		procs, err := discoverDoltProcesses()
		if err != nil {
			return backoff.Permanent(err)
		}
		for _, p := range procs {
			if extractConfigPath(p.Argv) == cfg {
				pid = p.PID
				return nil
			}
		}
		return fmt.Errorf("no dolt sql-server with --config %s yet", cfg)
	}, bo)
	if err != nil {
		t.Fatalf("find fake dolt sql-server for %s: %v", cfg, err)
	}
	return pid
}

// startMntnsVictim starts a fake dolt sql-server for cfg inside a bwrap
// sandbox with a private /tmp, after running setup there, and returns its PID.
// wantPresent says whether cfg must be visible through the victim's root.
func startMntnsVictim(t *testing.T, cfg, setup string, unsharePID, wantPresent bool) int {
	t.Helper()
	requireBwrapSandbox(t, unsharePID)
	cmd := launchFakeDoltSQLServer(t, cfg, fakeDoltSQLServerShape{bwrap: true, unsharePID: unsharePID, setup: setup})
	pid := findFakeDoltPID(t, cfg)
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s in the test's own namespace = %v, want ENOENT: the fixture needs a private /tmp", cfg, err)
	}
	if wantPresent {
		viewed := filepath.Join("/proc", strconv.Itoa(pid), "root") + cfg
		if _, err := os.Stat(viewed); err != nil {
			t.Fatalf("stat %s: %v: the victim's config must exist in its own mount namespace", viewed, err)
		}
	}
	return pid
}

// mntnsConfigSetup returns the shell that creates cfg and its directories,
// then runs extra.
func mntnsConfigSetup(cfg, extra string) string {
	return fmt.Sprintf("mkdir -p %q\n: > %q\n%s", filepath.Dir(cfg), cfg, extra)
}

func TestIsStaleCmdGCTestConfigPathKeepsLiveTempDirConfigSeenOnlyInItsOwnMountNamespace(t *testing.T) {
	cfg := mntnsFixtureParent + "/TestLive/001/.beads/dolt-config.yaml"
	pid := startMntnsVictim(t, cfg, mntnsConfigSetup(cfg, ""), false, true)
	if isStaleCmdGCTestConfigPathWithPIDCheck(pid, cfg, nil, mntnsFixtureParent, func(int) bool {
		t.Errorf("pidAliveFn must not be consulted for a Test* temp-dir config")
		return false
	}) {
		t.Fatalf("live victim pid %d judged STALE: its --config %s exists in its own mount namespace", pid, cfg)
	}
}

func TestIsStaleCmdGCTestConfigPathReapsTempDirConfigGoneInItsOwnMountNamespace(t *testing.T) {
	cfg := mntnsFixtureParent + "/TestGone/001/.beads/dolt-config.yaml"
	pid := startMntnsVictim(t, cfg, mntnsConfigSetup(cfg, fmt.Sprintf("rm -f %q", cfg)), false, false)
	if !isStaleCmdGCTestConfigPathWithPIDCheck(pid, cfg, nil, mntnsFixtureParent, func(int) bool {
		t.Errorf("pidAliveFn must not be consulted for a Test* temp-dir config")
		return true
	}) {
		t.Fatalf("victim pid %d whose --config %s is gone in its own mount namespace judged live", pid, cfg)
	}
}

func TestIsStaleCmdGCTestConfigPathKeepsOwnerRootWhoseSentinelIsHeldInAnotherPIDNamespace(t *testing.T) {
	root := mntnsFixtureParent + "/gct39-held"
	cfg := root + "/c/dolt-config.yaml"
	// The fake inherits fd 9, so the lock lives as long as the victim does.
	setup := mntnsConfigSetup(cfg, fmt.Sprintf("exec 9>%q\nflock -n 9", root+"/"+testAliveSentinelName))
	pid := startMntnsVictim(t, cfg, setup, true, true)
	if isStaleCmdGCTestConfigPathWithPIDCheck(pid, cfg, nil, mntnsFixtureParent, func(int) bool {
		t.Errorf("pidAliveFn must not be consulted when the root's sentinel is held")
		return false
	}) {
		t.Fatalf("live victim pid %d judged STALE: its owner holds %s/%s in another PID namespace", pid, root, testAliveSentinelName)
	}
}

func TestIsStaleCmdGCTestConfigPathReapsOwnerRootWhoseSentinelIsFree(t *testing.T) {
	root := mntnsFixtureParent + "/gct39-free"
	cfg := root + "/c/dolt-config.yaml"
	setup := mntnsConfigSetup(cfg, fmt.Sprintf(": > %q", root+"/"+testAliveSentinelName))
	pid := startMntnsVictim(t, cfg, setup, true, true)
	if !isStaleCmdGCTestConfigPathWithPIDCheck(pid, cfg, nil, mntnsFixtureParent, func(int) bool {
		t.Errorf("pidAliveFn must not be consulted when the root's sentinel is present")
		return true // a coincidentally live PID in this namespace
	}) {
		t.Fatalf("victim pid %d judged live: the sentinel under %s is free, so its owner is gone", pid, root)
	}
}

func TestIsStaleCmdGCTestConfigPathKeepsSentinellessOwnerRootFromAnotherPIDNamespace(t *testing.T) {
	root := mntnsFixtureParent + "/gct39-legacy"
	cfg := root + "/c/dolt-config.yaml"
	pid := startMntnsVictim(t, cfg, mntnsConfigSetup(cfg, ""), true, true)
	if isStaleCmdGCTestConfigPathWithPIDCheck(pid, cfg, nil, mntnsFixtureParent, func(int) bool { return false }) {
		t.Fatalf("victim pid %d judged STALE: its owner PID 39 names a process in another PID namespace, so it says nothing here", pid)
	}
}

func TestIsStaleCmdGCTestConfigPathReapsSentinellessOwnerRootInThisPIDNamespaceOnlyWhenOwnerIsDead(t *testing.T) {
	root := mntnsFixtureParent + "/gct39-samens"
	cfg := root + "/c/dolt-config.yaml"
	pid := startMntnsVictim(t, cfg, mntnsConfigSetup(cfg, ""), false, true)
	for _, tc := range []struct {
		name       string
		ownerAlive bool
		want       bool
	}{
		{"owner dead", false, true},
		{"owner alive", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []int
			got := isStaleCmdGCTestConfigPathWithPIDCheck(pid, cfg, nil, mntnsFixtureParent, func(p int) bool {
				asked = append(asked, p)
				return tc.ownerAlive
			})
			if got != tc.want {
				t.Errorf("stale = %v, want %v", got, tc.want)
			}
			if len(asked) != 1 || asked[0] != 39 {
				t.Errorf("pidAliveFn asked about %v, want exactly [39]", asked)
			}
		})
	}
}

func TestPathGoneInOwnMountNamespace(t *testing.T) {
	present := filepath.Join(t.TempDir(), "present")
	if err := os.WriteFile(present, nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", present, err)
	}
	out, _, err := runGCBeadsBdCommand(t, os.Environ(), "sh", "-c", "echo $$")
	if err != nil {
		t.Fatalf("run sh to get an exited pid: %v", err)
	}
	exited, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("parse exited pid %q: %v", out, err)
	}
	for _, tc := range []struct {
		name string
		pid  int
		path string
		want bool
	}{
		{"own pid, present path", os.Getpid(), present, false},
		{"own pid, missing path", os.Getpid(), present + ".missing", true},
		{"relative path", os.Getpid(), "relative/path", false},
		{"exited pid", exited, present + ".missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathGoneInOwnMountNamespace(tc.pid, tc.path); got != tc.want {
				t.Errorf("pathGoneInOwnMountNamespace(%d, %q) = %v, want %v", tc.pid, tc.path, got, tc.want)
			}
		})
	}
}
