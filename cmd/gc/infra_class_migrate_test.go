package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/storebinding"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
)

// infraSplitConfig returns a city whose five infra classes share one SQLite
// binding while work stays on the reserved work binding — the exact shape
// documented on config.StorageConfig.
func infraSplitConfig(path string) *config.City {
	return &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    "infra",
		},
		Bindings: map[string]config.StorageBindingConfig{
			"infra": {Provider: config.StorageProviderSQLiteBeads, Path: path},
		},
	}}
}

// migrateInfraClasses runs the operator command's migration body against an
// already configured city and returns the same report the command builds.
//
// It is the test-side spelling of `gc storage migrate --from-work`, minus the
// command's own preflights (source flag, controller probe, attestation, guard,
// rig census), so the copy machinery below is exercised on its own rather than
// through the flag parsing that gates it.
func migrateInfraClasses(t *testing.T, cityPath string, cfg *config.City, stderr io.Writer) infraMigrationReport {
	t.Helper()
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil {
		// A destination that will not resolve decides nothing about the city,
		// which is exactly what the command reports.
		fmt.Fprintf(stderr, "gc storage migrate: %v\n", err) //nolint:errcheck // best-effort test stderr
		return infraMigrationReport{Outcome: infraMigrationUncheckable}
	}
	if !ok {
		return infraMigrationReport{Outcome: infraMigrationNotConfigured}
	}
	report := runInfraClassMigration(cityPath, target, "gc storage migrate", stderr)
	report.Target = target
	report.BindingProvenEmpty, report.BindingProbe = infraBindingHoldsNothing(target)
	return report
}

// stubInfraMigrationSource points the migration at an in-memory work store. The
// destination is deliberately NOT stubbed: a fake destination is what let the
// migration pass while writing to a database no runtime binding opens, so every
// test below writes through the production opener at a temporary binding root.
func stubInfraMigrationSource(t *testing.T) beads.Store {
	t.Helper()
	source := beads.NewMemStore()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	return source
}

// refuseInfraMigrationSource fails the test if the work store is opened at all.
// This is the genesis property: a city with no infra split resolves no
// destination and must not touch the Dolt source at all.
//
// It is deliberately NOT the already-converged property. A converged boot
// re-proves containment against the source, because the marker records that
// the binding once held the source's infra slice and cannot record that it
// still does. Converged boots assert infraStoreFingerprint instead: read
// freely, change nothing.
func refuseInfraMigrationSource(t *testing.T) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		t.Error("the migration opened the work store when it should have done nothing")
		return beads.NewMemStore(), nil
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })
}

// mustResolveInfraTarget resolves the destination the migration will use.
func mustResolveInfraTarget(t *testing.T, cityPath string, cfg *config.City) infraBindingTarget {
	t.Helper()
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil {
		t.Fatalf("resolveInfraBindingTarget: %v", err)
	}
	if !ok {
		t.Fatal("infra binding did not resolve")
	}
	return target
}

// openMigratedDestination opens the migrated database through the production
// opener and closes it with the test.
func openMigratedDestination(t *testing.T, target infraBindingTarget) beads.Store {
	t.Helper()
	store, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening destination: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	return store
}

// infraStoreFingerprint returns the sorted ids of every bead in a store,
// closed rows included, so a test can prove a read-only path read.
func infraStoreFingerprint(t *testing.T, store beads.Store) []string {
	t.Helper()
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("fingerprinting store: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, b := range rows {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

// stubInfraControllerPing makes the city's controller-liveness probe answer
// with pid. Zero means nothing is serving the controller socket.
func stubInfraControllerPing(t *testing.T, pid int) {
	t.Helper()
	prev := infraMigrationControllerPing
	infraMigrationControllerPing = func(string) int { return pid }
	t.Cleanup(func() { infraMigrationControllerPing = prev })
}

func mustCreateInfraBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating %q: %v", b.Title, err)
	}
	return created
}

// populatedInfraBinding leaves one bead in the binding, written and closed the
// only way production writes it, and returns the city and the target.
func populatedInfraBinding(t *testing.T) (storageOperatorRequest, infraBindingTarget) {
	t.Helper()
	bindingParent := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(bindingParent, "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})

	target := mustResolveInfraTarget(t, request.CityPath, cfg)
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding to populate it: %v", err)
	}
	mustCreateInfraBead(t, binding, beads.Bead{Title: "a bead the binding already holds", Type: "session"})
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatalf("closing the populated binding: %v", err)
	}
	return request, target
}

// TestTheBindingDiagnosticsOpenerCannotWrite pins the read-only claim of
// `gc storage status` and `gc storage preflight` at the connection instead of
// at the caller.
//
// Both commands are documented read-only and both run against a LIVE city — a
// deploy gate may run either while a controller is serving the binding. Held by
// the writer opener, that contract is a property of what the writer opener
// happens to do today: a read-write connection CAN checkpoint the WAL on close,
// rewriting the main database and the -wal of a store something else is
// serving, and any write-on-open added later for the migration's benefit turns
// two diagnostics into writers with nothing in either command changing.
//
// The residue tests next to the two commands assert the consequence — nothing
// left behind. This asserts the cause, which is the half those tests cannot
// see: a mode=ro connection cannot take the write lock at all.
//
// Red-before, on the writer opener: the Create succeeds and the binding gains a
// row a diagnostic put there.
func TestTheBindingDiagnosticsOpenerCannotWrite(t *testing.T) {
	_, target := populatedInfraBinding(t)

	store, err := openInfraBindingReadOnly(target)
	if err != nil {
		t.Fatalf("opening the binding read-only: %v", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close

	// The read has to work, or the refusal below is the refusal of a store
	// that answers nothing and proves nothing about the one being served.
	if got := infraStoreFingerprint(t, store); len(got) != 1 {
		t.Fatalf("the read-only handle does not read the binding it was opened on: %v", got)
	}
	if _, err := store.Create(beads.Bead{Title: "a row a diagnostic must not be able to write", Type: "session"}); err == nil {
		t.Fatal("the opener the read-only diagnostics use accepted a write to a binding a controller may be serving")
	}
}

// leaveInfraBindingWALResident puts the binding in the state a stopped writer
// leaves behind — a populated, un-checkpointed -wal with no connection holding
// it — and returns the bytes of the main database and the -wal at that moment.
//
// It is built the way internal/beads builds the same fixture: copy both files
// out from under a still-open writer, let the writer close (which checkpoints
// and deletes the -wal), then put the pre-checkpoint pair back. Nothing here
// reaches past the production opener.
func leaveInfraBindingWALResident(t *testing.T, target infraBindingTarget) (mainDB, wal []byte) {
	t.Helper()
	walPath := target.Database + "-wal"

	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding to leave a live WAL: %v", err)
	}
	mustCreateInfraBead(t, binding, beads.Bead{Title: "a bead that is only in the WAL", Type: "session"})
	mainDB = mustReadFile(t, target.Database)
	wal = mustReadFile(t, walPath)
	if len(wal) == 0 {
		t.Fatalf("the fixture produced an empty -wal, so there is nothing for a close to checkpoint")
	}
	if err := closeBeadStoreHandle(binding); err != nil {
		t.Fatalf("closing the writer that seeded the WAL: %v", err)
	}

	if err := os.WriteFile(target.Database, mainDB, 0o644); err != nil {
		t.Fatalf("restoring the pre-checkpoint database: %v", err)
	}
	if err := os.WriteFile(walPath, wal, 0o644); err != nil {
		t.Fatalf("restoring the live -wal: %v", err)
	}
	if err := os.Remove(target.Database + "-shm"); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing the -shm the closed writer left: %v", err)
	}
	return mainDB, wal
}

// convergedInfraBindingRequest hands back a cut-over city as an operator
// request, so a diagnostic can be run against the arm past the convergence
// gate — the only arm a controller can be serving, and therefore the only arm
// on which "safe to run against a live city" means anything.
func convergedInfraBindingRequest(t *testing.T) (storageOperatorRequest, infraBindingTarget) {
	t.Helper()
	cityPath, cfg, _, target := convergedInfraCity(t)
	return storageOperatorRequest{CityPath: cityPath, Cfg: cfg, FleetStopped: true}, target
}

// TestTheReadOnlyDiagnosticsLeaveALiveWALIntact is the wiring half: the two
// commands actually take the read-only opener, not merely that one exists.
//
// The discriminator is the effect the finding names. A binding left by a
// stopped writer carries an un-checkpointed -wal, and a read-write connection
// closing as the sole connection checkpoints it — rewriting the main database
// and deleting the -wal of a store a controller may be about to serve, with
// zero logical writes. A mode=ro connection cannot take the write lock, so it
// reads the WAL-resident rows and leaves both files byte-identical.
//
// This is the half the two residue tests next to these commands cannot see:
// their binding was closed cleanly, so there is no -wal to checkpoint and the
// tree fingerprint is the same either way.
//
// The converged row is not a duplicate of the unconverged one, and it is the
// row that pins the wiring. `gc storage status` returns at the convergence gate
// on an unconverged city, having reached the binding only through the census —
// so an unconverged row passes while reportBindingRelics and
// classifyInfraContainmentGap, the two opens PAST that gate, go unexercised.
// Those two are also the only opens a live controller can collide with, because
// a binding being served is converged by construction: the boot gate serves
// infraConvergenceMarked and refuses infraMigrationUnconverged.
//
// Red-before, with any call site on openInfraDestination: that command deletes
// the -wal and rewrites the database it was only asked to read.
func TestTheReadOnlyDiagnosticsLeaveALiveWALIntact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func(*testing.T) (storageOperatorRequest, infraBindingTarget)
		run     func(storageOperatorRequest, io.Writer, io.Writer) int
		want    string
	}{
		{
			name:    "gc storage status counts the binding",
			fixture: populatedInfraBinding,
			run:     doStorageStatus,
			want:    "binding: 2 infrastructure bead(s)",
		},
		{
			name:    "gc storage status classifies a converged binding",
			fixture: convergedInfraBindingRequest,
			run:     doStorageStatus,
			// Past BOTH gates, so both remaining opens ran: `proven copy:`
			// prints only once classifyInfraContainmentGap has returned.
			// Neither weaker string proves that — the unconverged row's census
			// line prints BEFORE the convergence gate, and "converged: yes" is
			// a strict prefix of the no-manifest arm, which returns ahead of
			// that same open.
			want: "proven copy:",
		},
		{
			name:    "gc storage preflight rehearses the destination refusal",
			fixture: populatedInfraBinding,
			run:     doStoragePreflight,
			want:    "[BLOCK] destination",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, target := tc.fixture(t)
			mainBefore, walBefore := leaveInfraBindingWALResident(t, target)

			var stdout, stderr bytes.Buffer
			tc.run(request, &stdout, &stderr)

			// Without this the byte comparison is vacuous: a command that
			// blocked before it reached the binding would leave the files
			// alone for a reason that has nothing to do with the opener.
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("the command never read the binding, so the bytes below prove nothing; want %q\nstdout: %s\nstderr: %s", tc.want, stdout.String(), stderr.String())
			}
			if !bytes.Equal(mustReadFile(t, target.Database), mainBefore) {
				t.Errorf("a read-only command rewrote the binding database it was asked to read (checkpoint on close)")
			}
			if !bytes.Equal(mustReadFile(t, target.Database+"-wal"), walBefore) {
				t.Errorf("a read-only command rewrote the binding's -wal; a stopped writer's uncheckpointed rows are not a diagnostic's to move")
			}
		})
	}
}

// TestInfraMigrationClassesCoverEveryClassTheBindingsClaim pins the seam
// between the hand-listed set that MOVES beads and the derived set that CLAIMS
// them.
//
// infraMigrationClasses is written out by hand. infrastructureClasses() is
// derived — every coordclass.Class whose IsInfrastructure() says so — and it is
// what residencyBindingsFor builds bindings over and what
// storeref.ReservedPrefixesFor turns into the namespaces a binding covers. A
// sixth infrastructure class declared in coordclass therefore joins the derived
// set the day it is declared and joins the hand-listed one never.
//
// The failure that gap produces is silent and permanent: the new class's beads
// stay on the work axis because nothing migrated them, while the plan routes
// every read of that prefix to a binding that has never held one. Reads report
// absent for beads sitting in the work store, and no migration row notices,
// because the migration was asked to move five classes and moved five classes.
//
// The reverse direction is pinned too. Work in the migration list would copy
// the entire work ledger into the infrastructure binding — the one thing the
// list's own doc says is excluded by construction.
func TestInfraMigrationClassesCoverEveryClassTheBindingsClaim(t *testing.T) {
	migrated := make(map[coordclass.Class]bool, len(infraMigrationClasses))
	for _, class := range infraMigrationClasses {
		resolved := coordclassFor(string(class))
		if resolved == coordclass.ClassWork {
			t.Errorf("infraMigrationClasses names %q, which this build resolves to the work class; migrating work would copy the whole ledger into the infrastructure binding", class)
			continue
		}
		migrated[resolved] = true
	}

	infra := infrastructureClasses()
	if len(infra) == 0 {
		t.Fatal("no infrastructure classes at all, so the comparison below is vacuous and would pass against any drift")
	}
	for _, class := range infra {
		if !migrated[class] {
			t.Errorf("coordclass %s is infrastructure — it gets a binding and a reserved prefix — but infraMigrationClasses does not move it, so its beads stay on the work axis while every read of its prefix is routed at a binding that never held one", class)
		}
	}
	if len(migrated) != len(infra) {
		t.Errorf("infraMigrationClasses moves %d class(es) and the bindings claim %d; the two sets must be the same set, not merely overlapping", len(migrated), len(infra))
	}
}

// TestEnsureInfraClassMigratedCopiesEveryInfraClass pins the cutover: all five
// infra classes cross with ids and within-infra deps intact, closed infra beads
// cross too, work never crosses, the source is retained, and the marker makes
// the second boot a no-op.
func TestEnsureInfraClassMigratedCopiesEveryInfraClass(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)

	root := mustCreateInfraBead(t, source, beads.Bead{Title: "wisp root", Type: "molecule", Labels: []string{"gc:wisp"}})
	step := mustCreateInfraBead(t, source, beads.Bead{Title: "step", Type: "task", Labels: []string{"gc:wisp"}, ParentID: root.ID})
	if err := source.DepAdd(step.ID, root.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	message := mustCreateInfraBead(t, source, beads.Bead{Title: "mail", Type: "message"})
	order := mustCreateInfraBead(t, source, beads.Bead{Title: "order run", Type: "task", Labels: []string{"order-tracking"}})
	nudge := mustCreateInfraBead(t, source, beads.Bead{Title: "nudge", Type: "task", Labels: []string{"gc:nudge"}})
	closedOrder := mustCreateInfraBead(t, source, beads.Bead{Title: "finalize vote", Type: "task", Labels: []string{"order-tracking"}})
	if err := source.Close(closedOrder.ID); err != nil {
		t.Fatal(err)
	}
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "plain work", Type: "task"})

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migration outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}

	target := mustResolveInfraTarget(t, cityPath, cfg)
	if _, err := os.Stat(target.MarkerPath()); err != nil {
		t.Fatalf("marker missing: %v", err)
	}

	destination := openMigratedDestination(t, target)
	for _, want := range []beads.Bead{root, step, session, message, order, nudge, closedOrder} {
		got, err := destination.Get(want.ID)
		if err != nil {
			t.Fatalf("infra bead %s (%s) missing from destination: %v", want.ID, want.Title, err)
		}
		if got.Title != want.Title {
			t.Fatalf("bead %s title = %q, want %q", want.ID, got.Title, want.Title)
		}
		if got.CreatedAt.Unix() != want.CreatedAt.Unix() {
			t.Fatalf("bead %s creation time re-stamped: %s != %s", want.ID, got.CreatedAt.UTC(), want.CreatedAt.UTC())
		}
		if got.Metadata[infraMigrationStampKey] == "" {
			t.Fatalf("bead %s crossed without the migration provenance stamp", want.ID)
		}
	}
	if got, err := destination.Get(closedOrder.ID); err != nil || got.Status != "closed" {
		t.Fatalf("closed infra bead did not cross with its status: %+v %v", got, err)
	}
	if _, err := destination.Get(work.ID); err == nil {
		t.Fatal("work bead crossed into the infra binding; it must not")
	}

	deps, err := destination.DepList(step.ID, "down")
	if err != nil || len(deps) != 1 || deps[0].DependsOnID != root.ID {
		t.Fatalf("within-infra dep edge did not cross: %+v %v", deps, err)
	}

	// The source is retained verbatim: rollback is a config swap, not a restore.
	for _, want := range []beads.Bead{root, step, session, message, order, nudge, closedOrder, work} {
		if _, err := source.Get(want.ID); err != nil {
			t.Fatalf("source bead %s was removed by the migration: %v", want.ID, err)
		}
	}

	// The second boot re-proves containment rather than trusting the marker,
	// so it reads the source — and must leave it exactly as it found it.
	sourceBefore := infraStoreFingerprint(t, source)
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("second call outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Fatalf("the convergence re-check mutated the source: %v -> %v", sourceBefore, after)
	}
}

// TestEnsureInfraClassMigratedWritesTheComponentTheRuntimeReads is the decisive
// destination test. The migration must land in the database the deployed SQLite
// Beads provider opens — <binding root>/graph/beads.sqlite — under the reserved
// graph id prefix, and must NOT create the orphan <binding root>/beads.sqlite
// that a root-level open produces. It reads the copy back through the provider's
// own Graph front door, which is the only proof that matters: a migration
// nothing can read is worse than one that never ran.
func TestEnsureInfraClassMigratedWritesTheComponentTheRuntimeReads(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migration outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}

	wantPath, err := sqlitebinding.GraphPath(storeDir)
	if err != nil {
		t.Fatalf("GraphPath: %v", err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if target.Database != wantPath {
		t.Fatalf("destination database = %q, want the provider's component path %q", target.Database, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("nothing was written to the component the runtime binding opens (%s): %v", wantPath, err)
	}
	if orphan := filepath.Join(storeDir, filepath.Base(wantPath)); orphan != wantPath {
		if _, err := os.Stat(orphan); err == nil {
			t.Fatalf("the migration created the orphan database %s, which no runtime binding opens", orphan)
		}
	}

	// Read back through the provider's own front door, not through a handle
	// this test opened the same way the migration did.
	component, err := sqlitebinding.OpenGraph(storebinding.BindingSpec{
		Name:     "infra",
		Provider: sqlitebinding.ProviderID,
		Path:     storeDir,
	})
	if err != nil {
		t.Fatalf("opening the deployed Graph component: %v", err)
	}
	defer component.Close() //nolint:errcheck // best-effort close
	if component.Path() != wantPath {
		t.Fatalf("the provider opened %q, want %q", component.Path(), wantPath)
	}
	got, err := component.Graph().Get(session.ID)
	if err != nil {
		t.Fatalf("the migrated bead is not readable through the provider front door: %v", err)
	}
	if got.Title != session.Title {
		t.Fatalf("front-door read title = %q, want %q", got.Title, session.Title)
	}

	// The destination mints in the reserved graph namespace. Proven against the
	// provider's own minting rather than against a literal, so the prefix the
	// migration opens with cannot drift from the prefix the runtime opens with.
	viaProvider, err := component.Graph().Create(beads.Bead{Title: "minted by the provider", Type: "task"})
	if err != nil {
		t.Fatalf("minting through the provider front door: %v", err)
	}
	viaMigration, err := openMigratedDestination(t, target).Create(beads.Bead{Title: "minted by the migration handle", Type: "task"})
	if err != nil {
		t.Fatalf("minting through the migration's opener: %v", err)
	}
	providerPrefix, migrationPrefix := idNamespace(viaProvider.ID), idNamespace(viaMigration.ID)
	if providerPrefix != migrationPrefix {
		t.Fatalf("the migration opens the destination under id prefix %q while the runtime opens it under %q", migrationPrefix, providerPrefix)
	}
	if want, ok := config.ReservedClassPrefix(config.BeadClassGraph); !ok || migrationPrefix != want {
		t.Fatalf("destination id prefix = %q, want the reserved graph prefix %q", migrationPrefix, want)
	}
}

// idNamespace returns the prefix segment of a bead id.
func idNamespace(id string) string {
	namespace, _, found := strings.Cut(id, "-")
	if !found {
		return id
	}
	return namespace
}

// TestEnsureInfraClassMigratedIsDarkForGenesisCities pins the decisive negative
// property. A genesis city never migrates, and "never" means nothing is opened,
// nothing is created on disk, and nothing is said. A city born on an
// out-of-tree provider is genesis-only, so a binding this build has no
// migration for is the case that must stay dark forever, not just today.
func TestEnsureInfraClassMigratedIsDarkForGenesisCities(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.City
	}{
		{name: "no storage section", cfg: &config.City{}},
		{name: "every class on work", cfg: func() *config.City {
			c := infraSplitConfig(".gc/store")
			c.Storage.Classes = config.StorageClasses{
				Work: config.StorageWorkBinding, Graph: config.StorageWorkBinding,
				Sessions: config.StorageWorkBinding, Messaging: config.StorageWorkBinding,
				Orders: config.StorageWorkBinding, Nudges: config.StorageWorkBinding,
			}
			return c
		}()},
		{name: "non-sqlite provider", cfg: func() *config.City {
			c := infraSplitConfig("")
			c.Storage.Bindings["infra"] = config.StorageBindingConfig{Provider: "postgres", ConfigRef: "city-infra"}
			return c
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			refuseInfraMigrationSource(t)

			var log bytes.Buffer
			if got := checkInfraClassConvergence(cityPath, tc.cfg, "gc start", &log); got.Outcome != infraMigrationNotConfigured {
				t.Fatalf("outcome = %v, want not-configured; log: %s", got.Outcome, log.String())
			}
			if log.Len() != 0 {
				t.Fatalf("a genesis city reported migration activity: %s", log.String())
			}
			entries, err := os.ReadDir(cityPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("a genesis city had %d path(s) created under it: %v", len(entries), entries)
			}
		})
	}
}

// TestEnsureInfraClassMigratedIsDarkForAnAlreadyMigratedCity pins the other
// no-op: a converged city re-runs nothing. A migration that fires when it
// should not is worse than one that never fires — this one would clear and
// re-import a destination the city is already reading from.
func TestEnsureInfraClassMigratedIsDarkForAnAlreadyMigratedCity(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	var first bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &first); got.Outcome != infraMigrationConverged {
		t.Fatalf("first boot outcome = %v, want converged; log: %s", got.Outcome, first.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	before, err := os.Stat(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.Stat(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}

	sourceBefore := infraStoreFingerprint(t, source)
	var second bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &second); got.Outcome != infraMigrationConverged {
		t.Fatalf("second boot outcome = %v, want converged; log: %s", got.Outcome, second.String())
	}
	if second.Len() != 0 {
		t.Fatalf("a converged city reported migration activity: %s", second.String())
	}
	if after := infraStoreFingerprint(t, source); !slices.Equal(sourceBefore, after) {
		t.Fatalf("the convergence re-check mutated the source: %v -> %v", sourceBefore, after)
	}
	after, err := os.Stat(target.MarkerPath())
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the marker was rewritten on an already-migrated city")
	}
	// The manifest is the record of what the copy PROVED. Rewriting it on a
	// converged boot would re-derive it from whatever the binding holds now,
	// which is the copy's proof minus everything the binding's GC has collected
	// — and every one of those would then read as a stranded write.
	manifestAfter, err := os.Stat(target.ManifestPath())
	if err != nil {
		t.Fatal(err)
	}
	if !manifestBefore.ModTime().Equal(manifestAfter.ModTime()) {
		t.Fatal("the copy manifest was rewritten on an already-migrated city")
	}
}

// TestEnsureInfraClassMigratedRerunsOnAStaleMarker proves the marker is not
// trusted on its own. It lives at the binding root and the data lives in the
// component directory below it, so a destination wipe leaves a marker that
// would otherwise bless an empty binding as migrated.
func TestEnsureInfraClassMigratedRerunsOnAStaleMarker(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("first boot outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if err := os.RemoveAll(target.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target.MarkerPath()); err != nil {
		t.Fatalf("the wipe removed the marker too; this test no longer exercises staleness: %v", err)
	}

	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("re-run outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	if !bytes.Contains(log.Bytes(), []byte("claims convergence")) {
		t.Fatalf("the stale marker was not reported; log: %s", log.String())
	}
	if _, err := openMigratedDestination(t, target).Get(session.ID); err != nil {
		t.Fatalf("the wiped destination was blessed instead of re-copied: %v", err)
	}
}

// TestEnsureInfraClassMigratedRefusesPopulatedDestination pins the genesis
// guard: a destination holding content the work store never had is not
// overwritten, and no marker claims convergence.
func TestEnsureInfraClassMigratedRefusesPopulatedDestination(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "wisp root", Type: "molecule", Labels: []string{"gc:wisp"}})

	cfg := infraSplitConfig(storeDir)
	target := mustResolveInfraTarget(t, cityPath, cfg)
	destination := openMigratedDestination(t, target)
	genesis := mustCreateInfraBead(t, destination, beads.Bead{Title: "born here", Type: "session", Labels: []string{"gc:session"}})

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged; the migration overwrote a populated destination", got.Outcome)
	}
	if _, err := destination.Get(genesis.ID); err != nil {
		t.Fatalf("genesis bead was destroyed: %v", err)
	}
	if _, err := os.Stat(target.MarkerPath()); err == nil {
		t.Fatal("marker written despite a refused migration")
	}
	if !bytes.Contains(log.Bytes(), []byte("refusing to overwrite")) {
		t.Fatalf("refusal reason not reported; log: %s", log.String())
	}
}

// TestEnsureInfraClassMigratedRefusesIDCollidingDestination pins the guard as
// stamp-based rather than id-based: source and destination mint from
// independent sequences, so an unstamped destination row that happens to share
// a source id is still foreign content and must not be deleted. The collision
// is constructed rather than waited for — leaving it to chance would let the
// case go unexercised on every run.
func TestEnsureInfraClassMigratedRefusesIDCollidingDestination(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	migrating := mustCreateInfraBead(t, source, beads.Bead{Title: "from the work store", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	target := mustResolveInfraTarget(t, cityPath, cfg)
	destination := openMigratedDestination(t, target)
	collided, err := destination.(beads.ForeignIDCreator).CreateWithForeignID(beads.Bead{
		ID:     migrating.ID,
		Title:  "unrelated, same id",
		Type:   "session",
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding the id collision: %v", err)
	}

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged; the migration overwrote an id-colliding foreign row", got.Outcome)
	}
	got, err := destination.Get(collided.ID)
	if err != nil {
		t.Fatalf("colliding bead was destroyed: %v", err)
	}
	if got.Title != "unrelated, same id" {
		t.Fatalf("colliding bead was overwritten: title = %q", got.Title)
	}
}

// TestEnsureInfraClassMigratedResumesPartialAttempt proves an interrupted
// earlier attempt is cleared rather than merged, so a row the still-
// authoritative source has since changed cannot survive into the destination.
func TestEnsureInfraClassMigratedResumesPartialAttempt(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "renamed since", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	target := mustResolveInfraTarget(t, cityPath, cfg)
	destination := openMigratedDestination(t, target)
	stale := infraMigrationRow(session)
	stale.Title = "stale copy"
	if _, err := destination.(beads.ForeignIDCreator).CreateWithForeignID(stale); err != nil {
		t.Fatalf("seeding partial attempt: %v", err)
	}

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("resume outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	got, err := destination.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "renamed since" {
		t.Fatalf("stale partial-attempt row survived: title = %q", got.Title)
	}
}

// TestEnsureInfraClassMigratedAbortsBeforeMarker proves an unopenable work
// store leaves the city on the source with no convergence claim.
func TestEnsureInfraClassMigratedAbortsBeforeMarker(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged with an unopenable work store", got.Outcome)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if _, err := os.Stat(target.MarkerPath()); err == nil {
		t.Fatal("marker written despite an aborted migration")
	}
}

// growingInfraSource is a work store that gains one infra bead between the
// copy's read and the equality stage's read — the mid-copy arrival a
// snapshot-only equality check cannot see.
type growingInfraSource struct {
	beads.Store
	t     *testing.T
	lists int
	late  beads.Bead
}

func (s *growingInfraSource) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	if s.lists == 2 && s.late.ID == "" {
		created, err := s.Create(beads.Bead{Title: "arrived mid-copy", Type: "session", Labels: []string{"gc:session"}})
		if err != nil {
			s.t.Fatalf("seeding the mid-copy arrival: %v", err)
		}
		s.late = created
	}
	return s.Store.List(query)
}

// DepMetadata forwards the leaf's edge-payload read. The subject here is a
// mid-copy arrival, not an edge payload, and an interface-embedding double that
// stays silent about the capability would be refused as unanswerable before the
// arrival could ever be observed.
func (s *growingInfraSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return depMetadataThrough(s.Store, issueID, dependsOnID)
}

// TestEnsureInfraClassMigratedBlocksOnAnEqualityMismatch proves the equality
// stage is a real gate rather than a formality. A bead written to the work
// store while the copy was running is in the source and not in the destination;
// blessing that copy would strand it. The mismatch must block the marker, and
// the next boot against a quiet source must converge and carry the late row.
func TestEnsureInfraClassMigratedBlocksOnAnEqualityMismatch(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	growing := &growingInfraSource{Store: beads.NewMemStore(), t: t}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return growing, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	early := mustCreateInfraBead(t, growing.Store, beads.Bead{Title: "copied", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged on the equality mismatch; log: %s", got.Outcome, log.String())
	}
	if !bytes.Contains(log.Bytes(), []byte("equality check")) {
		t.Fatalf("the mismatch was not reported as an equality failure; log: %s", log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if _, err := os.Stat(target.MarkerPath()); err == nil {
		t.Fatal("marker written despite an equality mismatch")
	}
	if growing.late.ID == "" {
		t.Fatal("the mid-copy arrival never happened; this test proved nothing")
	}

	// Quiet source: the retry converges, and the late row crosses.
	openInfraMigrationSource = func(string) (beads.Store, error) { return growing.Store, nil }
	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("retry outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	destination := openMigratedDestination(t, target)
	for _, want := range []beads.Bead{early, growing.late} {
		if _, err := destination.Get(want.ID); err != nil {
			t.Fatalf("bead %s (%s) did not cross on the retry: %v", want.ID, want.Title, err)
		}
	}
}

// TestVerifyInfraCopyFailsClosed pins the equality stage directly: a
// destination missing a bead, holding a drifted copy of one, or holding a row
// the source does not have is rejected.
func TestVerifyInfraCopyFailsClosed(t *testing.T) {
	source := beads.NewMemStore()
	destination := beads.NewMemStore()
	bead := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	if _, err := verifyInfraCopy(infraStoreOpener(destination), source); err == nil {
		t.Fatal("equality passed against an empty destination")
	}

	drifted := bead
	drifted.Title = "not the same"
	if _, err := destination.Create(drifted); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyInfraCopy(infraStoreOpener(destination), source); err == nil {
		t.Fatal("equality passed against a drifted copy")
	}

	faithful := beads.NewMemStore()
	if _, err := faithful.Create(bead); err != nil {
		t.Fatal(err)
	}
	proven, err := verifyInfraCopy(infraStoreOpener(faithful), source)
	if err != nil {
		t.Fatalf("equality rejected a faithful copy: %v", err)
	}
	// The proven set is the manifest's content, so it must be exactly what was
	// checked — a manifest wider than the proof would excuse a real strand, and
	// a narrower one would report the binding's own GC as a strand.
	if !slices.Equal(proven, []string{bead.ID}) {
		t.Fatalf("proven ids = %v, want exactly the verified slice %v", proven, []string{bead.ID})
	}

	// Equality runs both ways: a row the copy invented is a failed copy too.
	if _, err := faithful.Create(beads.Bead{Title: "invented", Type: "session", Labels: []string{"gc:session"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyInfraCopy(infraStoreOpener(faithful), source); err == nil {
		t.Fatal("equality passed against a destination holding an extra row")
	}

	// A re-stamped creation time breaks every age-based gate downstream.
	// Asserted on the comparison directly: MemStore always stamps its own
	// creation time, so it cannot host a drifted one.
	shifted := bead
	shifted.CreatedAt = bead.CreatedAt.Add(48 * time.Hour)
	if diff := beadCopyDifference(bead, shifted); diff == "" {
		t.Fatal("a re-stamped creation time compared equal")
	}
	if diff := beadCopyDifference(bead, infraMigrationRow(bead)); diff != "" {
		t.Fatalf("the destination row shape broke equality: %s", diff)
	}
}

// TestResolveInfraBindingTargetRecognizesOnlyTheWholeSplit pins the gate: no
// [storage], a partial fan-out, a non-SQLite provider, and a relocated work
// class all read as "not configured" rather than half-migrated.
func TestResolveInfraBindingTargetRecognizesOnlyTheWholeSplit(t *testing.T) {
	cityPath := t.TempDir()

	notConfigured := func(name string, cfg *config.City) {
		t.Helper()
		if _, ok, err := resolveInfraBindingTarget(cityPath, cfg); ok || err != nil {
			t.Fatalf("%s resolved an infra binding (ok=%v err=%v)", name, ok, err)
		}
	}

	notConfigured("a city with no [storage]", &config.City{})

	partial := infraSplitConfig(".gc/store")
	partial.Storage.Classes.Nudges = config.StorageWorkBinding
	notConfigured("a partial infra fan-out", partial)

	foreign := infraSplitConfig(".gc/store")
	foreign.Storage.Bindings["infra"] = config.StorageBindingConfig{Provider: "postgres", ConfigRef: "city-infra"}
	notConfigured("a non-SQLite binding", foreign)

	relocatedWork := infraSplitConfig(".gc/store")
	relocatedWork.Storage.Classes.Work = "infra"
	notConfigured("a city with work off the work binding", relocatedWork)

	target := mustResolveInfraTarget(t, cityPath, infraSplitConfig(""))
	wantDatabase, err := sqlitebinding.GraphPath(filepath.Join(cityPath, config.DefaultSQLiteStoragePath))
	if err != nil {
		t.Fatalf("GraphPath: %v", err)
	}
	if target.Database != wantDatabase {
		t.Fatalf("default binding database = %q, want %q", target.Database, wantDatabase)
	}
	if target.Dir != filepath.Dir(wantDatabase) {
		t.Fatalf("default binding component dir = %q, want %q", target.Dir, filepath.Dir(wantDatabase))
	}
	if want := filepath.Dir(target.Dir); target.Root != want {
		t.Fatalf("default binding root = %q, want %q", target.Root, want)
	}
	if want := filepath.Join(target.Root, infraMigratedMarkerName); target.MarkerPath() != want {
		t.Fatalf("marker path = %q, want %q", target.MarkerPath(), want)
	}
}

// TestEnsureInfraClassMigratedRefusesWhileAnotherControllerIsLive pins the one
// writer exclusion the boot migration can actually prove. The hazard is not
// hypothetical: the supervisor calls newCityRuntime BEFORE it takes the
// controller lock, so without this gate a supervisor boot copies out from
// under a live standalone controller that is still writing infra beads to the
// Dolt source on pre-swap config.
func TestEnsureInfraClassMigratedRefusesWhileAnotherControllerIsLive(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	stubInfraControllerPing(t, os.Getpid()+1)

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged; the copy ran against a source another controller is writing", got.Outcome)
	}
	if !strings.Contains(log.String(), "is live on this city") {
		t.Fatalf("the refusal did not name the live controller; log: %s", log.String())
	}
	// Nothing may claim convergence, and nothing may have been written.
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if _, err := os.Stat(target.MarkerPath()); !os.IsNotExist(err) {
		t.Fatalf("a marker was written despite the refusal: %v", err)
	}
	if _, err := os.Stat(target.Database); !os.IsNotExist(err) {
		t.Fatalf("the destination database was created despite the refusal: %v", err)
	}
}

// TestInfraMigrationExcludesThisProcessFromTheControllerProbe pins the
// self-exclusion. The standalone controller starts its socket before it calls
// newCityRuntime, so an unfiltered ping answers with our own PID; reading that
// as a foreign writer would make every standalone boot refuse to migrate, and
// the failure would look like a permanently unconvergeable city.
func TestInfraMigrationExcludesThisProcessFromTheControllerProbe(t *testing.T) {
	cityPath := t.TempDir()

	stubInfraControllerPing(t, os.Getpid())
	if pid := infraMigrationForeignControllerPID(cityPath); pid != 0 {
		t.Fatalf("this process read as a foreign controller (pid %d); every standalone boot would refuse", pid)
	}

	stubInfraControllerPing(t, 0)
	if pid := infraMigrationForeignControllerPID(cityPath); pid != 0 {
		t.Fatalf("a silent socket read as a live controller (pid %d)", pid)
	}

	stubInfraControllerPing(t, os.Getpid()+1)
	if pid := infraMigrationForeignControllerPID(cityPath); pid != os.Getpid()+1 {
		t.Fatalf("a foreign controller was not reported: got %d", pid)
	}
}

// TestEnsureInfraClassMigratedSurfacesAStrandedWrite is the stranded-write
// window, closed. verifyInfraCopy re-reads the source, so a write DURING the
// copy is a refusal — but a write landing between that re-read and the marker
// rename is in the retained Dolt source and not in the binding, and every read
// after cutover routes past it. Trusting the marker on the next boot is what
// would make it invisible forever. Convergence is re-proved instead, and the
// strand is a blocked boot that names the bead.
func TestEnsureInfraClassMigratedSurfacesAStrandedWrite(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("first boot outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}

	// The write that lost the race: it landed in the source after the equality
	// re-read, so the marker blessed a binding that does not hold it.
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	log.Reset()
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationStranded {
		t.Fatalf("outcome = %v, want stranded; the stranded write was silently blessed as converged", report.Outcome)
	}
	if !strings.Contains(log.String(), stranded.ID) {
		t.Fatalf("the stranded bead id was not named; log: %s", log.String())
	}
	// A stranded city has been serving from the binding, so the revert would
	// abandon everything written since cutover. The migration names the defect;
	// the boot path's single revert decision — taken on evidence read off the
	// binding, which here holds the whole copy — is what warns against it.
	if advice := infraMigrationOperatorAdvice(report, "gc start"); !strings.Contains(advice, "Do NOT revert") {
		t.Fatalf("the strand report did not warn against the destructive revert; advice: %s", advice)
	}
	// The bead is not lost — it is intact in the retained source, which is what
	// makes recovery a real instruction rather than an apology.
	if _, err := source.Get(stranded.ID); err != nil {
		t.Fatalf("the stranded bead is not recoverable from the source: %v", err)
	}
}

// migratedThenCollectedCity builds a converged city, runs the REAL wisp GC over
// the binding, and returns the ids the GC hard-deleted from it.
//
// Nothing here simulates a deletion. The beads are seeded in the work store,
// migrated by the operator command, and then removed by memoryWispGC.runGC
// driving purgeExpiredBeadClosures and beadmail.PurgeReadMessageWisps against
// the binding's own store — the two paths that make a healthy city's binding
// diverge from its retained source. `now` is pushed past the TTL rather than
// the beads being back-dated, because the stores stamp creation time
// themselves.
func migratedThenCollectedCity(t *testing.T) (cityPath string, cfg *config.City, source beads.Store, collected []string) {
	t.Helper()
	cityPath = t.TempDir()
	source = stubInfraMigrationSource(t)

	// A closed workflow root and its step: the ownership closure the graph arm
	// of the GC collects once the root ages past the TTL.
	root := mustCreateInfraBead(t, source, beads.Bead{Title: "wisp root", Type: "molecule", Labels: []string{"gc:wisp"}})
	step := mustCreateInfraBead(t, source, beads.Bead{Title: "step", Type: "task", Labels: []string{"gc:wisp"}, ParentID: root.ID})
	if err := source.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	// A read message wisp: the retention arm's candidate, on a disjoint path.
	message := mustCreateInfraBead(t, source, beads.Bead{
		Title:     "delivered mail",
		Type:      "message",
		Ephemeral: true,
		Metadata:  beads.StringMap{mail.ReadMetadataKey: "true"},
	})
	// A bead nothing collects, so the city still has infra state to contain and
	// the containment check is doing real work rather than comparing two empties.
	survivor := mustCreateInfraBead(t, source, beads.Bead{Title: "live session", Type: "session", Labels: []string{"gc:session"}})

	cfg = infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)

	binding := openMigratedDestination(t, target)
	gc := newWispGC(time.Minute, 24*time.Hour, 24*time.Hour)
	if gc == nil {
		t.Fatal("wisp GC did not construct")
	}
	// Past the TTL for every seeded row, so this sweep is the first post-cutover
	// GC a real city would run rather than a no-op.
	purged, err := gc.runGC(beads.GraphStore{Store: binding}, beads.MailStore{Store: binding}, time.Now().Add(72*time.Hour))
	if err != nil {
		t.Fatalf("wisp gc: %v", err)
	}
	if purged == 0 {
		t.Fatal("the wisp GC collected nothing; this scenario proves nothing")
	}

	if _, err := binding.Get(survivor.ID); err != nil {
		t.Fatalf("the wisp GC took %s, which nothing makes collectible; the scenario is not the one being tested: %v", survivor.ID, err)
	}
	collected = []string{root.ID, step.ID, message.ID}
	sort.Strings(collected)
	for _, id := range collected {
		if _, err := binding.Get(id); err == nil {
			t.Fatalf("bead %s survived the wisp GC; the divergence this test needs was never created", id)
		}
		if _, err := source.Get(id); err != nil {
			t.Fatalf("the retained source lost %s; it keeps migrated rows verbatim by design: %v", id, err)
		}
	}
	return cityPath, cfg, source, collected
}

// TestEnsureInfraClassMigratedAcceptsWispGCDeletions is the false-positive the
// council caught, pinned closed. The binding is the live infra store after
// cutover and its own GC hard-deletes expired closed wisps and read mail, while
// the retained Dolt source keeps those rows verbatim forever by design. Absence
// alone therefore describes every healthy city from its first post-cutover
// sweep onward, and a detector that reported it would be muted — taking the
// real strand it exists to catch down with it.
func TestEnsureInfraClassMigratedAcceptsWispGCDeletions(t *testing.T) {
	cityPath, cfg, _, collected := migratedThenCollectedCity(t)

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("outcome = %v, want converged; the binding's own GC was reported as a stranded write. log: %s", got.Outcome, log.String())
	}
	if log.Len() != 0 {
		t.Fatalf("a healthy garbage-collected city reported activity: %s", log.String())
	}

	// Every later boot too: the divergence is permanent, so a check that only
	// tolerated it once would still block the city forever.
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("second boot outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	if log.Len() != 0 {
		t.Fatalf("a healthy garbage-collected city reported activity on reboot: %s", log.String())
	}
	if len(collected) == 0 {
		t.Fatal("nothing was collected; this test proved nothing")
	}
}

// TestEnsureInfraClassMigratedNamesAStrandOnAGarbageCollectedCity is the other
// direction, and the one that decides whether the fix was a fix or a mute. A
// city that has run its GC still has to name a genuine stranded write — and
// name only that, not the rows its GC legitimately collected.
func TestEnsureInfraClassMigratedNamesAStrandOnAGarbageCollectedCity(t *testing.T) {
	cityPath, cfg, source, collected := migratedThenCollectedCity(t)

	// The write that lost the race: it landed in the retained source after the
	// copy was verified, so the binding never received it.
	stranded := mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationStranded {
		t.Fatalf("outcome = %v, want stranded; the strand was lost in the GC exemption. log: %s", got.Outcome, log.String())
	}
	if !strings.Contains(log.String(), stranded.ID) {
		t.Fatalf("the stranded bead id was not named; log: %s", log.String())
	}
	for _, id := range collected {
		if strings.Contains(log.String(), id) {
			t.Fatalf("the report named %s, which the binding's own GC collected; log: %s", id, log.String())
		}
	}
	// The GC-collected rows are accounted for as context rather than silently
	// dropped, so an operator reading the alarm knows what the check can and
	// cannot see.
	if want := fmt.Sprintf("a further %d bead(s)", len(collected)); !strings.Contains(log.String(), want) {
		t.Fatalf("the report did not bound the indistinguishable residue (%q); log: %s", want, log.String())
	}
}

// TestInfraCopyManifestRecordsWhatTheEqualityStageProved pins the evidence the
// containment check reads. Absence from the binding is ambiguous on its own,
// and neither store retains anything to disambiguate it, so the proven id set
// is written down at cutover — before the marker, so a marker can never claim
// a convergence the manifest cannot substantiate.
func TestInfraCopyManifestRecordsWhatTheEqualityStageProved(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	source := stubInfraMigrationSource(t)
	session := mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	message := mustCreateInfraBead(t, source, beads.Bead{Title: "mail", Type: "message"})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "plain work", Type: "task"})

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)

	proven, recorded, err := readInfraCopyManifest(target)
	if err != nil {
		t.Fatalf("readInfraCopyManifest: %v", err)
	}
	if !recorded {
		t.Fatalf("no manifest was written at %s, so every later boot has to guess at absence", target.ManifestPath())
	}
	for _, want := range []beads.Bead{session, message} {
		if !proven[want.ID] {
			t.Fatalf("the manifest omits %s (%s), which the equality stage proved; the binding's GC deleting it would read as a strand", want.ID, want.Title)
		}
	}
	if proven[work.ID] {
		t.Fatalf("the manifest records work bead %s, which never crossed", work.ID)
	}
	if len(proven) != 2 {
		t.Fatalf("manifest holds %d ids, want exactly the two infra beads", len(proven))
	}

	// A refused copy leaves no manifest, for the same reason it leaves no
	// marker: nothing was proven.
	fresh := t.TempDir()
	freshCfg := infraSplitConfig(filepath.Join(fresh, ".gc", "store"))
	stubInfraControllerPing(t, os.Getpid()+1)
	log.Reset()
	if got := migrateInfraClasses(t, fresh, freshCfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged; log: %s", got.Outcome, log.String())
	}
	if _, err := os.Stat(mustResolveInfraTarget(t, fresh, freshCfg).ManifestPath()); !os.IsNotExist(err) {
		t.Fatalf("a manifest was written for a refused copy: %v", err)
	}
}

// TestEnsureInfraClassMigratedWritesTheManifestBeforeTheMarker pins the
// ordering the two files depend on. The marker says "converged"; the manifest
// is the only thing that makes that claim re-checkable. Writing the marker
// first means a failed manifest write leaves a city that reads as converged
// forever with stranded-write detection permanently off — the silent state this
// whole mechanism exists to avoid. A manifest that cannot be written must
// therefore leave no marker at all.
func TestEnsureInfraClassMigratedWritesTheManifestBeforeTheMarker(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	// Block the manifest rename alone, leaving the marker path writable: a
	// directory cannot be replaced by a rename of a regular file.
	target := mustResolveInfraTarget(t, cityPath, cfg)
	if err := os.MkdirAll(target.ManifestPath(), 0o755); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged; log: %s", got.Outcome, log.String())
	}
	if _, err := os.Stat(target.MarkerPath()); !os.IsNotExist(err) {
		t.Fatalf("a marker claims convergence that no manifest can substantiate: %v", err)
	}
	if !strings.Contains(log.String(), "infra copy manifest") {
		t.Fatalf("the failure was not reported as a manifest write; log: %s", log.String())
	}
}

// TestEnsureInfraClassMigratedReportsUncheckableConvergence pins the honest
// degradation. A city converged before the manifest existed has a marker and
// nothing to check it against. Both guesses available there are wrong — calling
// every absence a strand blocks a healthy garbage-collected city forever,
// calling none of them one hides the write the check exists to name — so it
// reports that detection is off instead of picking one.
func TestEnsureInfraClassMigratedReportsUncheckableConvergence(t *testing.T) {
	cityPath, cfg, source, target := convergedInfraCity(t)
	if err := os.Remove(target.ManifestPath()); err != nil {
		t.Fatal(err)
	}
	// A bead that WOULD be named as stranded if the manifest were there.
	mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})

	var log bytes.Buffer
	got := migrateInfraClasses(t, cityPath, cfg, &log)
	if got.Outcome != infraMigrationUncheckable {
		t.Fatalf("outcome = %v, want uncheckable; an unclassifiable city is neither proven converged nor blocked. log: %s", got.Outcome, log.String())
	}
	if !strings.Contains(log.String(), "detection is OFF") {
		t.Fatalf("the city did not report that the check is unavailable; log: %s", log.String())
	}
	if !strings.Contains(log.String(), target.ManifestPath()) {
		t.Fatalf("the report did not name the missing manifest; log: %s", log.String())
	}
	// This city has been serving from the binding, so the one thing the boot
	// path must not do is offer to point [storage.classes] back at the source.
	advice := infraMigrationOperatorAdvice(got, "gc start")
	if revert := fmt.Sprintf(infraRevertInstruction, config.StorageWorkBinding); strings.Contains(advice, revert) {
		t.Fatalf("a converged city with no manifest was offered the revert: %s", advice)
	}
}

// convergedInfraCity cuts a city over and hands it back converged, verifiable,
// and quiet — the state every degradation case below starts from, so that what
// those cases change is only the ability to RE-CHECK it.
func convergedInfraCity(t *testing.T) (cityPath string, cfg *config.City, source beads.Store, target infraBindingTarget) {
	t.Helper()
	cityPath = t.TempDir()
	source = stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	cfg = infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}
	return cityPath, cfg, source, mustResolveInfraTarget(t, cityPath, cfg)
}

// unlistableInfraSource is a work store whose List fails: one busy database, one
// momentary I/O fault. Nothing about it says the city failed to converge.
type unlistableInfraSource struct {
	beads.Store
	err error
}

func (s unlistableInfraSource) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }

// failInfraMigrationSourceWith points the work-store seam at a broken open for
// the rest of the test.
func failInfraMigrationSourceWith(t *testing.T, open func(string) (beads.Store, error)) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = open
	t.Cleanup(func() { openInfraMigrationSource = prev })
}

// infraRevertSentence is the exact instruction whose appearance this file
// polices. Pointing [storage.classes] back at the work binding re-routes every
// infra read at a Dolt source frozen at cutover; on a binding holding anything
// at all it silently abandons it.
func infraRevertSentence() string {
	return fmt.Sprintf(infraRevertInstruction, config.StorageWorkBinding)
}

// TestInfraMigrationRevertAdviceIsDecidedByEvidenceNotByOutcome pins the shape
// of the decision rather than the value of any one case: across the whole
// product of outcomes and evidence, the revert appears exactly when the binding
// was proven empty, and never because of which outcome was reached.
//
// The previous version of this test asserted "exactly one outcome carries the
// revert", and that framing is what let three rounds of this hazard through:
// each round found a new chain into that outcome, and the outcome was what
// decided. An outcome may now suppress the advice entirely — a converged or
// unconfigured city has nothing to say — but no outcome can produce the revert
// on its own.
func TestInfraMigrationRevertAdviceIsDecidedByEvidenceNotByOutcome(t *testing.T) {
	revert := infraRevertSentence()
	outcomes := []infraMigrationOutcome{
		infraMigrationNotConfigured,
		infraMigrationConverged,
		infraMigrationUnconverged,
		infraMigrationStranded,
		infraMigrationUncheckable,
	}
	// Outcomes that describe a city with nothing wrong say nothing at all;
	// every other outcome must end with a revert decision, one way or the other.
	silent := map[infraMigrationOutcome]bool{
		infraMigrationNotConfigured: true,
		infraMigrationConverged:     true,
	}
	for _, outcome := range outcomes {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/binding-proven-empty=%v", outcome, empty), func(t *testing.T) {
				report := infraMigrationReport{Outcome: outcome, BindingProvenEmpty: empty}
				if !empty {
					report.BindingProbe = fmt.Errorf("listing the infra binding: database is locked")
				}
				advice := infraMigrationOperatorAdvice(report, "gc start")
				if silent[outcome] {
					if advice != "" {
						t.Fatalf("a %v city was given boot advice it has no use for: %q", outcome, advice)
					}
					return
				}
				if got, want := strings.Contains(advice, revert), empty; got != want {
					t.Fatalf("revert rendered = %v, want %v for %v with binding-proven-empty=%v; the instruction must follow the evidence and nothing else: %q",
						got, want, outcome, empty, advice)
				}
				if !strings.Contains(strings.ToLower(advice), "revert") {
					t.Fatalf("the %v outcome neither offers the revert nor warns against it, so an operator reaching for it is unwarned: %q", outcome, advice)
				}
				if !empty && !strings.Contains(advice, "database is locked") {
					t.Fatalf("the revert was withheld without saying what stopped the binding being proven empty: %q", advice)
				}
			})
		}
	}
}

// infraBindingContents counts what the binding actually holds, independently of
// the migration's own evidence probe: it consults no marker, no manifest, and
// not infraBindingHoldsNothing. known=false means the test could not enumerate
// the binding at all, which is not the same as finding it empty.
//
// A binding whose database file does not exist holds nothing and is deliberately
// NOT created in order to be counted.
func infraBindingContents(t *testing.T, target infraBindingTarget) (count int, known bool) {
	t.Helper()
	if _, err := os.Stat(target.Database); err != nil {
		return 0, os.IsNotExist(err)
	}
	store, err := openInfraDestination(target)
	if err != nil {
		return 0, false
	}
	defer func() { _ = closeBeadStoreHandle(store) }()
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return 0, false
	}
	return len(rows), true
}

// skipWhenRootIgnoresPermissions skips a case whose whole subject is a
// directory this process cannot enter. Root enters anyway, so under euid 0 the
// case would be testing the opposite of what it says.
func skipWhenRootIgnoresPermissions(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits, so an unreadable root cannot be staged")
	}
}

// sealInfraDirectory makes a directory unenterable for the rest of the test —
// the local stand-in for a bare mountpoint or a volume whose permissions did
// not come back with it. The mode is restored on cleanup so t.TempDir can
// remove the tree.
func sealInfraDirectory(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// infraBindingRootLooked reports whether this test could enumerate the binding
// root, independently of the migration's own probe. It is the precondition on
// every absence the revert is derived from: a directory nobody can read answers
// "no such file" for the marker, the manifest and the database alike.
func infraBindingRootLooked(target infraBindingTarget) error {
	info, err := os.Stat(target.Root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", target.Root)
	}
	_, err = os.ReadDir(target.Root)
	return err
}

// failingInfraRename makes the atomic publish of the manifest or the marker fail
// for paths whose base name matches, and leaves every other rename alone.
func failingInfraRename(t *testing.T, base string) {
	t.Helper()
	prev := infraMigrationRename
	infraMigrationRename = func(from, to string) error {
		if filepath.Base(to) == base {
			return fmt.Errorf("simulated rename failure onto %s", to)
		}
		return prev(from, to)
	}
	t.Cleanup(func() { infraMigrationRename = prev })
}

// undeppableInfraSource is a work store whose rows list but whose dep edges
// stop listing partway through: the copy imports every bead and then fails,
// leaving a populated binding with no manifest and no marker.
//
// The failure is armed on the SECOND read of an id rather than on the first,
// and the two forwards below are what keep this double meaning what its name
// says. Two passes now read a source's edges before the import writes anything:
// infraSourceEdgePayloadRefusal walks every row to prove the payloads are
// readable, and importInfraSnapshot walks them again to copy. A double that
// failed on the first read — or that embedded beads.Store without forwarding
// DepMetadata, which strips beads.DepMetadataReader and reads as UNABLE TO
// ANSWER — would be refused before the destination was ever opened, and this
// case would quietly become a duplicate of the mute-source one instead of the
// populated-binding case the revert-advice table needs it to be.
type undeppableInfraSource struct {
	beads.Store
	err  error
	seen map[string]int
}

func (s undeppableInfraSource) DepList(id, direction string) ([]beads.Dep, error) {
	if s.seen == nil {
		return nil, s.err
	}
	s.seen[id]++
	if s.seen[id] > 1 {
		return nil, s.err
	}
	return s.Store.DepList(id, direction)
}

func (s undeppableInfraSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return depMetadataThrough(s.Store, issueID, dependsOnID)
}

// TestUndeppableInfraSourceFailsAfterTheRowsLand pins what the double above
// reaches, which the revert-advice table cannot.
//
// That table asserts only one direction for this case — the revert must not be
// RENDERED over a populated binding — so a double refused before the
// destination was ever opened leaves an empty binding, renders the revert
// legitimately, and passes while proving nothing. The case is then a silent
// duplicate of the source-cannot-list one two rows above it. Only an assertion
// that the rows actually landed keeps it the populated-binding case the table
// needs, and this is where the edge-payload passes would break it first.
func TestUndeppableInfraSourceFailsAfterTheRowsLand(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	source := stubInfraMigrationSource(t)
	resident := mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session", Labels: []string{"gc:session"}})
	broken := undeppableInfraSource{Store: source, err: fmt.Errorf("database is locked"), seen: map[string]int{}}
	failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return broken, nil })

	var log bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &log)
	if report.Outcome != infraMigrationUnconverged {
		t.Fatalf("Outcome = %v, want infraMigrationUnconverged: %s", report.Outcome, log.String())
	}
	if report.BindingProvenEmpty {
		t.Fatalf("the binding is provably empty, so the copy was refused before it imported anything and this double no longer reaches the import: %s", log.String())
	}
	count, known := infraBindingContents(t, report.Target)
	if !known || count == 0 {
		t.Fatalf("the binding holds %d bead(s) (known=%v), want the imported rows: the failure landed before %s was written", count, known, resident.ID)
	}
}

// TestInfraMigrationRevertAdviceRequiresAProvablyEmptyBinding is the property,
// driven over the whole space instead of over the paths.
//
// The property: the revert instruction is rendered ONLY for a binding whose
// root this boot could actually look inside — present, a directory, and
// enumerable — and which, having been looked inside, holds no beads and carries
// no record (marker or manifest) of ever having been in service. Anything else
// and the sentence abandons data.
//
// The root clause is the fourth round of this hazard and is a different shape
// from the three before it, which were all chains into an outcome. Every other
// fact here is an ABSENCE, and absence is only evidence when the place the
// thing would be was observed: an unmounted volume takes the marker, the
// manifest and the database away together, so all three read absent at once and
// a converged city whose data is sitting on that volume looks exactly like one
// that never cut over. "I looked and there is nothing there" and "I could not
// look" are different answers, and only the first licenses the revert.
//
// It is stated once and driven across every outcome and every failure point the
// cutover has, because the three previous rounds of this hazard were each caught
// by adding one more path test after one more chain reached the enum. A path
// test only ever covers the chain someone already thought of. This asserts the
// implication for whatever the boot did, so a new chain has to satisfy it too.
//
// Both halves of the boot's operator-visible output are checked, not just the
// returned advice: the migration writes its own reasons to stderr, and a revert
// named there is just as destructive as one named in the advice line.
func TestInfraMigrationRevertAdviceRequiresAProvablyEmptyBinding(t *testing.T) {
	revert := infraRevertSentence()
	covered := map[infraMigrationOutcome]bool{}

	// A city with no marker, no manifest and no database: the one state in
	// which the revert is correct, and the reason this property is not
	// vacuously satisfiable by never rendering the sentence at all.
	freshCity := func(t *testing.T) (string, *config.City, beads.Store) {
		t.Helper()
		cityPath := t.TempDir()
		source := stubInfraMigrationSource(t)
		mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
		return cityPath, infraSplitConfig(filepath.Join(cityPath, ".gc", "store")), source
	}
	// The same city with its binding root materialized — the state every boot
	// that opens the destination leaves behind, since beads.OpenSQLiteStore
	// creates the component directory. A pre-copy refusal is the only way to
	// reach the boot path WITHOUT it, and a root that was never created is a
	// root nothing could be read out of, so the two shapes are carried as
	// separate cases throughout this table.
	freshCityWithRoot := func(t *testing.T) (string, *config.City) {
		t.Helper()
		cityPath, cfg, _ := freshCity(t)
		if err := os.MkdirAll(mustResolveInfraTarget(t, cityPath, cfg).Root, 0o755); err != nil {
			t.Fatal(err)
		}
		return cityPath, cfg
	}

	for _, tc := range []struct {
		name string
		// setup leaves the city in the state to boot from and returns it.
		setup func(t *testing.T) (string, *config.City)
		// wantOutcome documents which outcome this case reaches, so the table
		// can prove it covers all five rather than assuming it does.
		wantOutcome infraMigrationOutcome
		// mustRender marks the cases where withholding the revert would be a
		// regression in the other direction: a city whose infra state is still
		// in the work store and which is given no way back to it.
		mustRender bool
	}{
		{
			name:        "a city with no infra split at all",
			wantOutcome: infraMigrationNotConfigured,
			setup: func(t *testing.T) (string, *config.City) {
				refuseInfraMigrationSource(t)
				return t.TempDir(), &config.City{}
			},
		},
		{
			name:        "a destination that will not resolve",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath := t.TempDir()
				blocker := filepath.Join(cityPath, "not-a-directory")
				if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				refuseInfraMigrationSource(t)
				return cityPath, infraSplitConfig(filepath.Join(blocker, "store"))
			},
		},
		{
			name:        "a clean cutover",
			wantOutcome: infraMigrationConverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _ := freshCity(t)
				return cityPath, cfg
			},
		},
		{
			name:        "a foreign controller before any copy",
			wantOutcome: infraMigrationUnconverged,
			mustRender:  true,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg := freshCityWithRoot(t)
				stubInfraControllerPing(t, os.Getpid()+1)
				return cityPath, cfg
			},
		},
		{
			// The same refusal against a root that was never created. Every
			// fact the evidence probe reads is an absence, and under a
			// directory that is not there they all read true at once — which
			// is indistinguishable from the vanished-root case two rows down.
			// So this boot says it could not check instead of guessing, and
			// the operator gets a definite answer on the next boot, once the
			// named reason is resolved and the destination opens.
			name:        "a foreign controller before any copy, with no binding root",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _ := freshCity(t)
				stubInfraControllerPing(t, os.Getpid()+1)
				return cityPath, cfg
			},
		},
		{
			name:        "a work store that will not open before any copy",
			wantOutcome: infraMigrationUnconverged,
			mustRender:  true,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg := freshCityWithRoot(t)
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return nil, os.ErrPermission })
				return cityPath, cfg
			},
		},
		{
			name:        "a work store that will not list before any copy",
			wantOutcome: infraMigrationUnconverged,
			mustRender:  true,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, source := freshCity(t)
				broken := unlistableInfraSource{Store: source, err: fmt.Errorf("database is locked")}
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return broken, nil })
				return cityPath, cfg
			},
		},
		{
			name:        "a copy that imports the rows and then fails",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, source := freshCity(t)
				broken := undeppableInfraSource{Store: source, err: fmt.Errorf("database is locked"), seen: map[string]int{}}
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return broken, nil })
				return cityPath, cfg
			},
		},
		{
			name:        "an equality stage that fails on a mid-copy arrival",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath := t.TempDir()
				growing := &growingInfraSource{Store: beads.NewMemStore(), t: t}
				mustCreateInfraBead(t, growing.Store, beads.Bead{Title: "copied", Type: "session", Labels: []string{"gc:session"}})
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return growing, nil })
				return cityPath, infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			},
		},
		{
			name:        "a manifest that will not rename after copy and equality pass",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _ := freshCity(t)
				failingInfraRename(t, infraCopyManifestName)
				return cityPath, cfg
			},
		},
		{
			// The third round's chain: everything succeeded except the last
			// rename, so the binding holds the whole copy and nothing records
			// it. The outcome is the same one that used to name the revert.
			name:        "a marker that will not rename after copy and equality pass",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _ := freshCity(t)
				failingInfraRename(t, infraMigratedMarkerName)
				return cityPath, cfg
			},
		},
		{
			// The boot after that one, on a city that kept running: the copy is
			// in the binding, the manifest records it, and no marker does.
			name:        "a proven copy with no marker, refused again on the next boot",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _ := freshCity(t)
				var log bytes.Buffer
				if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
					t.Fatalf("seeding the cutover: %v; log: %s", got.Outcome, log.String())
				}
				if err := os.Remove(mustResolveInfraTarget(t, cityPath, cfg).MarkerPath()); err != nil {
					t.Fatal(err)
				}
				stubInfraControllerPing(t, os.Getpid()+1)
				return cityPath, cfg
			},
		},
		{
			// The first unstamped infra write landing in a binding the city was
			// told not to trust but kept serving from anyway.
			name:        "a binding holding a row this migration did not stamp",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _ := freshCity(t)
				target := mustResolveInfraTarget(t, cityPath, cfg)
				mustCreateInfraBead(t, openMigratedDestination(t, target), beads.Bead{Title: "written while unconverged", Type: "session", Labels: []string{"gc:session"}})
				return cityPath, cfg
			},
		},
		{
			name:        "a converged city whose manifest will not read",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _, target := convergedInfraCity(t)
				if err := os.Remove(target.ManifestPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target.ManifestPath(), 0o755); err != nil {
					t.Fatal(err)
				}
				return cityPath, cfg
			},
		},
		{
			name:        "a converged city whose work store will not open",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _, _ := convergedInfraCity(t)
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return nil, os.ErrPermission })
				return cityPath, cfg
			},
		},
		{
			name:        "a converged city whose binding will not open",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _, target := convergedInfraCity(t)
				if err := os.Remove(target.Database); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target.Database, 0o755); err != nil {
					t.Fatal(err)
				}
				return cityPath, cfg
			},
		},
		{
			// A marker whose database is gone. The binding enumerates empty —
			// and it must still not be reverted, because the marker says this
			// city served from it and the volume may simply be absent.
			name:        "a stale marker whose re-copy refuses",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _, target := convergedInfraCity(t)
				if err := os.RemoveAll(target.Dir); err != nil {
					t.Fatal(err)
				}
				stubInfraControllerPing(t, os.Getpid()+1)
				return cityPath, cfg
			},
		},
		{
			// The fourth round, and the one absence-of-everything reads as
			// emptiness: the volume carrying the binding is not mounted, so
			// the marker, the manifest and the database are gone together. All
			// three "no such file" answers are artifacts of one missing
			// directory, and this city's infra state is sitting on that volume
			// intact.
			name:        "a converged city whose binding root has vanished wholesale",
			wantOutcome: infraMigrationUnconverged,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _, target := convergedInfraCity(t)
				if err := os.RemoveAll(target.Root); err != nil {
					t.Fatal(err)
				}
				stubInfraControllerPing(t, os.Getpid()+1)
				return cityPath, cfg
			},
		},
		{
			name:        "a converged city whose binding root is replaced by a file",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, _, target := convergedInfraCity(t)
				if err := os.RemoveAll(target.Root); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target.Root, []byte("not a binding"), 0o644); err != nil {
					t.Fatal(err)
				}
				return cityPath, cfg
			},
		},
		{
			// A root that stats as a directory and answers "no such file" for
			// everything inside it, because this process cannot enter it.
			name:        "a converged city whose binding root cannot be read",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				skipWhenRootIgnoresPermissions(t)
				cityPath, cfg, _, target := convergedInfraCity(t)
				sealInfraDirectory(t, target.Root)
				return cityPath, cfg
			},
		},
		{
			// A bare mountpoint: the directory the volume mounts onto, with
			// the volume absent and its permissions keeping this process out.
			// Nothing about this city's history is visible, so nothing about
			// it is claimed — a mountpoint this process CAN read and finds
			// empty stays indistinguishable from a genesis root, which is
			// stated in infraBindingRootEnumerable rather than papered over.
			name:        "a bare mountpoint this boot can neither read nor write",
			wantOutcome: infraMigrationUncheckable,
			setup: func(t *testing.T) (string, *config.City) {
				skipWhenRootIgnoresPermissions(t)
				cityPath, cfg, _ := freshCity(t)
				root := mustResolveInfraTarget(t, cityPath, cfg).Root
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				sealInfraDirectory(t, root)
				return cityPath, cfg
			},
		},
		{
			name:        "a stranded write on a converged city",
			wantOutcome: infraMigrationStranded,
			setup: func(t *testing.T) (string, *config.City) {
				cityPath, cfg, source, _ := convergedInfraCity(t)
				mustCreateInfraBead(t, source, beads.Bead{Title: "landed after the proof", Type: "session", Labels: []string{"gc:session"}})
				return cityPath, cfg
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := tc.setup(t)

			var log bytes.Buffer
			report := migrateInfraClasses(t, cityPath, cfg, &log)
			if report.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v; log: %s", report.Outcome, tc.wantOutcome, log.String())
			}
			// Recorded from what the boot actually reached, not from what the
			// case intended: a property driven over a table proves only as much
			// as the table still reaches, and a case that quietly stops
			// producing its outcome leaves the space unexplored.
			covered[report.Outcome] = true
			advice := infraMigrationOperatorAdvice(report, "gc start")
			rendered := strings.Contains(log.String()+advice, revert)

			target, resolved, err := resolveInfraBindingTarget(cityPath, cfg)
			if err != nil || !resolved {
				if rendered {
					t.Fatalf("the revert was named for a city whose binding could not even be resolved (%v): %s%s", err, log.String(), advice)
				}
				return
			}
			// The property, asserted against the binding rather than against
			// the path that got here. The root read comes first because it is
			// what makes every absence below it mean anything: a root that is
			// missing, is not a directory, or cannot be entered reports the
			// marker, the manifest and the database as absent all at once, and
			// on an unmounted volume all three of those files exist.
			if rendered {
				if err := infraBindingRootLooked(target); err != nil {
					t.Fatalf("the revert was named for a binding root this boot could not look inside (%v); absence of everything under it is not evidence of emptiness. output: %s%s",
						err, log.String(), advice)
				}
			}
			count, known := infraBindingContents(t, target)
			if rendered && (!known || count != 0) {
				t.Fatalf("the revert was named for a binding this test could not prove empty (known=%v, beads=%d); reverting [storage.classes] abandons every bead in it. output: %s%s",
					known, count, log.String(), advice)
			}
			for _, record := range []struct{ what, path string }{
				{"convergence marker", target.MarkerPath()},
				{"copy manifest", target.ManifestPath()},
			} {
				if _, err := os.Stat(record.path); err == nil && rendered {
					t.Fatalf("the revert was named for a binding carrying a %s (%s), which is its own record of having been in service. output: %s%s",
						record.what, record.path, log.String(), advice)
				}
			}
			// The other direction, on the cases where withholding would itself
			// be the regression: a city whose infra state is still entirely in
			// the work store must be told how to get back to it, or the
			// property is satisfiable by never rendering the sentence at all.
			if tc.mustRender && !rendered {
				t.Fatalf("no revert was offered to a city with an empty binding and its whole infra slice still in the work store. output: %s%s", log.String(), advice)
			}
		})
	}

	for _, outcome := range []infraMigrationOutcome{
		infraMigrationNotConfigured,
		infraMigrationConverged,
		infraMigrationUnconverged,
		infraMigrationStranded,
		infraMigrationUncheckable,
	} {
		if !covered[outcome] {
			t.Errorf("no case in this table reaches the %v outcome, so the property is unproven there", outcome)
		}
	}
}

// TestEnsureInfraClassMigratedSeparatesUnverifiableFromUnconverged is the whole
// distinction, proved in all three states against one city shape.
//
// "This city never converged" and "this boot could not check" are different
// claims with opposite instructions, and conflating them is not a cosmetic
// error: a transient fault — a busy store, a momentary permission problem, a
// manifest that would not read — would hand a converged city the revert, which
// re-points it at a stale source and abandons every infra bead written since
// cutover. The marker is what tells them apart, and it is durable: a store that
// will not open right now does not retract the proof that the copy completed.
func TestEnsureInfraClassMigratedSeparatesUnverifiableFromUnconverged(t *testing.T) {
	revert := fmt.Sprintf(infraRevertInstruction, config.StorageWorkBinding)

	t.Run("genuinely unconverged is blocked and keeps the revert", func(t *testing.T) {
		cityPath := t.TempDir()
		cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
		source := stubInfraMigrationSource(t)
		mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
		// A binding root that exists and holds nothing. Without it the boot has
		// not looked at a binding at all — it has been handed a path that is
		// not there, which an unmounted volume produces just as readily as a
		// city that never cut over — and it withholds the revert on those
		// grounds instead.
		if err := os.MkdirAll(mustResolveInfraTarget(t, cityPath, cfg).Root, 0o755); err != nil {
			t.Fatal(err)
		}
		stubInfraControllerPing(t, os.Getpid()+1)

		var log bytes.Buffer
		got := migrateInfraClasses(t, cityPath, cfg, &log)
		if got.Outcome != infraMigrationUnconverged {
			t.Fatalf("outcome = %v, want unconverged; log: %s", got.Outcome, log.String())
		}
		// The fact that licenses the revert, asserted rather than assumed.
		target := mustResolveInfraTarget(t, cityPath, cfg)
		if _, err := os.Stat(target.MarkerPath()); !os.IsNotExist(err) {
			t.Fatalf("this city has a convergence marker, so it is not the unconverged case: %v", err)
		}
		if advice := infraMigrationOperatorAdvice(got, "gc start"); !strings.Contains(advice, revert) {
			t.Fatalf("a city whose infra state is still in the work store was not told how to get back to it: %q", advice)
		}
	})

	t.Run("an unresolvable destination decides nothing", func(t *testing.T) {
		cityPath := t.TempDir()
		// A regular file where a directory has to be, so the provider's own
		// path resolver fails with ENOTDIR. Nothing here can then even look
		// for the marker that says whether this city cut over — which is
		// exactly why it must not answer as though it had looked.
		blocker := filepath.Join(cityPath, "not-a-directory")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := infraSplitConfig(filepath.Join(blocker, "store"))
		refuseInfraMigrationSource(t)

		var log bytes.Buffer
		got := migrateInfraClasses(t, cityPath, cfg, &log)
		if got.Outcome != infraMigrationUncheckable {
			t.Fatalf("outcome = %v, want uncheckable; an unresolved destination is not evidence about the city. log: %s", got.Outcome, log.String())
		}
		if log.Len() == 0 {
			t.Fatal("the destination could not be resolved and nothing said so")
		}
		if advice := infraMigrationOperatorAdvice(got, "gc start"); strings.Contains(advice, revert) {
			t.Fatalf("a city nothing could resolve was offered the revert: %q", advice)
		}
	})

	t.Run("converged and verifiable is silent", func(t *testing.T) {
		cityPath, cfg, _, _ := convergedInfraCity(t)

		var log bytes.Buffer
		got := migrateInfraClasses(t, cityPath, cfg, &log)
		if got.Outcome != infraMigrationConverged {
			t.Fatalf("outcome = %v, want converged; log: %s", got.Outcome, log.String())
		}
		if log.Len() != 0 {
			t.Fatalf("a healthy converged city reported activity: %s", log.String())
		}
		if advice := infraMigrationOperatorAdvice(got, "gc start"); advice != "" {
			t.Fatalf("the boot path added an instruction to a city with nothing wrong: %q", advice)
		}
	})

	// Each fault leaves a converged, in-service city and breaks only the ability
	// to re-read it. Every one of them reached the revert before this fix.
	for _, tc := range []struct {
		name string
		// fault breaks the re-check and returns the substring the report must
		// name, so an operator is told which read failed rather than that
		// "something" did.
		fault func(t *testing.T, target infraBindingTarget, source beads.Store) string
	}{
		{
			name: "the work store will not open",
			fault: func(t *testing.T, _ infraBindingTarget, _ beads.Store) string {
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return nil, os.ErrPermission })
				return "opening work store"
			},
		},
		{
			name: "the work store will not list",
			fault: func(t *testing.T, _ infraBindingTarget, source beads.Store) string {
				broken := unlistableInfraSource{Store: source, err: fmt.Errorf("database is locked")}
				failInfraMigrationSourceWith(t, func(string) (beads.Store, error) { return broken, nil })
				return "listing work store"
			},
		},
		{
			name: "the copy manifest will not read",
			fault: func(t *testing.T, target infraBindingTarget, _ beads.Store) string {
				if err := os.Remove(target.ManifestPath()); err != nil {
					t.Fatal(err)
				}
				// A directory where the manifest was: a read error, not absence,
				// which is the case the missing-manifest path does not cover.
				if err := os.Mkdir(target.ManifestPath(), 0o755); err != nil {
					t.Fatal(err)
				}
				return target.ManifestPath()
			},
		},
		{
			name: "the binding database will not open",
			fault: func(t *testing.T, target infraBindingTarget, _ beads.Store) string {
				if err := os.Remove(target.Database); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target.Database, 0o755); err != nil {
					t.Fatal(err)
				}
				return "opening binding"
			},
		},
		{
			// The marker outlives its database, and it still means what it
			// says about the past: this city cut over and served from the
			// binding. A re-copy that then refuses leaves the same two
			// possibilities as every other fault here — the database may be a
			// volume that has not come back yet — so the revert stays
			// withheld rather than being licensed by the missing file.
			name: "the database is gone and the re-copy refuses",
			fault: func(t *testing.T, target infraBindingTarget, _ beads.Store) string {
				if err := os.RemoveAll(target.Dir); err != nil {
					t.Fatal(err)
				}
				stubInfraControllerPing(t, os.Getpid()+1)
				return "is live on this city"
			},
		},
		{
			name: "the binding database will not stat",
			fault: func(t *testing.T, target infraBindingTarget, _ beads.Store) string {
				if err := os.RemoveAll(target.Dir); err != nil {
					t.Fatal(err)
				}
				// A regular file where the component directory was: stat of the
				// database below it fails with ENOTDIR, which is not absence.
				if err := os.WriteFile(target.Dir, []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
				return "reading the binding database"
			},
		},
	} {
		t.Run("converged but unverifiable degrades: "+tc.name, func(t *testing.T) {
			cityPath, cfg, source, target := convergedInfraCity(t)
			markerBefore, err := os.ReadFile(target.MarkerPath())
			if err != nil {
				t.Fatal(err)
			}
			names := tc.fault(t, target, source)

			var log bytes.Buffer
			got := migrateInfraClasses(t, cityPath, cfg, &log)
			if got.Outcome != infraMigrationUncheckable {
				t.Fatalf("outcome = %v, want uncheckable; a fault in the CHECK was reported as a state of the CITY. log: %s", got.Outcome, log.String())
			}
			advice := infraMigrationOperatorAdvice(got, "gc start")
			if reported := log.String() + advice; strings.Contains(reported, revert) {
				t.Fatalf("a converged city was told to revert [storage.classes] because one read failed; that revert abandons every infra bead written since cutover: %s", reported)
			}
			if !strings.Contains(log.String(), names) {
				t.Fatalf("the degraded report does not name the read that failed (want %q): %s", names, log.String())
			}
			if advice == "" {
				t.Fatal("the boot path said nothing at all about a city whose binding it could not verify")
			}
			// The marker is the durable evidence the whole distinction rests on.
			// A check that cannot run must not disturb it, or the next boot loses
			// the ability to tell these two cities apart at all.
			if after, err := os.ReadFile(target.MarkerPath()); err != nil || !bytes.Equal(after, markerBefore) {
				t.Fatalf("the failed re-check disturbed the convergence marker: %q -> %q (%v)", markerBefore, after, err)
			}
		})
	}
}

// TestEnsureInfraClassMigratedConvergenceCheckAcceptsBindingGrowth guards the
// direction of the containment check. After cutover the binding is the live
// infra store and legitimately mints beads the retained source never had.
// Comparing for equality rather than containment would read normal operation
// as data loss and block every boot of a working city.
func TestEnsureInfraClassMigratedConvergenceCheckAcceptsBindingGrowth(t *testing.T) {
	cityPath := t.TempDir()
	storeDir := filepath.Join(cityPath, ".gc", "store")
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})

	cfg := infraSplitConfig(storeDir)
	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("first boot outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}

	target := mustResolveInfraTarget(t, cityPath, cfg)
	mustCreateInfraBead(t, openMigratedDestination(t, target), beads.Bead{Title: "minted after cutover", Type: "session", Labels: []string{"gc:session"}})

	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("outcome = %v, want converged; post-cutover binding growth was read as loss. log: %s", got.Outcome, log.String())
	}
	if log.Len() != 0 {
		t.Fatalf("a healthy converged city reported activity: %s", log.String())
	}
}
