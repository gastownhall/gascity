package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/rig"
	"github.com/gastownhall/gascity/internal/ssrf"
)

// writeRigStore creates a minimal structurally-valid .beads store under dir so
// RigComplete / teardown tests have real files to act on.
func writeRigStore(t *testing.T, dir string) {
	t.Helper()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"web"}`), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
}

// TestTeardownPartialRigRemovesDirAndDropsDB proves the G14 physical teardown:
// the created working tree is removed (subsuming .beads) and the manifested Dolt
// database is dropped through the swappable client seam.
func TestTeardownPartialRigRemovesDirAndDropsDB(t *testing.T) {
	cs := &controllerState{cityPath: t.TempDir()}
	rigDir := filepath.Join(cs.cityPath, "rigs", "x")
	writeRigStore(t, rigDir)

	var dropped []string
	orig := controllerDropManagedDoltDatabase
	controllerDropManagedDoltDatabase = func(_ *controllerState, _ context.Context, name string) error {
		dropped = append(dropped, name)
		return nil
	}
	defer func() { controllerDropManagedDoltDatabase = orig }()

	err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{
		RigName:    "x",
		CreatedDir: rigDir,
		DoltDB:     "xdb",
	})
	if err != nil {
		t.Fatalf("TeardownPartialRig = %v, want nil", err)
	}
	if _, statErr := os.Stat(rigDir); !os.IsNotExist(statErr) {
		t.Fatalf("rig dir still present after teardown (stat err = %v)", statErr)
	}
	if len(dropped) != 1 || dropped[0] != "xdb" {
		t.Fatalf("dropped = %v, want [xdb]", dropped)
	}
}

// TestTeardownPartialRigRederivesDoltDBFromDisk proves the managed-DB orphan
// fix: when a provision fails AFTER InitStore minted the managed Dolt DB but
// BEFORE the success path recorded it, the manifest carries only CreatedDir.
// Teardown must re-derive the DB name from the on-disk .beads/metadata.json
// (before RemoveAll destroys it) and drop it, so the DB is not orphaned and
// cannot collide with a later same-name add.
func TestTeardownPartialRigRederivesDoltDBFromDisk(t *testing.T) {
	t.Setenv("GC_DOLT", "") // the managed DB must not be skip-deferred
	cs := &controllerState{cityPath: t.TempDir()}
	rigDir := filepath.Join(cs.cityPath, "rigs", "web")
	writeRigStore(t, rigDir) // .beads/metadata.json names dolt_database "web"

	var dropped []string
	orig := controllerDropManagedDoltDatabase
	controllerDropManagedDoltDatabase = func(_ *controllerState, _ context.Context, name string) error {
		dropped = append(dropped, name)
		return nil
	}
	defer func() { controllerDropManagedDoltDatabase = orig }()

	// The bug's pre-fix state: the manifest recorded the created dir but NOT the
	// minted DB (the success-path onManifest never ran).
	err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{
		RigName:    "web",
		CreatedDir: rigDir,
	})
	if err != nil {
		t.Fatalf("TeardownPartialRig = %v, want nil", err)
	}
	if _, statErr := os.Stat(rigDir); !os.IsNotExist(statErr) {
		t.Fatalf("rig dir still present after teardown (stat err = %v)", statErr)
	}
	if len(dropped) != 1 || dropped[0] != "web" {
		t.Fatalf("dropped = %v, want [web] re-derived from on-disk metadata.json", dropped)
	}
}

// TestTeardownPartialRigZeroManifestIsNoOp proves a zero manifest removes
// nothing and drops nothing — the safe default that never deletes data the
// machine cannot prove it created.
func TestTeardownPartialRigZeroManifestIsNoOp(t *testing.T) {
	cs := &controllerState{cityPath: t.TempDir()}
	dropCalled := false
	orig := controllerDropManagedDoltDatabase
	controllerDropManagedDoltDatabase = func(_ *controllerState, _ context.Context, _ string) error {
		dropCalled = true
		return nil
	}
	defer func() { controllerDropManagedDoltDatabase = orig }()

	if err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{}); err != nil {
		t.Fatalf("zero-manifest teardown = %v, want nil", err)
	}
	if dropCalled {
		t.Fatal("zero manifest dropped a database")
	}
}

// TestRigCompleteProbe proves the boot-sweep completeness probe: a rig present
// in config with a valid store is complete; one without the store file, or one
// absent from config, is not.
func TestRigCompleteProbe(t *testing.T) {
	tmp := t.TempDir()
	rigDir := filepath.Join(tmp, "rigs", "web")
	writeRigStore(t, rigDir)

	cs := &controllerState{
		cityPath: tmp,
		cfg: &config.City{Rigs: []config.Rig{
			{Name: "web", Path: rigDir, Prefix: "web", DefaultBranch: "main"},
			{Name: "empty", Path: filepath.Join(tmp, "rigs", "empty")}, // no .beads
		}},
	}

	if complete, prefix, branch := cs.RigComplete("web"); !complete || prefix != "web" || branch != "main" {
		t.Fatalf("RigComplete(web) = (%v,%q,%q), want (true, web, main)", complete, prefix, branch)
	}
	if complete, _, _ := cs.RigComplete("empty"); complete {
		t.Fatal("RigComplete(empty) = true, want false (no store)")
	}
	if complete, _, _ := cs.RigComplete("missing"); complete {
		t.Fatal("RigComplete(missing) = true, want false (not in config)")
	}
}

// TestProvisionedManagedDoltDatabaseSkipReturnsEmpty proves that under
// GC_DOLT=skip (the store init is deferred to the controller, so THIS request
// mints no database) the manifest claims no Dolt DB to drop — the rollback must
// never drop a database this add did not create.
func TestProvisionedManagedDoltDatabaseSkipReturnsEmpty(t *testing.T) {
	t.Setenv("GC_DOLT", "skip")
	cs := &controllerState{cityPath: t.TempDir()}
	rigDir := filepath.Join(cs.cityPath, "rigs", "x")
	writeRigStore(t, rigDir) // metadata.json carries dolt_database=web
	if db := cs.provisionedManagedDoltDatabase(rigDir); db != "" {
		t.Fatalf("provisionedManagedDoltDatabase under GC_DOLT=skip = %q, want empty", db)
	}
}

// TestProvisionRigFromGitRejectsPreexistingPath proves a git_url add refuses a
// path that already exists (the created-vs-preexisting invariant): the manifest
// callback is never invoked, so the rollback can never remove a dir this request
// did not create.
func TestProvisionRigFromGitRejectsPreexistingPath(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "rigs", "taken")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cs := &controllerState{cityPath: tmp}

	manifested := false
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "taken", Path: existing},
		"https://example.com/r.git",
		nil,
		func(api.RigProvisionManifest) error { manifested = true; return nil },
	)
	if err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("ProvisionRigFromGit preexisting = %v, want a validation error", err)
	}
	if manifested {
		t.Fatal("manifest callback ran for a preexisting path (rollback could delete a caller dir)")
	}
}

// TestProvisionRigFromGitManifestsThenWrapsCloneError proves two C4c behaviors
// in one no-network flow: the created_dir manifest is reported BEFORE the clone
// (record-then-create), and a git.Clone failure is wrapped with rig.ErrCloneFailed
// so the async mapper classifies it as clone_failed. An http:// URL is refused by
// git.Clone's scheme guard before any subprocess, so no network is touched.
func TestProvisionRigFromGitManifestsThenWrapsCloneError(t *testing.T) {
	origResolver := ssrf.HostResolver
	ssrf.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("140.82.112.3")}, nil }
	defer func() { ssrf.HostResolver = origResolver }()

	cs := &controllerState{cityPath: t.TempDir()}
	var manifests []api.RigProvisionManifest
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "httpfail"},
		"http://myhost.example/repo.git", // scheme-rejected by git.Clone, no network
		nil,
		func(m api.RigProvisionManifest) error { manifests = append(manifests, m); return nil },
	)
	if err == nil || !errors.Is(err, rig.ErrCloneFailed) {
		t.Fatalf("ProvisionRigFromGit clone-fail = %v, want wrapped rig.ErrCloneFailed", err)
	}
	// The dir was manifested (record-then-create) before the clone failure.
	if len(manifests) != 1 || manifests[0].CreatedDir == "" || manifests[0].RigName != "httpfail" {
		t.Fatalf("manifests = %+v, want one created_dir entry before the clone", manifests)
	}
}

// TestProvisionRigFromGitAbortsWhenPreCloneManifestPersistFails locks the C4c
// fail-closed contract for record-then-create: if durably persisting the
// created_dir manifest fails BEFORE the clone, ProvisionRigFromGit must abort
// without cloning. The pre-fix behavior logged the persist error and cloned
// anyway, creating an un-manifested rig directory that neither the boot sweep
// nor a re-clone pre-drop could discover — wedging the request_id/name on every
// retry. The clone is stubbed so no network is touched and the assertion is
// purely "did we reach the clone".
func TestProvisionRigFromGitAbortsWhenPreCloneManifestPersistFails(t *testing.T) {
	origResolver := ssrf.HostResolver
	ssrf.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("140.82.112.3")}, nil }
	defer func() { ssrf.HostResolver = origResolver }()

	cloneCalled := false
	origClone := rigCloneGit
	rigCloneGit = func(context.Context, string, string, git.CloneOptions) error {
		cloneCalled = true
		return nil
	}
	defer func() { rigCloneGit = origClone }()

	cs := &controllerState{cityPath: t.TempDir()}
	persistErr := errors.New("SetMetadataBatch failed")
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "wedge"},
		"https://example.com/repo.git",
		nil,
		func(api.RigProvisionManifest) error { return persistErr },
	)
	if err == nil || !errors.Is(err, persistErr) {
		t.Fatalf("ProvisionRigFromGit with a failing pre-clone manifest = %v, want the persist error", err)
	}
	if cloneCalled {
		t.Fatal("clone ran after the pre-clone manifest persist failed (an un-manifested dir could wedge the name)")
	}
}

// TestEnsurePublicGitHostFailsClosed proves the clone-path fence blocks a
// resolution error (fail-closed strict), where the fail-open pack fence would
// allow it.
func TestEnsurePublicGitHostFailsClosed(t *testing.T) {
	origResolver := ssrf.HostResolver
	ssrf.HostResolver = func(string) ([]net.IP, error) { return nil, errors.New("SERVFAIL") }
	defer func() { ssrf.HostResolver = origResolver }()

	_, err := ensurePublicGitHost("https://rebind.attacker.example/repo.git")
	if err == nil || !errors.Is(err, ssrf.ErrBlockedHost) {
		t.Fatalf("ensurePublicGitHost on resolution error = %v, want ErrBlockedHost (fail-closed)", err)
	}
}

// TestEnsurePublicGitHostPinsResolvedAddress proves the fence returns an
// http.curloptResolve override that pins the fence-approved public IP for the
// clone, so git connects to exactly that address instead of re-resolving the
// name (closing the DNS-rebinding TOCTOU). A literal-IP host has no name to pin.
func TestEnsurePublicGitHostPinsResolvedAddress(t *testing.T) {
	origResolver := ssrf.HostResolver
	ssrf.HostResolver = func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { ssrf.HostResolver = origResolver }()

	pin, err := ensurePublicGitHost("https://example.com/repo.git")
	if err != nil {
		t.Fatalf("ensurePublicGitHost = %v, want nil", err)
	}
	if pin != "example.com:443:93.184.216.34" {
		t.Fatalf("resolve override = %q, want %q", pin, "example.com:443:93.184.216.34")
	}

	// A literal-IP host names the address directly; there is no name to pin.
	pin, err = ensurePublicGitHost("https://93.184.216.34/repo.git")
	if err != nil {
		t.Fatalf("ensurePublicGitHost(literal) = %v, want nil", err)
	}
	if pin != "" {
		t.Fatalf("resolve override for literal IP = %q, want empty", pin)
	}
}

// TestProvisionRigFromGitRejectsEscapingRelativePath proves the city-root
// containment guard on the git_url path: a "../" relative path that resolves
// outside the city is refused with a validation error BEFORE any manifest,
// clone, or filesystem side effect — so the server can never clone (or later
// RemoveAll) outside the city.
func TestProvisionRigFromGitRejectsEscapingRelativePath(t *testing.T) {
	cs := &controllerState{cityPath: t.TempDir()}
	manifested := false
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "evil", Path: "../../etc/evil"},
		"https://example.com/r.git",
		nil,
		func(api.RigProvisionManifest) error { manifested = true; return nil },
	)
	if err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("escaping relative path = %v, want a validation error", err)
	}
	if manifested {
		t.Fatal("manifest callback ran for an escaping path (rollback could RemoveAll outside the city)")
	}
}

// TestProvisionRigFromGitRejectsAbsoluteClientPath proves a git_url add refuses
// a client-supplied absolute path outright: the clone destination is
// server-derived, so an absolute path (which could point anywhere) is a
// validation error.
func TestProvisionRigFromGitRejectsAbsoluteClientPath(t *testing.T) {
	cs := &controllerState{cityPath: t.TempDir()}
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "evil", Path: "/etc/evil"},
		"https://example.com/r.git",
		nil,
		nil,
	)
	if err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("absolute client path = %v, want a validation error", err)
	}
}

// TestProvisionRigFromGitRejectsSymlinkedParent proves the containment guard is
// symlink-aware: a lexically "../"-free path that escapes through a symlinked
// ancestor (a pre-existing <city>/link -> /outside) is still refused.
func TestProvisionRigFromGitRejectsSymlinkedParent(t *testing.T) {
	city := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(city, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cs := &controllerState{cityPath: city}
	manifested := false
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "rig", Path: "link/rig"},
		"https://example.com/r.git",
		nil,
		func(api.RigProvisionManifest) error { manifested = true; return nil },
	)
	if err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("symlinked-parent path = %v, want a validation error", err)
	}
	if manifested {
		t.Fatal("manifest callback ran for a symlink-escaping path")
	}
}

// TestTeardownPartialRigRefusesEscapingDir proves the teardown re-asserts
// containment before RemoveAll: a manifest CreatedDir outside the city (e.g. a
// poisoned durable record read by the boot sweep) is refused, and the external
// directory is left intact.
func TestTeardownPartialRigRefusesEscapingDir(t *testing.T) {
	cs := &controllerState{cityPath: t.TempDir()}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{
		RigName:    "x",
		CreatedDir: victim,
	})
	if err == nil {
		t.Fatal("teardown of an out-of-city dir returned nil, want a refusal error")
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Fatalf("teardown removed a directory outside the city (stat err = %v)", statErr)
	}
}

// TestTeardownPartialRigRefusesCityDatabaseDrop proves the Dolt-drop guard: a
// manifest DoltDB naming the city's own database (as a crafted repo's
// metadata.json could) is refused as a hard error and never dropped, even
// though the contained created dir is removed.
func TestTeardownPartialRigRefusesCityDatabaseDrop(t *testing.T) {
	tmp := t.TempDir()
	// City metadata pins dolt_database=hq.
	cityBeads := filepath.Join(tmp, ".beads")
	if err := os.MkdirAll(cityBeads, 0o755); err != nil {
		t.Fatalf("mkdir city .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityBeads, "metadata.json"), []byte(`{"dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatalf("write city metadata: %v", err)
	}
	rigDir := filepath.Join(tmp, "rigs", "x")
	writeRigStore(t, rigDir)

	dropped := []string{}
	orig := controllerDropManagedDoltDatabase
	controllerDropManagedDoltDatabase = func(_ *controllerState, _ context.Context, name string) error {
		dropped = append(dropped, name)
		return nil
	}
	defer func() { controllerDropManagedDoltDatabase = orig }()

	cs := &controllerState{cityPath: tmp, cfg: &config.City{}}
	err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{
		RigName:    "x",
		CreatedDir: rigDir,
		DoltDB:     "hq", // the city database — must be refused
	})
	if err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("dropping the city database = %v, want a validation refusal", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("city database was dropped: %v", dropped)
	}
}

// TestTeardownPartialRigRefusesOtherRigDatabaseDrop proves the guard also
// refuses a name belonging to a DIFFERENT rig's managed database (cross-tenant
// protection), while still allowing this rig's own database to drop.
func TestTeardownPartialRigRefusesOtherRigDatabaseDrop(t *testing.T) {
	tmp := t.TempDir()
	otherRig := filepath.Join(tmp, "rigs", "other")
	if err := os.MkdirAll(filepath.Join(otherRig, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir other .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherRig, ".beads", "metadata.json"), []byte(`{"dolt_database":"otherdb"}`), 0o644); err != nil {
		t.Fatalf("write other metadata: %v", err)
	}
	dropped := []string{}
	orig := controllerDropManagedDoltDatabase
	controllerDropManagedDoltDatabase = func(_ *controllerState, _ context.Context, name string) error {
		dropped = append(dropped, name)
		return nil
	}
	defer func() { controllerDropManagedDoltDatabase = orig }()

	cs := &controllerState{cityPath: tmp, cfg: &config.City{Rigs: []config.Rig{
		{Name: "other", Path: otherRig, Prefix: "other"},
	}}}

	// Dropping another rig's DB during rig "x" teardown is refused.
	err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{RigName: "x", DoltDB: "otherdb"})
	if err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("dropping another rig's database = %v, want a validation refusal", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("cross-tenant database was dropped: %v", dropped)
	}

	// This rig's own (distinct) database still drops.
	if err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{RigName: "x", DoltDB: "xdb"}); err != nil {
		t.Fatalf("dropping this rig's own database = %v, want nil", err)
	}
	if len(dropped) != 1 || dropped[0] != "xdb" {
		t.Fatalf("dropped = %v, want [xdb]", dropped)
	}
}
