package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeOwnedRigScope(t *testing.T, dir, doltDatabase string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "proxied-server",
		DoltDatabase: doltDatabase,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRenamingARigInPlaceDoesNotRefuseTheCity pins the startup sequence
// startBeadsLifecycle runs: ensureFreshRigProviderOwnership re-keys the journal,
// then validateProviderScopeOwnership checks it. An operator who renames a rig
// in city.toml without moving it changed a label, not a topology, and `gc start`
// used to refuse the whole city with "scope ownership journal path drift" —
// recoverable only by hand-editing a file gc owns.
func TestRenamingARigInPlaceDoesNotRefuseTheCity(t *testing.T) {
	city := t.TempDir()
	rigDir := filepath.Join(city, "rigs", "repo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOwnedRigScope(t, city, "hq")
	writeOwnedRigScope(t, rigDir, "rp")
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(city))

	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"c\"\n\n[[rigs]]\nname = \"a\"\npath = \"rigs/repo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rigDir, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rigDir); err != nil {
		t.Fatal(err)
	}
	if key, _, owned, err := providerScopeOwnershipRecord(city, rigDir); err != nil || !owned || key != "rig:a" {
		t.Fatalf("pre-rename record = (%q, %t, %v), want rig:a", key, owned, err)
	}

	// The rename: same directory, new label.
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"c\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"rigs/repo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: rigDir}}}

	if err := ensureFreshRigProviderOwnership(city, cfg); err != nil {
		t.Fatalf("ensureFreshRigProviderOwnership after rename: %v", err)
	}
	if err := validateProviderScopeOwnership(city, cfg); err != nil {
		t.Fatalf("renamed rig refused city startup: %v", err)
	}
	key, entry, owned, err := providerScopeOwnershipRecord(city, rigDir)
	if err != nil || !owned || key != "rig:alpha" {
		t.Fatalf("post-rename record = (%q, %+v, %t, %v), want rig:alpha", key, entry, owned, err)
	}
	if entry.State != providerScopeReady {
		t.Fatalf("re-keying lost the record's state: %+v", entry)
	}
	data, err := os.ReadFile(filepath.Join(city, ".gc", "scope-ownership.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"rig:a"`) {
		t.Fatalf("stale rig key survived the rename: %s", data)
	}
}

// TestValidateProviderScopeOwnershipAcceptsARigKeyedRecordAtAConfiguredPath is
// the second half of the same rule: validation asks where a scope is, not what
// it is currently called. A rig-keyed record whose path is one of the configured
// rigs is a stale label, not drift.
func TestValidateProviderScopeOwnershipAcceptsARigKeyedRecordAtAConfiguredPath(t *testing.T) {
	city := t.TempDir()
	rigDir := filepath.Join(city, "rigs", "repo")
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal := `{"version":1,"scopes":{"rig:a":{"scope_path":"` + rigDir + `","lifecycle_owner":"provider","state":"ready"}}}`
	if err := os.WriteFile(filepath.Join(city, ".gc", "scope-ownership.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: rigDir}}}
	if err := validateProviderScopeOwnership(city, cfg); err != nil {
		t.Fatalf("validateProviderScopeOwnership = %v, want nil for a renamed rig at a configured path", err)
	}
}

// TestProviderScopeOwnershipLockWaitsForContention pins the fix for a shared
// mutex that was taken non-blocking: two legitimate writers — a CLI `gc rig add`
// and the controller's AddRig handler are not serialized against each other —
// must both get their turn, not have one fail with "journal is busy" after its
// rig's store already exists.
func TestProviderScopeOwnershipLockWaitsForContention(t *testing.T) {
	city := t.TempDir()
	held := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	var firstErr error
	go func() {
		defer wg.Done()
		firstErr = withProviderScopeOwnershipLock(city, func() error {
			close(held)
			<-release
			return nil
		})
	}()

	<-held
	second := make(chan error, 1)
	go func() {
		second <- withProviderScopeOwnershipLock(city, func() error { return nil })
	}()

	select {
	case err := <-second:
		t.Fatalf("second taker returned %v while the lock was held; it must wait", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second taker = %v, want success after the first released", err)
		}
	case <-time.After(providerScopeOwnershipLockWait + time.Second):
		t.Fatal("second taker never acquired the lock")
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("first taker = %v", firstErr)
	}
}

// TestProviderScopeOwnershipLockGivesUpWithAClearMessage keeps the wait bounded:
// a lock nobody ever releases must still produce an error an operator can act
// on rather than hanging a command forever.
func TestProviderScopeOwnershipLockGivesUpWithAClearMessage(t *testing.T) {
	city := t.TempDir()
	restore := providerScopeOwnershipLockWait
	providerScopeOwnershipLockWait = 150 * time.Millisecond
	t.Cleanup(func() { providerScopeOwnershipLockWait = restore })

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withProviderScopeOwnershipLock(city, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		<-done
	}()

	err := withProviderScopeOwnershipLock(city, func() error { return nil })
	if err == nil {
		t.Fatal("withProviderScopeOwnershipLock = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "scope ownership journal is busy") || !strings.Contains(err.Error(), "150ms") {
		t.Fatalf("error = %v, want a busy message naming the wait it gave up after", err)
	}
}

// TestRemovingAnInPlaceRenamedRigDetachesItsStaleRecord closes the other door
// on the same rename. Only the start-time attach pass re-keys a renamed rig, so
// an operator who renames in city.toml and then removes the rig never runs it:
// detaching by the configured name alone left the stale `rig:<old>` record at a
// directory city.toml no longer declares, and the next `gc start` refused the
// whole city with the very path drift the rename fix set out to remove.
func TestRemovingAnInPlaceRenamedRigDetachesItsStaleRecord(t *testing.T) {
	city := t.TempDir()
	rigDir := filepath.Join(city, "rigs", "repo")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := func(rigName string) {
		t.Helper()
		toml := "[workspace]\nname = \"c\"\n\n[[rigs]]\nname = \"" + rigName + "\"\npath = \"rigs/repo\"\n"
		if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cityToml("api")
	if err := persistProviderScopeOwnership(city, rigDir, providerScopeIntent{Transport: "proxied", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rigDir); err != nil {
		t.Fatal(err)
	}

	// The rename: same directory, new label, no start in between.
	cityToml("web")

	// The door every removal takes: `gc rig remove`, controllerState.DeleteRig
	// and the path change in controllerState.UpdateRig all detach by this key.
	if err := removeProviderScopeOwnershipRecord(city, "rig:web"); err != nil {
		t.Fatalf("removeProviderScopeOwnershipRecord: %v", err)
	}

	data, err := os.ReadFile(providerScopeOwnershipPath(city))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"rig:`) {
		t.Fatalf("removal left a rig-keyed record behind: %s", data)
	}
	key, entry, owned, err := providerScopeOwnershipRecord(city, rigDir)
	if err != nil || !owned || !strings.HasPrefix(key, "path:") {
		t.Fatalf("post-removal record = (%q, %+v, %t, %v), want a detached path record", key, entry, owned, err)
	}
	if entry.State != providerScopeReady {
		t.Fatalf("detaching lost the record's state: %+v", entry)
	}

	// city.toml is written without the rig only after the detach succeeds.
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"c\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateProviderScopeOwnership(city, &config.City{}); err != nil {
		t.Fatalf("removed renamed rig refused city startup: %v", err)
	}
}
