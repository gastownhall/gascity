package runtimecontract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// screenReferencePath is the repo's reference session script,
// contrib/session-scripts/gc-session-screen: under Bazel in the test's
// runfiles (//contrib:all_files), otherwise beside this source file.
func screenReferencePath(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "contrib", "session-scripts", "gc-session-screen")
	}
	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "contrib", "session-scripts", "gc-session-screen"))
}

// screenReferenceState allocates the conformance run's private state root.
func screenReferenceState(t *testing.T) string {
	t.Helper()
	state, err := os.MkdirTemp("", "gcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	return state
}

// TestScreenReferenceStateFitsSocketPath checks the real-screen fixture's
// socket budget and private lifetime, even when ambient TMPDIR is long.
func TestScreenReferenceStateFitsSocketPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		long bool
	}{{name: "short TMPDIR"}, {name: "macOS TMPDIR", long: true}} {
		t.Run(tc.name, func(t *testing.T) {
			ambient := tmpDirOfLength(t, 12)
			if tc.long {
				ambient = filepath.Join(ambient, "var", "folders", strings.Repeat("m", 30), "T")
				if err := os.MkdirAll(ambient, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TMPDIR", ambient)
			var roots []string
			t.Run("owner", func(t *testing.T) {
				// Use the kit's generated prefix and a catalog-sized counter,
				// not a short stand-in that hides the macOS path overflow.
				opts := Options{WorkDir: ambient}
				opts.applyDefaults()
				r := runner{opts: opts, counter: len(catalog)}
				name := r.nextName()
				for range 2 {
					state := screenReferenceState(t)
					roots = append(roots, state)
					if !filepath.IsAbs(state) {
						t.Errorf("state root %q is not absolute", state)
					}
					info, err := os.Lstat(state)
					if err != nil {
						t.Fatal(err)
					}
					if !info.IsDir() || info.Mode().Perm() != 0o700 {
						t.Errorf("state root mode = %v, want private directory 0700", info.Mode())
					}
					// Match gc-session-screen's socket-budget calculation: an
					// eight-hex city key, seven-digit pid and the session name.
					socket := filepath.Join(state, "01234567", "1234567."+name)
					if len(socket) > 107 {
						t.Errorf("screen socket path is %d bytes with TMPDIR %q, want at most 107: %s", len(socket), ambient, socket)
					}
				}
				if roots[0] == roots[1] {
					t.Errorf("independent conformance runs share state root %q", roots[0])
				}
			})
			for _, state := range roots {
				if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Errorf("state root %q after owner cleanup: %v, want not exist", state, err)
				}
			}
			if _, err := os.Stat(ambient); err != nil {
				t.Errorf("state cleanup removed ambient TMPDIR: %v", err)
			}
		})
	}
}

// TestScreenReferenceIsConformant runs the full catalog against
// gc-session-screen in a private state directory (its screen sockets live
// there too): it must pass every requirement it does not skip, read
// v2-ready, and skip only what it does not declare (exec, provision,
// report-activity's last_activity). Skipped where screen, jq, flock or
// cksum is missing: the RBE worker image has no screen, so in CI it runs
// only on a lane that installs it.
func TestScreenReferenceIsConformant(t *testing.T) {
	for _, tool := range []string{"screen", "jq", "flock", "cksum", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	script := screenReferencePath(t)
	if _, err := os.Stat(script); err != nil {
		t.Skipf("reference script not available: %v", err)
	}
	// A short state directory: screen's socket paths must fit 107 bytes.
	state := screenReferenceState(t)
	t.Setenv("GC_EXEC_STATE_DIR", state)

	report, err := Run(context.Background(), script, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.V2Ready || report.Protocol.Locality != "local" {
		t.Errorf("handshake: v2-ready %v (missing %v), locality %q; want v2-ready and local", report.V2Ready, report.V2Missing, report.Protocol.Locality)
	}
	wantSkip := map[Code]bool{ReqConnectionExec: true, ReqProvisionBoxWithoutAgent: true, ReqObserveInventoryHasActivity: true}
	for _, res := range report.Results {
		switch {
		case res.Status == StatusFail:
			t.Errorf("%s failed: %s", res.Code, res.Detail)
		case res.Status == StatusSkip && !wantSkip[res.Code]:
			t.Errorf("%s skipped: %s", res.Code, res.Detail)
		}
	}
}
