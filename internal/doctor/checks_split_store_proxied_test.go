package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
)

// clearProxiedRootEnv removes every environment arm bd consults before the
// scope's own .beads/dolt, so a test sees only the layout it writes.
func clearProxiedRootEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		proxyendpoint.RootPathEnv,
		proxyendpoint.DoltDataDirEnv,
		proxyendpoint.SharedServerModeEnv,
		proxyendpoint.SharedServerDirEnv,
	} {
		t.Setenv(name, "")
	}
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

// TestBDSplitStoreCheck_ProxiedServerActiveWarnsWhenEmbeddedStoreHasRepos is
// the layout `bd bootstrap` leaves in a healthy proxied city: the live ledger
// under .beads/dolt and a second, empty-or-not hq under .beads/embeddeddolt.
// The check must name .beads/dolt as the store bd actually serves.
func TestBDSplitStoreCheck_ProxiedServerActiveWarnsWhenEmbeddedStoreHasRepos(t *testing.T) {
	dir, beadsDir := newProxiedSplitStoreScope(t)
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "hq"))

	r := NewBDSplitStoreCheck(dir).Run(&CheckContext{})
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
}

// TestBDSplitStoreCheck_ProxiedServerRelocatedRootStaysNeutral guards the
// fail-closed side of the proxied arm: when bd roots the proxy anywhere other
// than the scope's own .beads/dolt, neither local directory is known to be live.
func TestBDSplitStoreCheck_ProxiedServerRelocatedRootStaysNeutral(t *testing.T) {
	cases := []struct {
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
	}
	for _, tc := range cases {
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
		})
	}
}
