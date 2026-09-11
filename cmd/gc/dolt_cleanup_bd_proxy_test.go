package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBdProxyRoot lays out a bd proxy root the way `bd init --proxied-server`
// does at its default location and returns the sql-server config path the
// child is launched with. A pid of 0 means "no proxy.pid at all".
func writeBdProxyRoot(t *testing.T, scope string, pid int) string {
	t.Helper()
	return writeBdProxyRootAt(t, filepath.Join(scope, ".beads", "dolt"), pid)
}

// writeBdProxyRootAt is the same fixture at an arbitrary root, which is what
// BEADS_PROXIED_SERVER_ROOT_PATH or a sidecar root_path produces.
func writeBdProxyRootAt(t *testing.T, root string, pid int) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte("listener:\n  host: 127.0.0.1\n  port: 32969\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid != 0 {
		record := fmt.Sprintf(`{"pid":%d,"port":35425,"upstream_id":"u","schema":2,"kind":"db-proxy","control_port":46445}`, pid)
		if err := os.WriteFile(filepath.Join(root, "proxy.pid"), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return configPath
}

// stubBdProxyPIDAlive swaps the liveness probe so a fabricated process table
// can drive the live and dead branches deterministically.
func stubBdProxyPIDAlive(t *testing.T, alive map[int]bool) {
	t.Helper()
	prev := bdProxyPIDAlive
	bdProxyPIDAlive = func(pid int) bool { return alive[pid] }
	t.Cleanup(func() { bdProxyPIDAlive = prev })
}

// TestClassifyDoltProcess_ProtectsBdOwnedProxyUnderTestTempRoot is the R4
// reaper case: a real-bd lifecycle test runs its proxy under t.TempDir(), so
// the sql-server's --config lands on the test-config-path allowlist. A live
// proxy.pid beside that config proves bd owns the process, and ownership wins
// over the allowlist.
func TestClassifyDoltProcess_ProtectsBdOwnedProxyUnderTestTempRoot(t *testing.T) {
	scope := filepath.Join(t.TempDir(), "city")
	configPath := writeBdProxyRoot(t, scope, 4242)
	stubBdProxyPIDAlive(t, map[int]bool{4242: true})
	if !isTestConfigPath(configPath, "/home/u", os.TempDir()) {
		t.Fatalf("fixture %q is not on the test-config-path allowlist; the test would pass vacuously", configPath)
	}

	p := DoltProcInfo{PID: 9001, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)

	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect; reason = %q", got.Action, got.Reason)
	}
	if !strings.Contains(got.Reason, "bd-owned proxied dolt server") {
		t.Errorf("Reason = %q, want the bd-ownership reason", got.Reason)
	}
	if !strings.Contains(got.Reason, "4242") {
		t.Errorf("Reason = %q, want it to name the live proxy pid", got.Reason)
	}
	if got.ConfigPath != configPath {
		t.Errorf("ConfigPath = %q, want %q", got.ConfigPath, configPath)
	}
}

// TestClassifyDoltProcess_StaleProxyPIDKeepsExistingBehaviour: a dead proxy is
// no ownership claim, so the pre-existing allowlist verdict stands.
func TestClassifyDoltProcess_StaleProxyPIDKeepsExistingBehaviour(t *testing.T) {
	scope := filepath.Join(t.TempDir(), "city")
	configPath := writeBdProxyRoot(t, scope, 4243)
	stubBdProxyPIDAlive(t, map[int]bool{4243: false})

	p := DoltProcInfo{PID: 9002, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)

	if got.Action != "reap" {
		t.Fatalf("Action = %q, want reap for a dead proxy.pid; reason = %q", got.Action, got.Reason)
	}
}

// TestClassifyDoltProcess_MissingProxyPIDKeepsExistingBehaviour: no proxy.pid
// at all is likewise no ownership claim.
func TestClassifyDoltProcess_MissingProxyPIDKeepsExistingBehaviour(t *testing.T) {
	scope := filepath.Join(t.TempDir(), "city")
	configPath := writeBdProxyRoot(t, scope, 0)
	stubBdProxyPIDAlive(t, map[int]bool{})

	p := DoltProcInfo{PID: 9003, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)

	if got.Action != "reap" {
		t.Fatalf("Action = %q, want reap when no proxy.pid exists; reason = %q", got.Action, got.Reason)
	}
}

// TestClassifyDoltProcess_ProtectsBdOwnedProxyOutsideTestRoots covers the
// production shape: a real city's live proxy is protected for the bd-ownership
// reason, not the generic not-on-allowlist one. The scope deliberately avoids
// t.TempDir(), whose "Test"-prefixed name is itself on the allowlist and would
// make the assertion vacuous.
func TestClassifyDoltProcess_ProtectsBdOwnedProxyOutsideTestRoots(t *testing.T) {
	scope, err := os.MkdirTemp("", "prod-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scope) }) //nolint:errcheck
	configPath := writeBdProxyRoot(t, scope, 4244)
	stubBdProxyPIDAlive(t, map[int]bool{4244: true})
	if isTestConfigPath(configPath, "/home/u", os.TempDir()) {
		t.Fatalf("fixture %q is on the test-config-path allowlist; the test would not prove the production shape", configPath)
	}

	p := DoltProcInfo{PID: 9004, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect; reason = %q", got.Action, got.Reason)
	}
	if !strings.Contains(got.Reason, "bd-owned proxied dolt server") {
		t.Errorf("Reason = %q, want the bd-ownership reason", got.Reason)
	}
}

// TestClassifyDoltProcess_ProtectsBdOwnedProxyWithOverriddenRoot is the
// review's MUST-FIX case: bd resolves its proxy root from
// BEADS_PROXIED_SERVER_ROOT_PATH or the sidecar's root_path, so ownership
// cannot depend on the root being named <scope>/.beads/dolt.
func TestClassifyDoltProcess_ProtectsBdOwnedProxyWithOverriddenRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "shared-server", "store")
	configPath := writeBdProxyRootAt(t, root, 4249)
	stubBdProxyPIDAlive(t, map[int]bool{4249: true})

	p := DoltProcInfo{PID: 9007, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)

	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect for an overridden proxy root; reason = %q", got.Action, got.Reason)
	}
	if !strings.Contains(got.Reason, "bd-owned proxied dolt server") {
		t.Errorf("Reason = %q, want the bd-ownership reason", got.Reason)
	}
}

// TestClassifyDoltProcess_ProtectsBdOwnedProxyWithDeletedCWD pins that
// ownership outranks the deleted-cwd reap signal: bd may have replaced the
// scope directory under a resident proxy, and killing the child is bd's call.
func TestClassifyDoltProcess_ProtectsBdOwnedProxyWithDeletedCWD(t *testing.T) {
	scope := t.TempDir()
	configPath := writeBdProxyRoot(t, scope, 4245)
	stubBdProxyPIDAlive(t, map[int]bool{4245: true})

	p := DoltProcInfo{
		PID:      9005,
		Argv:     []string{"dolt", "sql-server", "--config", configPath},
		CWDState: procPathStateDeleted,
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect; reason = %q", got.Action, got.Reason)
	}
}

// TestClassifyDoltProcess_ProtectsBdDBProxyChild is the standing guard for the
// proxy supervisor itself.
func TestClassifyDoltProcess_ProtectsBdDBProxyChild(t *testing.T) {
	p := DoltProcInfo{
		PID:  9006,
		Argv: []string{"/usr/local/bin/bd", "db-proxy-child", "--root", "/tmp/TestX/.beads/dolt", "--port", "0"},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)

	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect; reason = %q", got.Action, got.Reason)
	}
	if !strings.Contains(got.Reason, "db-proxy-child") {
		t.Errorf("Reason = %q, want it to name the bd proxy supervisor", got.Reason)
	}
}

func TestBdOwnedProxyDoltConfigRejectsForeignLayouts(t *testing.T) {
	scope := t.TempDir()
	configPath := writeBdProxyRoot(t, scope, 4246)
	stubBdProxyPIDAlive(t, map[int]bool{4246: true})

	cases := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"wrong file name", filepath.Join(filepath.Dir(configPath), "dolt-config.yaml")},
		// A config.yaml with no proxy.pid beside it is some other server's
		// config: the pid record, not the directory name, is the proof.
		{"no proxy pid sibling", filepath.Join(scope, "unrelated", "config.yaml")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := bdOwnedProxyDoltConfig(tc.path); ok {
				t.Fatalf("bdOwnedProxyDoltConfig(%q) claimed bd ownership", tc.path)
			}
		})
	}
	if _, ok := bdOwnedProxyDoltConfig(configPath); !ok {
		t.Fatal("bdOwnedProxyDoltConfig rejected the canonical bd proxy layout")
	}
}

func TestBdOwnedProxyDoltConfigRejectsNonProxyRecords(t *testing.T) {
	scope := t.TempDir()
	configPath := writeBdProxyRoot(t, scope, 4247)
	stubBdProxyPIDAlive(t, map[int]bool{4247: true, 4248: true})
	pidPath := filepath.Join(filepath.Dir(configPath), "proxy.pid")

	for _, body := range []string{
		`{"pid":4248,"kind":"something-else"}`,
		`{"pid":0,"kind":"db-proxy"}`,
		`{"pid":-1,"kind":"db-proxy"}`,
		"not json",
	} {
		if err := os.WriteFile(pidPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := bdOwnedProxyDoltConfig(configPath); ok {
			t.Fatalf("bdOwnedProxyDoltConfig accepted proxy.pid %q", body)
		}
	}
}
