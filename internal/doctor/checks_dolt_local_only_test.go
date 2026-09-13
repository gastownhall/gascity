package doctor

// RED tests for internal/doctor/checks_dolt_local_only.go (ga-673qo6.1).
//
// These tests define the expected behavior of DoltLocalOnlyRemoteCheck and
// must fail to compile until the builder implements:
//   - internal/doctor/checks_dolt_local_only.go with DoltLocalOnlyRemoteCheck,
//     NewDoltLocalOnlyRemoteCheck, and the injectable removeRemote field.
//
// Re-entry paths covered (both vectors from ga-d457b):
//   Path 1 (normal store open remote sync): bd auto-push re-derives the git
//     origin remote and re-adds it to dolt_remotes on every bd write, even
//     with dolt.auto-push:false set (blocks PUSH, not re-registration). Any
//     bd write causes origin to reappear in repo_state.json.
//   Path 2 (bd init/reattach wiring): bd init or gc rig reattach reads git
//     remotes from the working tree and wires them into Dolt's dolt_remotes
//     table, re-adding origin after a one-time CALL DOLT_REMOTE remove.
//
// Both paths leave the same artifact — a remote entry in
// <doltDataDir>/<db>/.dolt/repo_state.json — so one doctor check detects both.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeRigConfigYAML writes .beads/config.yaml with the given content.
func writeRigConfigYAML(t *testing.T, rigPath, content string) {
	t.Helper()
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("create .beads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// writeRepoStateWithRemotes writes a minimal repo_state.json under
// <doltDataDir>/<dbName>/.dolt/ with the given remotes map.
// Each entry in remotes maps remote name → URL.
func writeRepoStateWithRemotes(t *testing.T, doltDataDir, dbName string, remotes map[string]string) {
	t.Helper()
	doltDir := filepath.Join(doltDataDir, dbName, ".dolt")
	if err := os.MkdirAll(doltDir, 0o700); err != nil {
		t.Fatalf("create .dolt dir: %v", err)
	}
	remotesMap := make(map[string]any, len(remotes))
	for name, url := range remotes {
		remotesMap[name] = map[string]any{
			"name":        name,
			"url":         url,
			"fetch_specs": []string{"refs/heads/*:refs/remotes/" + name + "/*"},
			"params":      map[string]any{},
		}
	}
	state := map[string]any{
		"head":     "refs/heads/main",
		"remotes":  remotesMap,
		"backups":  map[string]any{},
		"branches": map[string]any{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal repo_state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(doltDir, "repo_state.json"), data, 0o600); err != nil {
		t.Fatalf("write repo_state.json: %v", err)
	}
}

const localOnlyConfigYAML = `issue_prefix: ga
dolt.local-only: true
dolt.auto-push: false
no-push: true
export.auto: false
`

const localOnlyFalseConfigYAML = `issue_prefix: ga
dolt.local-only: false
`

// TestDoltLocalOnlyRemoteCheck_Name verifies the canonical check identifier.
func TestDoltLocalOnlyRemoteCheck_Name(t *testing.T) {
	rig := config.Rig{Name: "gascity", Path: t.TempDir()}
	c := NewDoltLocalOnlyRemoteCheck(t.TempDir(), rig, filepath.Join(t.TempDir(), ".beads", "dolt"))
	want := "rig:gascity:dolt-local-only-remote"
	if got := c.Name(); got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// TestDoltLocalOnlyRemoteCheck_NoConfig_OK verifies that a rig with no
// .beads/config.yaml is treated as StatusOK — local-only is not configured so
// any remote is legitimate.
func TestDoltLocalOnlyRemoteCheck_NoConfig_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin": "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK (local-only not configured)", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_LocalOnlyFalse_OK verifies that a rig with
// dolt.local-only: false (remote sync intentional) does not warn even when
// an off-box remote is present.
func TestDoltLocalOnlyRemoteCheck_LocalOnlyFalse_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyFalseConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin": "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK (remote sync is legitimate when not local-only)", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_LocalOnlyTrue_NoRepoState_OK verifies that
// dolt.local-only: true with no repo_state.json (fresh db) is StatusOK.
func TestDoltLocalOnlyRemoteCheck_LocalOnlyTrue_NoRepoState_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	// No repo_state.json written — database has no remotes.

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK (no repo_state → no remotes)", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_LocalOnlyTrue_EmptyRemotes_OK verifies that
// dolt.local-only: true with an empty remotes map is StatusOK. This is the
// expected state after a successful one-time CALL DOLT_REMOTE remove.
func TestDoltLocalOnlyRemoteCheck_LocalOnlyTrue_EmptyRemotes_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", nil) // no remotes

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK (empty remotes is the desired state)", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_LocalOnlyTrue_LocalBackupOnly_OK verifies that
// a local file:// backup remote is not treated as an off-box sync remote.
// The <db>-backup convention is used by mol-dog-backup for local snapshots;
// it is neither an off-box sync vector nor a violation of local-only policy.
func TestDoltLocalOnlyRemoteCheck_LocalOnlyTrue_LocalBackupOnly_OK(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"testdb-backup": "file://" + filepath.Join(cityPath, ".dolt-backup", "testdb"),
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want StatusOK (local file:// backup remote is not an off-box sync vector)", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_StoreWriteReEntry_GitPlusHttps_Warns covers
// Path 1 (normal store open remote sync): bd re-derives origin from the git
// working tree and re-adds it on every bd write (git+https URL scheme).
// After the one-time CALL DOLT_REMOTE remove, the next bd update recreates:
//
//	origin | git+https://github.com/gastownhall/gascity.git
//
// The guard must detect this and surface it as StatusWarning.
func TestDoltLocalOnlyRemoteCheck_StoreWriteReEntry_GitPlusHttps_Warns(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin": "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "origin") {
		t.Errorf("Message should name the offending remote %q: %s", "origin", r.Message)
	}
	if !strings.Contains(r.FixHint, "bd dolt remote remove origin") {
		t.Errorf("FixHint should contain 'bd dolt remote remove origin': %s", r.FixHint)
	}
}

// TestDoltLocalOnlyRemoteCheck_InitWiringReEntry_HttpsRemote_Warns covers
// Path 2 (bd init/reattach wiring): bd init reads git config and wires the
// git remote as a Dolt remote (upstream cmd/bd/sync_git.go AddRemote site).
// After a one-time cleanup, the next gc start / gc rig reattach re-adds:
//
//	origin | https://github.com/gastownhall/gascity.git
//
// The guard must detect the plain https URL and surface it as StatusWarning.
func TestDoltLocalOnlyRemoteCheck_InitWiringReEntry_HttpsRemote_Warns(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin": "https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "origin") {
		t.Errorf("Message should name the offending remote %q: %s", "origin", r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_SshRemote_Warns verifies that an SSH remote
// URL is also treated as an off-box sync vector under local-only policy.
func TestDoltLocalOnlyRemoteCheck_SshRemote_Warns(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"upstream": "ssh://git@github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning for ssh:// remote; message=%s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "upstream") {
		t.Errorf("Message should name the offending remote 'upstream': %s", r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_MultipleOffBoxRemotes_Warns verifies that
// multiple off-box remotes are all reported (both URLs reappear via V1/V2).
func TestDoltLocalOnlyRemoteCheck_MultipleOffBoxRemotes_Warns(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin":   "git+https://github.com/gastownhall/gascity.git",
		"upstream": "https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning for multiple off-box remotes; message=%s", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_MixedRemotes_Warns verifies that a mix of
// local backup and off-box sync remotes still triggers StatusWarning (the
// off-box remote is the violation; the local backup is fine).
func TestDoltLocalOnlyRemoteCheck_MixedRemotes_Warns(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"testdb-backup": "file://" + filepath.Join(cityPath, ".dolt-backup", "testdb"),
		"origin":        "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning (off-box remote present alongside local backup); message=%s",
			r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "origin") {
		t.Errorf("Message should name the off-box remote 'origin', not the backup: %s", r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_CanFix_LocalOnly verifies CanFix returns true
// when the rig is configured for local-only. The explicit flag makes
// auto-strip safe (unambiguous operator intent, unlike warn-only).
func TestDoltLocalOnlyRemoteCheck_CanFix_LocalOnly(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, filepath.Join(cityPath, ".beads", "dolt"))
	if !c.CanFix() {
		t.Fatal("CanFix() = false, want true when dolt.local-only: true (explicit flag makes auto-strip safe)")
	}
}

// TestDoltLocalOnlyRemoteCheck_CanFix_NotConfigured verifies CanFix returns
// false when local-only is not configured (remote sync may be intentional).
func TestDoltLocalOnlyRemoteCheck_CanFix_NotConfigured(t *testing.T) {
	rig := config.Rig{Name: "testrig", Path: t.TempDir()}
	c := NewDoltLocalOnlyRemoteCheck(t.TempDir(), rig, filepath.Join(t.TempDir(), ".beads", "dolt"))
	if c.CanFix() {
		t.Fatal("CanFix() = true, want false when dolt.local-only not configured")
	}
}

// TestDoltLocalOnlyRemoteCheck_Fix_CallsRemoveForOffBoxRemotes verifies that
// Fix() calls the injectable remove function for each off-box sync remote, and
// does not call it for local file:// backup remotes.
func TestDoltLocalOnlyRemoteCheck_Fix_CallsRemoveForOffBoxRemotes(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin":        "git+https://github.com/gastownhall/gascity.git",
		"testdb-backup": "file://" + filepath.Join(cityPath, ".dolt-backup", "testdb"),
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)

	var removeCalls []string
	c.removeRemote = func(_ string, remoteName string) error {
		removeCalls = append(removeCalls, remoteName)
		return nil
	}

	if err := c.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}

	if len(removeCalls) != 1 {
		t.Fatalf("removeRemote called %d times, want 1; calls=%v", len(removeCalls), removeCalls)
	}
	if removeCalls[0] != "origin" {
		t.Errorf("removeRemote called with %q, want %q", removeCalls[0], "origin")
	}
}

// TestDoltLocalOnlyRemoteCheck_Fix_PropagatesRemoveError verifies that Fix()
// surfaces errors from the remove function rather than silently swallowing them.
func TestDoltLocalOnlyRemoteCheck_Fix_PropagatesRemoveError(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "testdb")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "testdb", map[string]string{
		"origin": "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	removeErr := errors.New("bd dolt remote remove: server not reachable")
	c.removeRemote = func(_ string, _ string) error {
		return removeErr
	}

	err := c.Fix(&CheckContext{CityPath: cityPath})
	if err == nil {
		t.Fatal("Fix() should propagate error from removeRemote")
	}
	if !strings.Contains(err.Error(), "server not reachable") {
		t.Errorf("error should contain remove error detail, got: %v", err)
	}
}

// TestDoltLocalOnlyRemoteCheck_RelativeRigPath verifies that a relative rig
// path is resolved against cityPath when reading config.yaml.
func TestDoltLocalOnlyRemoteCheck_RelativeRigPath(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rigs", "gascity")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "ga")
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "ga", map[string]string{
		"origin": "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "gascity", Path: filepath.Join("rigs", "gascity")}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning with relative rig path; message=%s", r.Status, r.Message)
	}
}

// TestDoltLocalOnlyRemoteCheck_DBNameFromMetadata verifies that the check
// resolves the Dolt database name from .beads/metadata.json when present,
// so the repo_state.json lookup uses the correct database directory.
func TestDoltLocalOnlyRemoteCheck_DBNameFromMetadata(t *testing.T) {
	cityPath := t.TempDir()
	doltDataDir := filepath.Join(cityPath, ".beads", "dolt")
	rigPath := filepath.Join(cityPath, "rig")
	if err := os.MkdirAll(rigPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigMetadata(t, rigPath, "customdb") // pinned dolt_database != rig.Name
	writeRigConfigYAML(t, rigPath, localOnlyConfigYAML)
	writeRepoStateWithRemotes(t, doltDataDir, "customdb", map[string]string{
		"origin": "git+https://github.com/gastownhall/gascity.git",
	})

	rig := config.Rig{Name: "testrig", Path: rigPath}
	c := NewDoltLocalOnlyRemoteCheck(cityPath, rig, doltDataDir)
	r := c.Run(&CheckContext{CityPath: cityPath})

	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want StatusWarning (resolved db=customdb from metadata); message=%s",
			r.Status, r.Message)
	}
}
