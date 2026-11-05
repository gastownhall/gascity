package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
)

// clearProxiedRootEnv removes every environment arm bd consults before the
// scope's own .beads/dolt, and points HOME and XDG_CONFIG_HOME at an empty
// directory so no user-level bd config applies either. A test sees only the
// layout it writes.
func clearProxiedRootEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		proxyendpoint.RootPathEnv,
		proxyendpoint.DoltDataDirEnv,
		proxyendpoint.SharedServerModeEnv,
		proxyendpoint.SharedServerDirEnv,
		bdSharedServerConfigEnv,
	} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func newProxiedSplitStoreScope(t *testing.T) (string, string) {
	t.Helper()
	clearProxiedRootEnv(t)
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorProxiedMetadata(t, dir, "hq")
	return dir, beadsDir
}

const bdSharedServerOnConfig = "dolt:\n  shared-server: true\n"

// writeScopeBdConfig writes the scope's own .beads/config.yaml.
func writeScopeBdConfig(t *testing.T, beadsDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

// pinScopeSharedServerOff writes the dolt.shared-server: false pin exactly as
// gc writes it into each proxied scope it owns.
func pinScopeSharedServerOff(t *testing.T, beadsDir string) {
	t.Helper()
	if _, _, err := contract.EnsureSharedServerDisabled(fsys.OSFS{}, filepath.Join(beadsDir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

// writeUserLevelBdConfig writes ~/.config/bd/config.yaml under the HOME
// clearProxiedRootEnv isolated.
func writeUserLevelBdConfig(t *testing.T, body string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "bd", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

// proxiedLedgerElsewhereCases are proxied-server shapes whose ledger is not,
// or is not known to be, the scope's own .beads/dolt: bd roots the proxy
// elsewhere, the root cannot be resolved, the data lives on an external
// upstream, shared-server mode may be on, or a legacy absolute `database`
// names the data directory. elsewhere is an absolute directory outside the
// scope.
var proxiedLedgerElsewhereCases = []struct {
	name  string
	setup func(t *testing.T, dir, beadsDir, elsewhere string)
}{
	{"sidecar root_path", func(t *testing.T, dir, _, elsewhere string) {
		writeDoctorSidecar(t, dir, `{"root_path":"`+elsewhere+`"}`)
	}},
	{"root path env", func(t *testing.T, _, _, elsewhere string) {
		t.Setenv(proxyendpoint.RootPathEnv, elsewhere)
	}},
	{"dolt data dir env", func(t *testing.T, _, _, elsewhere string) {
		t.Setenv(proxyendpoint.DoltDataDirEnv, elsewhere)
	}},
	{"metadata dolt_data_dir", func(t *testing.T, _, beadsDir, elsewhere string) {
		body := `{"database":"dolt","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq","dolt_data_dir":"` + elsewhere + `"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}},
	{"malformed sidecar", func(t *testing.T, dir, _, _ string) {
		writeDoctorSidecar(t, dir, `{`)
	}},
	{"external upstream over tcp", func(t *testing.T, dir, _, _ string) {
		writeDoctorSidecar(t, dir, `{"external":{"host":"db.example.com","port":3307}}`)
	}},
	{"external upstream over a socket", func(t *testing.T, dir, _, _ string) {
		writeDoctorSidecar(t, dir, `{"external":{"socket":"/run/dolt.sock"}}`)
	}},
	{"scope config pins shared-server on", func(t *testing.T, _, beadsDir, _ string) {
		writeScopeBdConfig(t, beadsDir, bdSharedServerOnConfig)
	}},
	{"unreadable scope config", func(t *testing.T, _, beadsDir, _ string) {
		writeScopeBdConfig(t, beadsDir, "dolt: [\n")
	}},
	{"user-level config turns shared-server on, scope unpinned", func(t *testing.T, _, _, _ string) {
		writeUserLevelBdConfig(t, bdSharedServerOnConfig)
	}},
	{"BD_DOLT_SHARED_SERVER outranks the scope's off pin", func(t *testing.T, _, beadsDir, _ string) {
		pinScopeSharedServerOff(t, beadsDir)
		t.Setenv(bdSharedServerConfigEnv, "true")
	}},
	{"absolute metadata database", func(t *testing.T, _, beadsDir, elsewhere string) {
		body := `{"database":"` + elsewhere + `","backend":"dolt","dolt_mode":"proxied-server","dolt_database":"hq"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}},
}

// TestBDSplitStoreCheck_ProxiedServerActiveWarnsWhenEmbeddedStoreHasRepos is
// the layout `bd bootstrap` leaves in a healthy proxied city: the live ledger
// under .beads/dolt and a second, empty-or-not hq under .beads/embeddeddolt.
// The check must name .beads/dolt as the store bd actually serves.
func TestBDSplitStoreCheck_ProxiedServerActiveWarnsWhenEmbeddedStoreHasRepos(t *testing.T) {
	dir, beadsDir := newProxiedSplitStoreScope(t)
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))

	assertProxiedSplitStoreIdentified(t, NewBDSplitStoreCheck(dir).Run(&CheckContext{}))
}

// TestBDSplitStoreCheck_ProxiedServerSharedServerOffStaysIdentified is the
// other side of the shared-server guard: a source that keeps bd off its shared
// server keeps .beads/dolt identified even when bd's user-level config turns
// the mode on. The first case is the default gc layout, the pin gc writes into
// every proxied scope it owns.
func TestBDSplitStoreCheck_ProxiedServerSharedServerOffStaysIdentified(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, beadsDir string)
	}{
		{"gc's off pin under a user-level on", func(t *testing.T, beadsDir string) {
			pinScopeSharedServerOff(t, beadsDir)
			writeUserLevelBdConfig(t, bdSharedServerOnConfig)
		}},
		{"BD_DOLT_SHARED_SERVER=false under a user-level on", func(t *testing.T, _ string) {
			writeUserLevelBdConfig(t, bdSharedServerOnConfig)
			t.Setenv(bdSharedServerConfigEnv, "false")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, beadsDir := newProxiedSplitStoreScope(t)
			writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
			writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))
			tc.setup(t, beadsDir)

			assertProxiedSplitStoreIdentified(t, NewBDSplitStoreCheck(dir).Run(&CheckContext{}))
		})
	}
}

// assertProxiedSplitStoreIdentified checks a both-directories proxied result
// that names .beads/dolt active. bd's read-time guard never raises its
// unread-store notice for a proxied-server scope, so the hint must not promise
// it.
func assertProxiedSplitStoreIdentified(t *testing.T, r *CheckResult) {
	t.Helper()
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	want := "active .beads/dolt (metadata.json dolt_mode=proxied-server), inactive .beads/embeddeddolt contains 1 Dolt repo(s)"
	if !strings.Contains(r.Message, want) {
		t.Fatalf("message = %q, want it to contain %q", r.Message, want)
	}
	if !strings.Contains(r.FixHint, "export from the inactive store") {
		t.Fatalf("fix hint = %q, want identified-store guidance", r.FixHint)
	}
	if strings.Contains(r.FixHint, "current or intended active store") {
		t.Fatalf("fix hint = %q, want no neutral unknown-store guidance", r.FixHint)
	}
	if strings.Contains(r.FixHint, beads.AllowUnreadStoreReadEnvVar) {
		t.Fatalf("fix hint = %q, names a read-time notice the guard never raises for a proxied scope", r.FixHint)
	}
}

// TestBDSplitStoreCheck_ProxiedServerRelocatedRootStaysNeutral guards the
// fail-closed side of the proxied arm: when the ledger is not, or is not known
// to be, under the scope's own .beads/dolt, neither local directory is known to
// be live.
func TestBDSplitStoreCheck_ProxiedServerRelocatedRootStaysNeutral(t *testing.T) {
	for _, tc := range proxiedLedgerElsewhereCases {
		t.Run(tc.name, func(t *testing.T) {
			dir, beadsDir := newProxiedSplitStoreScope(t)
			writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
			writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))
			tc.setup(t, dir, beadsDir, t.TempDir())

			r := NewBDSplitStoreCheck(dir).Run(&CheckContext{})
			if r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
			}
			if !strings.Contains(r.Message, "no active local store was identified") {
				t.Fatalf("message = %q, want the unknown-store message", r.Message)
			}
			if !strings.Contains(r.FixHint, "current or intended active store") {
				t.Fatalf("fix hint = %q, want neutral active-store guidance", r.FixHint)
			}
		})
	}
}

// TestBDSplitStoreCheck_ProxiedServerUnreadShapeNeverClaimsALedgerElsewhere is
// the unread-only shape of the cases above: an hq under .beads/embeddeddolt
// and no .beads/dolt. Naming .beads/dolt active there would tell an operator
// whose ledger is elsewhere that their live store is that empty directory.
func TestBDSplitStoreCheck_ProxiedServerUnreadShapeNeverClaimsALedgerElsewhere(t *testing.T) {
	for _, tc := range proxiedLedgerElsewhereCases {
		t.Run(tc.name, func(t *testing.T) {
			dir, beadsDir := newProxiedSplitStoreScope(t)
			writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))
			tc.setup(t, dir, beadsDir, t.TempDir())

			r := NewBDSplitStoreCheck(dir).Run(&CheckContext{})
			for _, line := range append([]string{r.Message}, r.Details...) {
				if strings.Contains(line, "active .beads/dolt") || strings.Contains(line, "active store: .beads/dolt") {
					t.Fatalf("result names .beads/dolt active (%q); msg = %q details = %q", line, r.Message, r.Details)
				}
			}
		})
	}
}

// TestBDSplitStoreCheck_ProxiedServerFreshInitLayoutStaysOK is what `gc init`
// creates: one proxied ledger under .beads/dolt and nothing beside it.
func TestBDSplitStoreCheck_ProxiedServerFreshInitLayoutStaysOK(t *testing.T) {
	dir, beadsDir := newProxiedSplitStoreScope(t)
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))

	r := NewBDSplitStoreCheck(dir).Run(&CheckContext{})
	if r.Status != StatusOK || r.Message != "no legacy split store detected" {
		t.Fatalf("status = %d msg = %q, want OK no legacy split store detected", r.Status, r.Message)
	}
}

// TestBDSplitStoreCheck_ProxiedServerNonDoltBackendStaysNeutral pins that the
// non-Dolt guard runs before the proxied arm.
func TestBDSplitStoreCheck_ProxiedServerNonDoltBackendStaysNeutral(t *testing.T) {
	dir, beadsDir := newProxiedSplitStoreScope(t)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"database":"sqlite","backend":"sqlite","dolt_mode":"proxied-server"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))

	r := NewBDSplitStoreCheck(dir).Run(&CheckContext{})
	if !strings.Contains(r.Message, "no active local store was identified") {
		t.Fatalf("message = %q, want the unknown-store message", r.Message)
	}
}

// TestBDSplitStoreCheck_ProxiedServerWarnsWhenOnlyTheUnreadEmbeddedStoreExists
// is the proxied shape of the false all-clear: an hq under .beads/embeddeddolt
// and no .beads/dolt yet. The active store is still .beads/dolt, the directory
// bd roots the proxy at, even before that directory exists — including when the
// scope is reached through a symlink.
func TestBDSplitStoreCheck_ProxiedServerWarnsWhenOnlyTheUnreadEmbeddedStoreExists(t *testing.T) {
	want := "unread bead database beside the active store: active .beads/dolt (metadata.json dolt_mode=proxied-server), unread .beads/embeddeddolt contains 1 Dolt repo(s)"
	for _, viaSymlink := range []bool{false, true} {
		name := "direct"
		if viaSymlink {
			name = "via symlink"
		}
		t.Run(name, func(t *testing.T) {
			dir, beadsDir := newProxiedSplitStoreScope(t)
			writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))
			scope := dir
			if viaSymlink {
				scope = filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(dir, scope); err != nil {
					t.Fatal(err)
				}
			}

			r := NewBDSplitStoreCheck(scope).Run(&CheckContext{})
			if r.Status != StatusWarning {
				t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
			}
			if r.Message != want {
				t.Fatalf("message = %q, want %q", r.Message, want)
			}
			if !strings.Contains(r.FixHint, "export from the inactive store") {
				t.Fatalf("fix hint = %q, want identified-store guidance", r.FixHint)
			}
			if !strings.Contains(r.FixHint, "keep both directories until reconciled") {
				t.Fatalf("fix hint = %q, want the keep-both recovery", r.FixHint)
			}
			if strings.Contains(r.FixHint, beads.AllowUnreadStoreReadEnvVar) {
				t.Fatalf("fix hint = %q, names a read-time notice the guard never raises for a proxied scope", r.FixHint)
			}
		})
	}
}
