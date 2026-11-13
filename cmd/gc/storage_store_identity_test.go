package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func writeScopeProjectID(t *testing.T, scopeRoot, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, id); err != nil {
		t.Fatalf("writing project identity: %v", err)
	}
}

// TestReusedDirectoryWithRecreatedBeadsRefusesAsIdentityMismatch is B15: a
// city converged (here by genesis) on work store project A. Its .beads is
// recreated (project B) and .gc kept. The new work store holds no
// infrastructure row, so containment passes vacuously, and without the
// identity line the boot served project A's binding to project B.
func TestReusedDirectoryWithRecreatedBeadsRefusesAsIdentityMismatch(t *testing.T) {
	cityPath := t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-original")
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	stubInfraMigrationSource(t)

	var first bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &first)
	if err != nil {
		t.Fatalf("genesis boot refused: %v", err)
	}
	_ = routes.close()

	writeScopeProjectID(t, cityPath, "prj-recreated")
	var second bytes.Buffer
	again, err := storageBootGate(cityPath, cfg, "gc start", nil, &second)
	if err == nil {
		_ = again.close()
		t.Fatalf("a binding converged from project prj-original was served to work store prj-recreated; stderr=%s", second.String())
	}
	for _, want := range []string{"prj-original", "prj-recreated", "different work store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

// TestStaleBindingOfAnotherWorkStoreRefusesAsIdentityMismatch is B15 on a
// stale binding: the marker records work store project A, the binding's
// database is gone, and .beads was recreated (project B). The re-copy the
// stale state prescribes would carry project A's retained rows into project
// B's city and rewrite the marker with B's identity, after which nothing
// shows the mismatch. So the boot refuses as identity-mismatch without
// offering it, and both re-copies refuse without writing; the same work
// store still gets, and can run, the backup re-copy.
func TestStaleBindingOfAnotherWorkStoreRefusesAsIdentityMismatch(t *testing.T) {
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "project A's session", Type: "session", Labels: []string{"gc:session"}})
	cityPath := t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-original")
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if backedUp, err := infraPathExists(target.RetainedBackupPath()); err != nil || !backedUp {
		t.Fatalf("the cutover kept no retained backup (%v, %v); the re-copy under test is never offered", backedUp, err)
	}
	if err := os.RemoveAll(target.Dir); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if boot := checkInfraClassConvergence(cityPath, cfg, "gc start", &log); boot.Outcome != infraMigrationUncheckable || !strings.Contains(log.String(), "--"+storageFromBackupFlag) {
		t.Fatalf("the control boot of the same work store = %s, want the stale binding's backup re-copy: %s", boot.Outcome, log.String())
	}

	writeScopeProjectID(t, cityPath, "prj-recreated")
	log.Reset()
	boot := checkInfraClassConvergence(cityPath, cfg, "gc start", &log)
	advice := infraMigrationOperatorAdvice(boot, "gc start")
	if boot.Outcome != infraMigrationIdentityMismatch {
		t.Fatalf("boot over a stale binding of another work store = %s, want identity-mismatch: %s", boot.Outcome, log.String())
	}
	if strings.Contains(log.String()+advice, storageFromBackupFlag) {
		t.Errorf("the boot offers --%s for another work store's binding: %s%s", storageFromBackupFlag, log.String(), advice)
	}
	markerBefore, err := os.ReadFile(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []infraMigrationSource{infraMigrationFromBackup, infraMigrationFromWork} {
		log.Reset()
		if got := runInfraClassMigrationFrom(cityPath, target, from, "gc storage migrate", &log); got.Outcome != infraMigrationIdentityMismatch {
			t.Fatalf("a re-copy (%v) for another work store = %s, want identity-mismatch: %s", from, got.Outcome, log.String())
		}
		if present, _ := infraPathExists(target.Database); present {
			t.Fatalf("the refused re-copy (%v) rebuilt the binding's database", from)
		}
		if after, _ := os.ReadFile(target.MarkerPath()); !bytes.Equal(markerBefore, after) {
			t.Fatalf("the refused re-copy (%v) rewrote the marker: %q -> %q", from, markerBefore, after)
		}
	}

	writeScopeProjectID(t, cityPath, "prj-original")
	log.Reset()
	if got := runInfraClassMigrationFrom(cityPath, target, infraMigrationFromBackup, "gc storage migrate", &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the backup re-copy for the work store the marker names = %s: %s", got.Outcome, log.String())
	}
}

// TestStorageRecoverStrandedRefusesABindingOfAnotherWorkStore is B15 on the
// repair: the binding was converged from work store project A, and .beads was
// recreated (project B). Recovering would copy project B's rows into project
// A's binding and record them in its manifest, so the repair holds as the boot
// does, a dry run included, and writes nothing. The work store the marker
// names still recovers the same row.
func TestStorageRecoverStrandedRefusesABindingOfAnotherWorkStore(t *testing.T) {
	cityPath := recoveryCityPath(t)
	writeScopeProjectID(t, cityPath, "prj-original")
	source := stubStrandedRecoverySource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "live session", Type: "session", Labels: []string{"gc:session"}})
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "an order vote", Type: "task", Labels: []string{"order-tracking"}})
	if got := strandedIDs(t, cityPath, target); !slices.Contains(got, stranded.ID) {
		t.Fatalf("the fixture did not strand %s (stranded: %v); the scenario under test was never created", stranded.ID, got)
	}

	writeScopeProjectID(t, cityPath, "prj-recreated")
	sourceBefore := infraStoreFingerprint(t, source)
	manifestBefore := manifestIDs(t, target)
	bindingBefore := bindingFingerprint(t, target)
	stubInfraControllerPing(t, 0)
	request := storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}
	for _, dryRun := range []bool{true, false} {
		var stdout, stderr bytes.Buffer
		code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{DryRun: dryRun}, &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "different work store") || !strings.Contains(stderr.String(), "prj-recreated") {
			t.Fatalf("recovery (dry run %v) into the binding of another work store = %d, output %q; want the identity-mismatch refusal", dryRun, code, stdout.String()+stderr.String())
		}
	}
	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Errorf("the refused recovery changed the work store: before %v, after %v", sourceBefore, after)
	}
	if after := manifestIDs(t, target); !slices.Equal(manifestBefore, after) {
		t.Errorf("the refused recovery changed the manifest: before %v, after %v", manifestBefore, after)
	}
	if after := bindingFingerprint(t, target); !slices.Equal(bindingBefore, after) {
		t.Errorf("the refused recovery changed the binding: before %v, after %v", bindingBefore, after)
	}

	writeScopeProjectID(t, cityPath, "prj-original")
	var stdout, stderr bytes.Buffer
	if code := doStorageRecoverStranded(context.Background(), request, strandedRecoveryOptions{}, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery for the work store the marker names = %d: %s%s", code, stdout.String(), stderr.String())
	}
	if got := manifestIDs(t, target); !slices.Contains(got, stranded.ID) {
		t.Errorf("the recovery did not record %s in the manifest: %v", stranded.ID, got)
	}
}

// TestConvergedBootBackfillsALegacyMarker: a marker written before the
// identity line existed is back-filled by the first boot that proves
// containment, and from then on a different work store refuses.
func TestConvergedBootBackfillsALegacyMarker(t *testing.T) {
	cityPath := t.TempDir()
	writeScopeProjectID(t, cityPath, "prj-legacy")
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	stubInfraMigrationSource(t)
	target := mustResolveInfraTarget(t, cityPath, cfg)

	routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("genesis boot refused: %v", err)
	}
	_ = routes.close()
	// Strip the marker back to its legacy one-line shape.
	data, err := os.ReadFile(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	legacy := lines[len(lines)-1] + "\n"
	if err := os.WriteFile(target.MarkerPath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	routes, err = storageBootGate(cityPath, cfg, "gc start", nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("a legacy-marker city was refused: %v", err)
	}
	_ = routes.close()
	id, recorded, err := readMarkerWorkStoreIdentity(target.MarkerPath())
	if err != nil || !recorded || id.ProjectID != "prj-legacy" {
		t.Fatalf("after a converged boot the marker identity = (%+v, %v, %v), want project prj-legacy back-filled", id, recorded, err)
	}
	after, _ := os.ReadFile(target.MarkerPath())
	if !strings.HasSuffix(string(after), legacy) {
		t.Errorf("the back-fill rewrote the marker's time line, which every gc reads as the file's last field: %q", after)
	}
	if _, ok := readInfraMarkerBound(target); !ok {
		t.Error("after the back-fill the marker's write time no longer reads")
	}

	writeScopeProjectID(t, cityPath, "prj-other")
	if routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &bytes.Buffer{}); err == nil {
		_ = routes.close()
		t.Fatal("after the back-fill, a different work store was still served")
	}
}

// TestMovedCityIsNotAnIdentityMismatch: the path is recorded but never
// decides. A city directory moved with its .gc is the same city.
func TestMovedCityIsNotAnIdentityMismatch(t *testing.T) {
	recorded := workStoreIdentity{ProjectID: "prj-a", Path: "/old/place"}
	current := workStoreIdentity{ProjectID: "prj-a", Path: "/new/place"}
	if why := workStoreIdentityMismatch(recorded, current); why != "" {
		t.Fatalf("a moved city reported a mismatch: %s", why)
	}
}

func TestWorkStoreIdentityMismatchRules(t *testing.T) {
	for _, tc := range []struct {
		name              string
		recorded, current workStoreIdentity
		mismatch          bool
	}{
		{"same project", workStoreIdentity{ProjectID: "a"}, workStoreIdentity{ProjectID: "a"}, false},
		{"project differs", workStoreIdentity{ProjectID: "a"}, workStoreIdentity{ProjectID: "b"}, true},
		{"project decides over database", workStoreIdentity{ProjectID: "a", Database: "x"}, workStoreIdentity{ProjectID: "a", Database: "y"}, false},
		{"database differs, no projects", workStoreIdentity{Database: "x"}, workStoreIdentity{Database: "y"}, true},
		{"recorded project, current has none", workStoreIdentity{ProjectID: "a"}, workStoreIdentity{Path: "/c"}, true},
		{"nothing recorded to contradict", workStoreIdentity{Path: "/c"}, workStoreIdentity{ProjectID: "a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workStoreIdentityMismatch(tc.recorded, tc.current) != ""; got != tc.mismatch {
				t.Fatalf("mismatch = %v, want %v", got, tc.mismatch)
			}
		})
	}
}

func TestWorkStoreIdentityRoundTripsThroughTheMarkerLine(t *testing.T) {
	id := workStoreIdentity{ProjectID: "prj-x", Database: "bd_x", Path: "/city with spaces/x"}
	got, err := parseWorkStoreIdentity(id.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != id.ProjectID || got.Database != id.Database || got.Path != id.Path {
		t.Fatalf("round trip = %+v, want %+v", got, id)
	}
	empty, err := parseWorkStoreIdentity(workStoreIdentity{}.String())
	if err != nil || !empty.empty() {
		t.Fatalf("empty round trip = (%+v, %v)", empty, err)
	}
	if _, err := parseWorkStoreIdentity("project"); err == nil {
		t.Fatal("a field with no value parsed")
	}
}

func TestReadWorkStoreIdentityPrefersL1OverL2(t *testing.T) {
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scopeMetadataJSONPath(scope), []byte(`{"backend":"dolt","project_id":"l2","dolt_database":"bd_l2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := readWorkStoreIdentity(scope)
	if err != nil || id.ProjectID != "l2" || id.Database != "bd_l2" {
		t.Fatalf("L2-only identity = (%+v, %v)", id, err)
	}
	writeScopeProjectID(t, scope, "l1")
	id, err = readWorkStoreIdentity(scope)
	if err != nil || id.ProjectID != "l1" {
		t.Fatalf("identity with L1 = (%+v, %v), want l1", id, err)
	}
}

// TestUnreadableWorkStoreIdentityIsUncheckableNotAMismatch: an identity file
// that is malformed or unreadable says nothing about which work store this
// is. The binding is uncheckable (a standing refusal naming the file), never
// "a different work store".
func TestUnreadableWorkStoreIdentityIsUncheckableNotAMismatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, path string)
	}{
		{"malformed", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("[project\nid = "), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"unreadable", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeScopeProjectID(t, cityPath, "prj-original")
			cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
			stubInfraMigrationSource(t)
			routes, err := storageBootGate(cityPath, cfg, "gc start", nil, &bytes.Buffer{})
			if err != nil {
				t.Fatalf("genesis boot refused: %v", err)
			}
			_ = routes.close()

			identityPath := contract.ProjectIdentityPath(cityPath)
			tc.spoil(t, identityPath)
			var stderr bytes.Buffer
			report := checkInfraClassConvergence(cityPath, cfg, "gc start", &stderr)
			if report.Outcome != infraMigrationUncheckable {
				t.Fatalf("outcome = %s, want uncheckable; stderr=%s", report.Outcome, stderr.String())
			}
			if report.Fault == nil || !strings.Contains(report.Fault.Error(), identityPath) {
				t.Errorf("fault %v does not name %s", report.Fault, identityPath)
			}
			if strings.Contains(stderr.String(), "different work store") {
				t.Errorf("an unreadable identity was reported as a different work store: %s", stderr.String())
			}
			if _, err := storageBootGate(cityPath, cfg, "gc start", nil, &bytes.Buffer{}); err == nil {
				t.Error("boot served a binding whose work-store identity could not be read")
			}
		})
	}
}
