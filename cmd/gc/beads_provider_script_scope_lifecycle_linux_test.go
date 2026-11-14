// This file is Linux-only by its filename, for the same reason as
// beads_provider_missing_scope_stop_linux_test.go: it executes the POSIX
// provider script, and 07-design scopes the proxied lifecycle to Linux.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

// providerScriptPath is the real script every test in this file drives. The
// decisions under test are shell decisions — a `cd`, a sidecar read, a case arm
// — so only the shell can prove them.
func providerScriptPath(t *testing.T) string {
	t.Helper()
	script := filepath.Join("..", "..", "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("provider script not available: %v", err)
	}
	return script
}

// runProviderOwnedScriptOp runs one provider-owned op, with its positional
// arguments, against the real script.
//
// It is the single subprocess call site for the provider-script tests on purpose:
// the source resource census ratchet is shrink-only, so a new shape is covered by
// another call to this helper rather than another exec.Command.
func runProviderOwnedScriptOp(t *testing.T, env []string, op string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{providerScriptPath(t), op}, args...)...) //nolint:gosec // repository script under test
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// providerOwnedScriptEnv is the environment gc's provider adapter passes for a
// proxied, provider-owned scope.
func providerOwnedScriptEnv(city, scope, bdBin string) []string {
	return append(os.Environ(),
		"GC_CITY_PATH="+city,
		"GC_BEADS_PROVIDER_OWNED=1",
		"GC_BEADS_TRANSPORT=proxied",
		"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
		"GC_BEADS_TARGET=local",
		"BEADS_DOLT_PROXIED_SERVER=1",
		"GC_DOLT=",
		"BEADS_DIR="+filepath.Join(scope, ".beads"),
		"BD_BIN="+bdBin,
	)
}

// writeProxiedSidecar plants the bd client-info sidecar that names the physical
// Dolt root a proxied scope's lifecycle commands act on.
func writeProxiedSidecar(t *testing.T, scope, rootPath string) {
	t.Helper()
	beads := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"root_path": "` + rootPath + `", "idle_timeout": -1}`
	if err := os.WriteFile(filepath.Join(beads, "proxied_server_client_info.json"), []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

// writeRecordingBd installs a bd that logs its argv and succeeds, so a test can
// assert on the lifecycle commands the script chose to issue.
func writeRecordingBd(t *testing.T, dir string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "bd-calls.log")
	bin = filepath.Join(dir, "bd")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	return bin, logPath
}

func bdCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestProviderScriptRecoverLeavesTheSharedCityProxyAlone pins the invariant that
// a single flaky rig must not cycle the city's Dolt and every other rig's.
//
// `gc beads city migrate-proxied` points a rig's Dolt data dir at the city's
// .beads/dolt, and bd resolves the root for `bd dolt stop` from the sidecar
// rather than from BEADS_DIR. So a rig-scoped recover used to shut down the one
// proxy and Dolt child serving hq and every other rig, under live agents, to
// recover one rig — and the ping that follows cold-starts it while the rig-local
// cause is still there, so the next health pass does it again.
func TestProviderScriptRecoverLeavesTheSharedCityProxyAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rootPath is the rig sidecar's root_path, relative to the rig or absolute.
		rigRootPath func(city, rig string) string
		wantStop    bool
	}{
		{
			// Three segments, not two: bd resolves a relative root_path
			// against BEADS_DIR (`<rig>/.beads`), so the city's root is three
			// levels up from a rig at <city>/rigs/api. The two-segment spelling
			// this used to pin resolves to <city>/rigs/.beads/dolt for bd —
			// a root the rig would own — and only matched because the script
			// joined it to the scope dir instead.
			name:        "shared city root via a relative data dir",
			rigRootPath: func(_, _ string) string { return filepath.Join("..", "..", "..", ".beads", "dolt") },
			wantStop:    false,
		},
		{
			name:        "shared city root spelled absolutely",
			rigRootPath: func(city, _ string) string { return filepath.Join(city, ".beads", "dolt") },
			wantStop:    false,
		},
		{
			name:        "the rig's own root is the rig's to cycle",
			rigRootPath: func(_, rig string) string { return filepath.Join(rig, ".beads", "dolt") },
			wantStop:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			rig := filepath.Join(city, "rigs", "api")
			for _, dir := range []string{
				filepath.Join(city, ".beads", "dolt"),
				filepath.Join(rig, ".beads", "dolt"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))
			writeProxiedSidecar(t, rig, tc.rigRootPath(city, rig))

			bin, logPath := writeRecordingBd(t, city)
			out, err := runProviderOwnedScriptOp(t, providerOwnedScriptEnv(city, rig, bin), "recover")
			if err != nil {
				t.Fatalf("recover on the rig: %v\n%s", err, out)
			}

			calls := bdCalls(t, logPath)
			var stopped bool
			for _, call := range calls {
				if strings.HasPrefix(call, "dolt stop") {
					stopped = true
				}
			}
			if stopped != tc.wantStop {
				t.Fatalf("rig recover issued `bd dolt stop` = %v, want %v; bd calls %q\n%s", stopped, tc.wantStop, calls, out)
			}
			// Either way recover must still re-establish the rig's own store.
			var pinged bool
			for _, call := range calls {
				if call == "ping" {
					pinged = true
				}
			}
			if !pinged {
				t.Fatalf("rig recover never pinged the scope; bd calls %q\n%s", calls, out)
			}
		})
	}
}

// TestProviderScriptRecoverStillCyclesTheCityScopesOwnProxy is the control for
// the guard above: the city scope owns the shared root, so its own recover must
// keep retiring it. A guard that silenced the city's recover too would leave
// nothing able to cycle a wedged shared proxy.
func TestProviderScriptRecoverStillCyclesTheCityScopesOwnProxy(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProxiedSidecar(t, city, filepath.Join(city, ".beads", "dolt"))

	bin, logPath := writeRecordingBd(t, city)
	out, err := runProviderOwnedScriptOp(t, providerOwnedScriptEnv(city, city, bin), "recover")
	if err != nil {
		t.Fatalf("recover on the city: %v\n%s", err, out)
	}
	var stopped bool
	for _, call := range bdCalls(t, logPath) {
		if strings.HasPrefix(call, "dolt stop") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("city recover did not retire its own proxy; bd calls %q\n%s", bdCalls(t, logPath), out)
	}
}

// bd honors BEADS_DIR only when that directory already holds a project file;
// otherwise it walks up from the CWD and binds whatever bd workspace sits above
// the scope. Provider-owned init must therefore hand bd a scope that already
// carries one, and must not leave that anchor behind when bd init fails, since
// a stray config.yaml reads as a persisted beads identity to gc.
func TestGcBeadsBdProviderOwnedInitAnchorsBeadsDirBeforeBdInit(t *testing.T) {
	const userConfig = "dolt.auto-start: true\n"
	for _, tt := range []struct {
		name          string
		transport     string
		seed          map[string]string // .beads files present before the op
		bdExit        int
		bdWritesMeta  bool   // bd writes metadata.json before exiting
		wantSeen      string // what bd must see at invocation
		wantAfter     bool   // config.yaml present after the op
		wantAfterStr  string
		wantAfterPerm os.FileMode // 0 = not checked
	}{
		{name: "proxied fresh scope", transport: "proxied", wantSeen: "config.yaml=empty", wantAfter: true, wantAfterPerm: 0o600},
		{name: "direct fresh scope", transport: "direct", wantSeen: "config.yaml=empty", wantAfter: true, wantAfterPerm: 0o600},
		{name: "failed init removes the anchor", transport: "proxied", bdExit: 1, wantSeen: "config.yaml=empty"},
		{
			name: "init that wrote metadata then failed keeps the anchor", transport: "proxied", bdExit: 1, bdWritesMeta: true,
			wantSeen: "config.yaml=empty", wantAfter: true,
		},
		{
			name: "retry after a leftover empty anchor", transport: "proxied", seed: map[string]string{"config.yaml": ""},
			wantSeen: "config.yaml=empty", wantAfter: true,
		},
		{
			name: "failed retry leaves a leftover anchor it did not create", transport: "direct", seed: map[string]string{"config.yaml": ""}, bdExit: 1,
			wantSeen: "config.yaml=empty", wantAfter: true,
		},
		{
			name: "existing db file already anchors the scope", transport: "direct", seed: map[string]string{"beads.db": ""},
			wantSeen: "config.yaml=missing",
		},
		{
			name: "vc.db and backups do not count as project files", transport: "proxied",
			seed:     map[string]string{"vc.db": "", "issues.backup.db": ""},
			wantSeen: "config.yaml=empty", wantAfter: true, wantAfterPerm: 0o600,
		},
		{
			name: "existing config is left alone", transport: "proxied", seed: map[string]string{"config.yaml": userConfig},
			wantSeen: "config.yaml=present", wantAfter: true, wantAfterStr: userConfig,
		},
		{
			name: "existing config survives a failed init", transport: "direct", seed: map[string]string{"config.yaml": userConfig}, bdExit: 1,
			wantSeen: "config.yaml=present", wantAfter: true, wantAfterStr: userConfig,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			scopeDir := filepath.Join(cityDir, "rigs", "anchored")
			beadsDir := filepath.Join(scopeDir, ".beads")
			if err := os.MkdirAll(scopeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(beadsDir, "config.yaml")
			for name, content := range tt.seed {
				if err := os.MkdirAll(beadsDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(beadsDir, name), []byte(content), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(t.TempDir(), "bd.log")
			bdPath := filepath.Join(t.TempDir(), "bd")
			fakeBD := "#!/bin/sh\n" +
				"if [ ! -f \"$BEADS_DIR/config.yaml\" ]; then s=missing; elif [ -s \"$BEADS_DIR/config.yaml\" ]; then s=present; else s=empty; fi\n" +
				"printf 'config.yaml=%s\\n' \"$s\" >> " + strconv.Quote(logPath) + "\n"
			if tt.bdWritesMeta {
				fakeBD += "printf '{}\\n' > \"$BEADS_DIR/metadata.json\"\n"
			}
			fakeBD += "exit " + strconv.Itoa(tt.bdExit) + "\n"
			if err := os.WriteFile(bdPath, []byte(fakeBD), 0o755); err != nil { //nolint:gosec // fixture must be executable
				t.Fatal(err)
			}
			env := sanitizedBaseEnv(
				"GC_CITY_PATH="+cityDir,
				"BEADS_DIR="+beadsDir,
				"BD_BIN="+bdPath,
				"GC_BEADS_PROVIDER_OWNED=1",
				"GC_BEADS_TRANSPORT="+tt.transport,
				"GC_BEADS_TARGET=local",
				"GC_BEADS_PROXIED_IDLE_TIMEOUT=0",
			)
			out, err := runProviderOwnedScriptOp(t, env, "init", scopeDir, "anc", "hq")
			if tt.bdExit == 0 && err != nil {
				t.Fatalf("gc-beads-bd init: %v\n%s", err, out)
			}
			if tt.bdExit != 0 && err == nil {
				t.Fatalf("gc-beads-bd init succeeded although bd init failed:\n%s", out)
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(data)); got != tt.wantSeen {
				t.Fatalf("bd init saw %q, want %q", got, tt.wantSeen)
			}
			after, err := os.ReadFile(configPath)
			if !tt.wantAfter {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("config.yaml after init = %q (err %v), want absent", after, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("config.yaml after init: %v", err)
			}
			if string(after) != tt.wantAfterStr {
				t.Fatalf("config.yaml after init = %q, want %q", after, tt.wantAfterStr)
			}
			if tt.wantAfterPerm != 0 {
				info, err := os.Stat(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != tt.wantAfterPerm {
					t.Fatalf("anchor mode = %v, want %v", got, tt.wantAfterPerm)
				}
			}
		})
	}
}

// A city (or rig) created anywhere below another bd workspace — inside a repo
// that uses beads, or under a home directory holding ~/.beads — must get its
// own store. Before the anchor, bd v1.3.0 ignored the scope's still-empty
// BEADS_DIR, walked up to the ancestor, and `bd init --init-if-missing
// --database hq` aborted with `workspace already initialized as database
// "ancestor"`, failing every fresh `gc init` there (v1.4.2 regression).
func TestGcBeadsBdProviderOwnedRealInitIgnoresAncestorBeadsWorkspace(t *testing.T) {
	skipSlowCmdGCTest(t, "runs the real bd provider init; run make test-cmd-gc-process for full coverage")
	bdPath := waitTestRealBDPath(t)
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not installed")
	}
	for _, transport := range []string{"direct", "proxied"} {
		t.Run(transport, func(t *testing.T) {
			parent := t.TempDir()
			home := filepath.Join(t.TempDir(), "home")
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := testutil.SeedDoltGlobalConfig(home); err != nil {
				t.Fatal(err)
			}
			testutil.RunGit(t, parent, "init", "-q")
			// The ancestor workspace, in the on-disk shape of an ordinary
			// embedded bd project (the pinned test bd is built without the
			// embedded engine, so it cannot create one itself). bd's
			// already-initialized probe keys on exactly these two artifacts.
			ancestorBeads := filepath.Join(parent, ".beads")
			if err := os.MkdirAll(filepath.Join(ancestorBeads, "embeddeddolt", "ancestor", ".dolt"), 0o700); err != nil {
				t.Fatal(err)
			}
			ancestorMetadata := []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"ancestor"}` + "\n")
			if err := os.WriteFile(filepath.Join(ancestorBeads, "metadata.json"), ancestorMetadata, 0o600); err != nil {
				t.Fatal(err)
			}

			dir := filepath.Join(parent, "city")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// Every HTTPS request the init makes goes to a normally unroutable
			// proxy. The proxied init probes `dolt version`, whose update check is
			// a network call unless the dolt config seeded above disables it; on a
			// healthy network that call returns in 0.2 s and hides a missing
			// config, so on most hosts the slow network is reproduced instead of
			// waited for.
			env := sanitizedBaseEnv(append([]string{"HOME=" + home, "DOLT_ROOT_PATH=" + home, "GC_CITY_PATH=" + dir, "BEADS_DIR=" + filepath.Join(dir, ".beads"), "BD_BIN=" + bdPath, "GC_BEADS_PROVIDER_OWNED=1", "GC_BEADS_TRANSPORT=" + transport, "GC_BEADS_TARGET=local", "GC_BEADS_PROXIED_IDLE_TIMEOUT=0"}, testutil.UnroutableHTTPSProxyEnv()...)...)
			t.Cleanup(func() {
				_, _ = runProviderOwnedScriptOp(t, env, "stop")
				for _, root := range []string{dir, parent} {
					if leaked := providerOwnedDoltPIDsUnderRoot(t, root); len(leaked) != 0 {
						if errs := reapDoltLeakPIDs(leaked); len(errs) != 0 {
							t.Errorf("reap processes under %s: %v", root, errs)
						}
					}
				}
			})
			if out, err := runProviderOwnedScriptOp(t, env, "init", dir, "hq", "hq"); err != nil {
				t.Fatalf("gc-beads-bd init below an ancestor bd workspace: %v\n%s\nlogs under the test dirs:\n%s", err, out, providerOwnedLogTails(parent, home))
			}
			meta, ok, err := contract.LoadMetadataState(fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"))
			if err != nil || !ok {
				t.Fatalf("scope metadata after init: ok=%v err=%v", ok, err)
			}
			if meta.DoltDatabase != "hq" {
				t.Fatalf("scope dolt_database = %q, want hq (bound to the ancestor?)", meta.DoltDatabase)
			}
			wantMode := "server"
			if transport == "proxied" {
				wantMode = "proxied-server"
			}
			if meta.DoltMode != wantMode {
				t.Fatalf("scope dolt_mode = %q, want %q", meta.DoltMode, wantMode)
			}
			if after, err := os.ReadFile(filepath.Join(ancestorBeads, "metadata.json")); err != nil || !bytes.Equal(after, ancestorMetadata) {
				t.Fatalf("ancestor metadata changed by scope init:\nbefore: %s\nafter:  %s (err %v)", ancestorMetadata, after, err)
			}
			if out, err := runProviderOwnedScriptOp(t, env, "health"); err != nil {
				t.Fatalf("gc-beads-bd health after init: %v\n%s\nlogs under the test dirs:\n%s", err, out, providerOwnedLogTails(parent, home))
			}
		})
	}
}

// providerOwnedDoltPIDsUnderRoot lists live bd proxy / Dolt server processes
// whose command line names root. It reads /proc directly so this file keeps a
// single subprocess call site (see runProviderOwnedScriptOp).
func providerOwnedDoltPIDsUnderRoot(t *testing.T, root string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read process table: %v", err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue // exited between the listing and the read
		}
		args := string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '}))
		if !strings.Contains(args, root) {
			continue
		}
		if strings.Contains(args, "db-proxy-child") || strings.Contains(args, "sql-server") {
			pids = append(pids, pid)
		}
	}
	return pids
}

// providerOwnedLogTailBytes bounds how much of each log providerOwnedLogTails
// keeps, so a failure message stays readable.
const providerOwnedLogTailBytes = 8 << 10

// providerOwnedLogTails renders the last providerOwnedLogTailBytes of every
// *.log file under roots, for a failure message. A failing init has to carry
// its Dolt server log in the message: t.TempDir removes the log with the
// directory, so nothing is left to read once the test has failed. A file
// reachable from two roots is shown once, and one that cannot be read is
// named with its error.
func providerOwnedLogTails(roots ...string) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".log") || seen[path] {
				return nil // an unreadable directory or a missing root has no log to show
			}
			seen[path] = true
			tail, err := readFileTail(path, int64(providerOwnedLogTailBytes))
			if err != nil {
				fmt.Fprintf(&b, "--- %s: %v\n", path, err)
				return nil
			}
			fmt.Fprintf(&b, "--- %s (last %d bytes)\n%s\n", path, len(tail), tail)
			return nil
		})
	}
	if b.Len() == 0 {
		return "no log files under " + strings.Join(roots, ", ")
	}
	return b.String()
}

// readFileTail returns at most the last n bytes of the file at path.
func readFileTail(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, min(n, info.Size()))
	if _, err := f.ReadAt(buf, info.Size()-int64(len(buf))); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// The real-init test's Dolt server has died mid-init on a remote worker, and
// t.TempDir removed its log before anyone read it (ga-11kci7u). The failure
// message must carry the log tails instead.
func TestProviderOwnedLogTailsKeepsTheEndOfEveryLog(t *testing.T) {
	root := t.TempDir()
	logDir := filepath.Join(root, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "HEAD-MARKER\n" + strings.Repeat("filler line\n", 4*providerOwnedLogTailBytes/len("filler line\n")) + "TAIL-MARKER panic: server died\n"
	if err := os.WriteFile(filepath.Join(logDir, "dolt.log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("NOT-A-LOG"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := providerOwnedLogTails(filepath.Join(root, "missing"), root)

	if !strings.Contains(got, filepath.Join(logDir, "dolt.log")) {
		t.Errorf("tails do not name the log file:\n%s", got)
	}
	if !strings.Contains(got, "TAIL-MARKER panic: server died") {
		t.Errorf("tails dropped the end of the log:\n%s", got)
	}
	if strings.Contains(got, "HEAD-MARKER") {
		t.Errorf("tails kept the head of a log larger than %d bytes", providerOwnedLogTailBytes)
	}
	if strings.Contains(got, "NOT-A-LOG") {
		t.Errorf("tails included a file that is not a log:\n%s", got)
	}
}

func TestProviderOwnedLogTailsSaysWhenThereIsNoLog(t *testing.T) {
	root := t.TempDir()

	got := providerOwnedLogTails(root)

	if !strings.Contains(got, "no log files") || !strings.Contains(got, root) {
		t.Errorf("want a message naming the roots searched when no log exists, got %q", got)
	}
}
