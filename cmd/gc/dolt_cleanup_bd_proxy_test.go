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

// stubBdProxyProcesses swaps both process-table reads the ownership proof
// makes, so a test can fabricate exactly which process each proxy.pid names.
// A pid absent from procs is dead.
func stubBdProxyProcesses(t *testing.T, procs map[int][]string) {
	t.Helper()
	prevAlive, prevArgv := bdProxyPIDAlive, bdProxyProcessArgv
	bdProxyPIDAlive = func(pid int) bool { _, live := procs[pid]; return live }
	bdProxyProcessArgv = func(pid int) ([]string, error) {
		argv, live := procs[pid]
		if !live {
			return nil, fmt.Errorf("no process %d", pid)
		}
		return argv, nil
	}
	t.Cleanup(func() { bdProxyPIDAlive, bdProxyProcessArgv = prevAlive, prevArgv })
}

// bdProxyChildArgv is what bd execs for a proxy rooted at the directory
// holding configPath.
func bdProxyChildArgv(configPath string) []string {
	return []string{"/usr/local/bin/bd", "db-proxy-child", "--root", filepath.Dir(configPath), "--port", "0"}
}

// stubLiveBdProxy is the ordinary case: one live proxy supervising one root.
func stubLiveBdProxy(t *testing.T, pid int, configPath string) {
	t.Helper()
	stubBdProxyProcesses(t, map[int][]string{pid: bdProxyChildArgv(configPath)})
}

// stubNoBdProxyProcesses fabricates an empty process table.
func stubNoBdProxyProcesses(t *testing.T) {
	t.Helper()
	stubBdProxyProcesses(t, nil)
}

// TestClassifyDoltProcess_ProtectsBdOwnedProxyUnderTestTempRoot is the R4
// reaper case: a real-bd lifecycle test runs its proxy under t.TempDir(), so
// the sql-server's --config lands on the test-config-path allowlist. A live
// proxy.pid beside that config proves bd owns the process, and ownership wins
// over the allowlist.
func TestClassifyDoltProcess_ProtectsBdOwnedProxyUnderTestTempRoot(t *testing.T) {
	scope := filepath.Join(t.TempDir(), "city")
	configPath := writeBdProxyRoot(t, scope, 4242)
	stubLiveBdProxy(t, 4242, configPath)
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
	stubNoBdProxyProcesses(t)

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
	stubNoBdProxyProcesses(t)

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
	stubLiveBdProxy(t, 4244, configPath)
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
	stubLiveBdProxy(t, 4249, configPath)

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
	stubLiveBdProxy(t, 4245, configPath)

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
	stubLiveBdProxy(t, 4246, configPath)

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
	stubBdProxyProcesses(t, map[int][]string{
		4247: bdProxyChildArgv(configPath),
		4248: bdProxyChildArgv(configPath),
	})
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

// A live PID is not an ownership proof. bd leaves proxy.pid behind when its
// proxy is SIGKILLed and only quarantines the record on its next adoption in
// that root, which never happens for an abandoned test root — so once the
// recorded PID is recycled, a liveness-only check protected the orphaned
// sql-server for as long as the coincidental occupant lived. The forged shape
// is worse: any writable directory with a config.yaml and a hand-written
// proxy.pid naming PID 1 made every server started with that --config
// unreapable, because kill(1, 0) returns EPERM, which reads as alive.
func TestBdOwnedProxyDoltConfigRejectsRecycledAndForgedPIDs(t *testing.T) {
	for _, tt := range []struct {
		name string
		argv []string
	}{
		{name: "recycled pid", argv: []string{"/usr/bin/sleep", "infinity"}},
		{name: "forged init pid", argv: []string{"/sbin/init", "splash"}},
		{name: "bd but not the proxy", argv: []string{"/usr/local/bin/bd", "list", "--json"}},
		{name: "unreadable argv", argv: nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope := t.TempDir()
			configPath := writeBdProxyRoot(t, scope, 4250)
			stubBdProxyProcesses(t, map[int][]string{4250: tt.argv})
			if _, ok := bdOwnedProxyDoltConfig(configPath); ok {
				t.Fatal("bdOwnedProxyDoltConfig claimed bd ownership from a live pid alone")
			}
			p := DoltProcInfo{PID: 9008, Argv: []string{"dolt", "sql-server", "--config", configPath}}
			if got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil); got.Action != "reap" {
				t.Fatalf("Action = %q, want reap; reason = %q", got.Action, got.Reason)
			}
		})
	}
}

// A live `bd db-proxy-child` rooted somewhere else is somebody else's proxy.
// Its PID landing in this root's proxy.pid proves nothing about this root.
func TestBdOwnedProxyDoltConfigRejectsProxyForAnotherRoot(t *testing.T) {
	scope := t.TempDir()
	configPath := writeBdProxyRoot(t, scope, 4251)
	otherRoot := filepath.Join(t.TempDir(), "other", ".beads", "dolt")
	stubBdProxyProcesses(t, map[int][]string{
		4251: {"/usr/local/bin/bd", "db-proxy-child", "--root", otherRoot, "--port", "0"},
	})
	if _, ok := bdOwnedProxyDoltConfig(configPath); ok {
		t.Fatal("bdOwnedProxyDoltConfig accepted a proxy rooted at another workspace")
	}
}

// bd execs its proxy child with os.Executable(), so argv[0] is the operator's
// bd file — `BD_BIN=/opt/beads/bd-rc2` is a supported pin and produces a proxy
// whose argv[0] is not named "bd". Ownership must key on the verb and the
// root, or every versioned install loses the R4 protection: its live,
// bd-owned sql-server under a test root would classify as reap.
func TestClassifyDoltProcess_ProtectsBdOwnedProxyFromVersionedBdBinary(t *testing.T) {
	scope := t.TempDir()
	configPath := writeBdProxyRoot(t, scope, 4253)
	stubBdProxyProcesses(t, map[int][]string{
		4253: {"/opt/beads/bd-rc2", "db-proxy-child", "--root", filepath.Dir(configPath), "--port", "0"},
	})
	if !isTestConfigPath(configPath, "/home/u", os.TempDir()) {
		t.Fatalf("fixture %q is not on the test-config-path allowlist; the test would pass vacuously", configPath)
	}

	if _, ok := bdOwnedProxyDoltConfig(configPath); !ok {
		t.Fatal("bdOwnedProxyDoltConfig rejected a live proxy whose binary is not named \"bd\"")
	}

	p := DoltProcInfo{PID: 9009, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)
	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect; reason = %q", got.Action, got.Reason)
	}
	if !strings.Contains(got.Reason, "bd-owned proxied dolt server") {
		t.Errorf("Reason = %q, want the bd-ownership reason", got.Reason)
	}
}

// The standing guard for the supervisor itself must not key on the binary
// name either, for the same reason.
func TestClassifyDoltProcess_ProtectsVersionedBdDBProxyChild(t *testing.T) {
	p := DoltProcInfo{
		PID:  9010,
		Argv: []string{"/opt/beads/bd-rc2", "db-proxy-child", "--root", "/tmp/TestX/.beads/dolt", "--port", "0"},
	}
	got := classifyDoltProcess(p, nil, "/home/u", "", nil)
	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect; reason = %q", got.Action, got.Reason)
	}
	// Not the generic "no --config path detected" fallback: the guard must
	// recognize the supervisor, not merely fail to identify it.
	if !strings.Contains(got.Reason, "db-proxy-child") {
		t.Errorf("Reason = %q, want it to name the bd proxy supervisor", got.Reason)
	}
}

// A rig migrated by `gc beads city migrate-proxied` shares the CITY's proxy
// root: its metadata carries a relative dolt_data_dir and it has no
// .beads/dolt of its own, so the per-scope predicate
// "<scope>/.beads/dolt/config.yaml + live proxy.pid" does not describe it
// (B5b §8). It does not have to. The reaper classifies processes from argv
// alone, and the shared root has exactly one sql-server, launched with
// --config <city>/.beads/dolt/config.yaml — the city's root, where the live
// proxy.pid is. Resolving each scope's provider root would add a mapping with
// no process on the other end of it.
func TestClassifyDoltProcess_ProtectsSharedRootRigChild(t *testing.T) {
	city := filepath.Join(t.TempDir(), "city")
	configPath := writeBdProxyRoot(t, city, 4254)
	stubLiveBdProxy(t, 4254, configPath)

	// The migrated rig: proxied metadata pointing back at the city's root,
	// and deliberately no proxy root of its own.
	rig := filepath.Join(city, "rigs", "app")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"app","dolt_data_dir":"../../.beads/dolt"}`
	if err := os.WriteFile(filepath.Join(rig, ".beads", "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rig, ".beads", "dolt", "proxy.pid")); !os.IsNotExist(err) {
		t.Fatalf("fixture grew its own proxy root; the shared-root shape is not under test")
	}
	if root, err := proxiedScopeProviderRoot(rig); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(filepath.Join(root, "proxy.pid")); !os.IsNotExist(err) {
		t.Fatalf("per-scope provider root %q unexpectedly has a proxy.pid", root)
	}

	// bd runs one child for the shared root, and it names the city's config.
	p := DoltProcInfo{PID: 9011, Argv: []string{"dolt", "sql-server", "--config", configPath}}
	got := classifyDoltProcess(p, nil, "/home/u", os.TempDir(), nil)
	if got.Action != "protect" {
		t.Fatalf("Action = %q, want protect for a shared-root rig's child; reason = %q", got.Action, got.Reason)
	}
	if !strings.Contains(got.Reason, "bd-owned proxied dolt server") {
		t.Errorf("Reason = %q, want the bd-ownership reason", got.Reason)
	}
}

// bd writes its flags as `--root <value>`; a `--root=<value>` spelling and a
// symlinked path to the same directory are the same proxy.
func TestBdOwnedProxyDoltConfigAcceptsEquivalentRootSpellings(t *testing.T) {
	scope := t.TempDir()
	configPath := writeBdProxyRoot(t, scope, 4252)
	root := filepath.Dir(configPath)
	link := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"/usr/local/bin/bd", "db-proxy-child", "--root=" + root},
		{"/usr/local/bin/bd", "db-proxy-child", "--root", link},
	} {
		stubBdProxyProcesses(t, map[int][]string{4252: argv})
		if _, ok := bdOwnedProxyDoltConfig(configPath); !ok {
			t.Fatalf("bdOwnedProxyDoltConfig rejected argv %v", argv)
		}
	}
}
