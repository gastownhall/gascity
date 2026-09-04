package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// errStorageTestSourceUnreadable stands in for whatever makes a work store
// refuse to open — a permission fault, a corrupt file, a volume that went away.
var errStorageTestSourceUnreadable = errors.New("the work store is unreadable in this test")

// preflightReadyCity returns a city configured for the split, with a stubbed
// work store holding count infrastructure beads and no controller live — the
// state an operator is in between authoring the config and taking the window.
func preflightReadyCity(t *testing.T, count int) (storageOperatorRequest, *config.City, string) {
	t.Helper()
	bindingParent := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(bindingParent, "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	stubInfraControllerPing(t, 0)
	for i := 0; i < count; i++ {
		mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	}
	return request, cfg, bindingParent
}

// TestPreflightClearsACityThatIsReadyToMigrate is the happy path, and the
// reason the command exists: an operator wants to know whether the window they
// are about to take will be spent migrating or spent reading a refusal.
func TestPreflightClearsACityThatIsReadyToMigrate(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 3)

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a city with nothing wrong: exit %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), storageMigrationCommand) {
		t.Errorf("a cleared preflight does not name the command it cleared, so the operator's next step is not in the output that told them to take it: %q", stdout.String())
	}
}

// TestPreflightReportsTheSizeOfTheCopyItWouldRun puts a number on the window.
//
// "Ready" says the migration will not refuse; it says nothing about how long
// the operator's city will be stopped. The source census is the one number that
// bears on that, and preflight has already opened the source to take it.
//
// Two sizes, because one size cannot tell a census from a constant.
func TestPreflightReportsTheSizeOfTheCopyItWouldRun(t *testing.T) {
	for _, count := range []int{1, 5} {
		t.Run(fmt.Sprintf("%d to copy", count), func(t *testing.T) {
			request, _, _ := preflightReadyCity(t, count)
			var stdout, stderr bytes.Buffer
			if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
				t.Fatalf("preflight refused a ready city: exit %d stderr=%q", code, stderr.String())
			}
			want := fmt.Sprintf("would copy %d infrastructure bead(s)", count)
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("preflight does not report the size of the copy it cleared; want %q in %q", want, stdout.String())
			}
		})
	}
}

// TestPreflightCreatesNothing is the read-only contract, and it is the whole
// reason this is a separate verb rather than `migrate --dry-run`.
//
// The migration's own destination opener CREATES the database — that is what it
// is for — so a dry run sharing the migrate body would have to fork it anyway.
// A verb that cannot reach the writing path at all is the only version of this
// an operator can run against a city they have not decided to cut over yet.
func TestPreflightCreatesNothing(t *testing.T) {
	request, cfg, bindingParent := preflightReadyCity(t, 2)

	beforeBinding := treeFingerprint(t, bindingParent)
	beforeCity := treeFingerprint(t, request.CityPath)
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a ready city: exit %d stderr=%q", code, stderr.String())
	}
	if got := treeFingerprint(t, bindingParent); !equalStrings(beforeBinding, got) {
		t.Errorf("preflight changed the binding tree it was asked about:\n before %v\n after  %v", beforeBinding, got)
	}
	if got := treeFingerprint(t, request.CityPath); !equalStrings(beforeCity, got) {
		t.Errorf("preflight changed the city:\n before %v\n after  %v", beforeCity, got)
	}
	target := mustResolveInfraTarget(t, request.CityPath, cfg)
	for _, path := range []string{target.Database, target.MarkerPath(), target.ManifestPath()} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("preflight created %s", path)
		}
	}
}

// TestPreflightLeavesNothingBehindWhenItOpensTheDestination covers the one
// path where the read-only claim is not obviously true.
//
// Every other check reads files or a store the city already serves. The
// destination check opens a bead engine. TestPreflightCreatesNothing never
// reaches that code — its destination does not exist, so the open is skipped —
// which means "creates nothing" was asserted only on the path that creates
// nothing trivially. Here the database is already there and the open happens,
// so the fingerprint is the claim: nothing the engine does while answering the
// question may touch what the destination already holds.
// assertReadOnlyDiagnosticResidue is that claim, and its doc comment carries
// why the WAL and SHM sidecars are the one thing a mode=ro connection may leave
// behind.
func TestPreflightLeavesNothingBehindWhenItOpensTheDestination(t *testing.T) {
	request, cfg, bindingParent := preflightReadyCity(t, 1)
	target := mustResolveInfraTarget(t, request.CityPath, cfg)
	destination, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the destination to populate it: %v", err)
	}
	mustCreateInfraBead(t, destination, beads.Bead{Title: "a bead some other owner put here", Type: "session"})
	if err := closeBeadStoreHandle(destination); err != nil {
		t.Fatalf("closing the populated destination: %v", err)
	}

	before := treeFingerprint(t, bindingParent)
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatalf("preflight cleared a populated destination, so it never opened it and this proves nothing: %q", stdout.String())
	}
	// The exit code alone would be satisfied by any earlier check blocking
	// first, which would leave the open unreached and the fingerprint vacuous.
	if !strings.Contains(stdout.String(), "[BLOCK] destination") {
		t.Fatalf("something other than the destination check blocked, so the open never happened: %q", stdout.String())
	}
	assertReadOnlyDiagnosticResidue(t, before, treeFingerprint(t, bindingParent), target.Database)
}

// TestPreflightPublishesNoEvent keeps a diagnostic out of the verdict stream.
//
// storage.binding.* events are what a deploy gate reads to decide whether a
// city is serving. Preflight reaches no such verdict — it reports what a
// migration WOULD find — and publishing one would let a command an operator ran
// to plan a window answer a question they did not ask.
//
// The assertion is against .gc/events.jsonl, not against an injected fake.
// Production never takes a recorder from a caller: the migrate path builds its
// own with openCityRecorderAt, so a fake handed to the preflight body would sit
// unused whether or not the command published, and pass either way. The control
// runs that seam on the same fixture first, because "the file is not there" is
// only evidence once the file has been shown to appear when something writes.
func TestPreflightPublishesNoEvent(t *testing.T) {
	control, _, _ := preflightReadyCity(t, 1)
	controlLog := filepath.Join(control.CityPath, ".gc", "events.jsonl")
	recordStorageBindingOutcome(
		openCityRecorderAt(control.CityPath, io.Discard),
		infraMigrationReport{Outcome: infraMigrationConverged},
		"")
	if _, err := os.Stat(controlLog); err != nil {
		t.Fatalf("the seam this test watches does not write where it expects, so its negative proves nothing: %v", err)
	}

	request, _, _ := preflightReadyCity(t, 1)
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a ready city: exit %d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(request.CityPath, ".gc", "events.jsonl")); !os.IsNotExist(err) {
		t.Errorf("preflight wrote the city's event log (stat err %v); a diagnostic must not inject a serving verdict into the stream a deploy gate reads", err)
	}
}

// TestPreflightTakesNoMigrationGuard is the property that separates a read-only
// check from a cheap migration.
//
// The guard is exclusive. A preflight that took it would make a real migration
// started a moment later refuse with "another storage migration holds this
// city" — so the command an operator runs to find out whether they can migrate
// would be the reason they could not. Both directions are asserted: preflight
// runs while a migrator holds the city, and a migrator can take the guard
// straight after a preflight.
func TestPreflightTakesNoMigrationGuard(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)

	held, err := storebinding.AcquireMigrationGuard(context.Background(), cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if err != nil {
		t.Fatalf("taking the guard: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused while a migrator held the city, so a read-only check is excluded by a lock it does not need: exit %d stderr=%q", code, stderr.String())
	}
	if err := held.Release(); err != nil {
		t.Fatalf("releasing the guard: %v", err)
	}

	after, err := storebinding.AcquireMigrationGuard(context.Background(), cityMigrationGuardDirectory(request.CityPath), storageMigrationGeneration)
	if err != nil {
		t.Fatalf("a migrator could not take the guard after a preflight, so the preflight left a lock behind: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatalf("releasing the second guard: %v", err)
	}
}

// TestPreflightReportsALiveControllerWithoutBlocking is the one refusal
// preflight deliberately reports as informational.
//
// Every other check names something the operator must go and fix. A live
// controller names the thing they are about to do anyway — take the window —
// and blocking on it would mean the command for planning a window could only be
// run from inside one. So it is reported by PID, and the exit code stays clear.
func TestPreflightReportsALiveControllerWithoutBlocking(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)
	stubInfraControllerPing(t, 4242)

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight blocked on a live controller, so it can only be run from inside the window it exists to plan: exit %d stdout=%q", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "4242") {
		t.Errorf("preflight does not name the live controller's PID: %q", out)
	}
	if !strings.Contains(out, storageStopCommand) {
		t.Errorf("preflight names a live controller without naming the command that stops it: %q", out)
	}
}

// TestPreflightSaysSoWhenNoControllerIsLive is the control the case above is
// worthless without: a preflight that never mentioned the controller would pass
// every assertion there while reporting nothing.
func TestPreflightSaysSoWhenNoControllerIsLive(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a ready city: exit %d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "4242") {
		t.Fatalf("the fixture leaked a PID: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "controller: nothing answered") {
		t.Errorf("preflight reports nothing about the controller when none is live, so its silence in the live case would be indistinguishable from a check that never ran: %q", stdout.String())
	}
	// The probe returns 0 for an unreachable socket as well as an empty one, so
	// this line must report the observation rather than assert the absence.
	if strings.Contains(stdout.String(), "none is live") {
		t.Errorf("preflight asserts no controller is live from a probe that cannot tell that from a failed ping: %q", stdout.String())
	}
}

// TestPreflightBlocksOnRigResidueByName covers the expensive check, which is
// the one preflight most earns its place on.
//
// A bead in a rig scope is refused by the migration BY NAME and cannot be
// repaired by any command this binary carries — the operator has to move rows
// by hand. Finding that out inside a stopped-city window is the worst possible
// time, and it is exactly what this verb exists to move earlier.
func TestPreflightBlocksOnRigResidueByName(t *testing.T) {
	rigPath := t.TempDir()
	request, cfg, _ := preflightReadyCity(t, 1)
	cfg.Rigs = []config.Rig{{Name: "alpha", Prefix: "ga", Path: rigPath}}

	rig := beads.NewMemStore()
	stray := mustCreateInfraBead(t, rig, beads.Bead{Title: "a session in a rig", Type: "session"})
	mustCreateInfraBead(t, rig, beads.Bead{Title: "ordinary rig work", Type: "task"})
	prev := openStorageScopeStore
	openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
		if storePath == rigPath {
			return rig, nil
		}
		return prev(storePath, cityPath)
	}
	t.Cleanup(func() { openStorageScopeStore = prev })

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatal("preflight cleared a city whose migration would refuse by name")
	}
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, stray.ID) || !strings.Contains(out, "rig alpha") {
		t.Errorf("preflight blocks without naming the bead and its rig, so it says a migration would fail without saying what to move: %q", out)
	}
}

// TestPreflightBlocksOnATopologyThisBuildCannotServe mirrors the migration's
// first refusal.
//
// A half-split is refused before anything else is looked at, because a plan
// boot would not serve must not be migrated toward. Preflight reports the same
// refusal in the same place, and clearing it here would be worse than useless:
// it would send an operator into a window to run a command that refuses in its
// first step.
func TestPreflightBlocksOnATopologyThisBuildCannotServe(t *testing.T) {
	request, cfg, _ := preflightReadyCity(t, 1)
	cfg.Storage.Classes.Nudges = config.StorageWorkBinding

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatal("preflight cleared a half-split this build refuses to serve")
	}
	if !strings.Contains(stdout.String()+stderr.String(), storageSupportedTopologyStatement) {
		t.Errorf("preflight blocks on the topology without stating which topologies this build serves: %q", stdout.String()+stderr.String())
	}
}

// TestPreflightReportsAnAlreadyConvergedCity keeps the verb honest about the
// one city it has nothing to say about.
//
// The marker means the migration would not copy, so preflight clears it — but
// clearing it silently would read as "your cutover is pending and will go
// fine", which is the opposite of the truth. It says the cutover already
// happened and points at the read-only report that describes it, which is where
// the question this step does not ask — whether that converged city has
// stranded writes — is answered.
func TestPreflightReportsAnAlreadyConvergedCity(t *testing.T) {
	request, cfg, _ := preflightReadyCity(t, 2)

	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture migration reported %s: %s", got.Outcome, log.String())
	}

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a converged city, whose migration would exit zero: exit %d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "already converged") {
		t.Errorf("preflight does not say the cutover already happened, so a cleared report reads as a pending cutover that will go fine: %q", out)
	}
	if !strings.Contains(out, storageStatusInstruction()) {
		t.Errorf("preflight tells an operator their city is converged without naming the command that describes it: %q", out)
	}
}

// TestPreflightAndStatusAnswerDifferentQuestions pins the exit codes apart.
//
// They collide on exactly the city that matters: configured for a binding, not
// yet cut over, nothing wrong. `status` exits 1 there because it is the deploy
// gate and that city is not serving. Preflight exits 0 because the migration
// would run. A preflight that shared the gate's contract would be a status
// command with extra words.
func TestPreflightAndStatusAnswerDifferentQuestions(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 2)

	var preStdout, preStderr bytes.Buffer
	preflight := doStoragePreflight(request, &preStdout, &preStderr)
	var statusStdout, statusStderr bytes.Buffer
	status := doStorageStatus(request, &statusStdout, &statusStderr)

	if status == 0 {
		t.Fatalf("the fixture is not the unconverged city this test needs: status exited 0: %q", statusStdout.String())
	}
	if preflight != 0 {
		t.Errorf("preflight exited %d on a city whose migration would run, which is status's contract rather than its own: %q", preflight, preStdout.String())
	}
}

// TestPreflightNamesTheAttestationItCannotCheck closes the gap between what
// preflight proves and what the migration will ask for.
//
// --fleet-stopped is an operator attestation precisely because no process can
// check it. A preflight that cleared a city without saying so would leave the
// operator believing every condition had been verified, and the one that was
// not is the one that strands writes.
func TestPreflightNamesTheAttestationItCannotCheck(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a ready city: exit %d stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, storageFleetStoppedFlag) || !strings.Contains(out, storageFleetStoppedAttestation) {
		t.Errorf("preflight clears a city without naming the one condition it cannot check, so its clearance reads as broader than it is: %q", out)
	}
}

// TestPreflightBlocksWhenTheWorkStoreCannotBeRead refuses to clear a city on
// evidence it never gathered.
//
// The source census is what "the migration would copy N beads" rests on. A read
// that failed leaves the whole clearance unfounded — nothing is known about
// what the copy would carry, or whether the copy could run at all — so it
// blocks rather than reporting zero, which is the same positive-looking absence
// the boot path refuses everywhere else.
func TestPreflightBlocksWhenTheWorkStoreCannotBeRead(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return nil, errStorageTestSourceUnreadable }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatal("preflight cleared a city whose work store it could not open")
	}
	if !strings.Contains(stdout.String()+stderr.String(), errStorageTestSourceUnreadable.Error()) {
		t.Errorf("preflight blocks without naming the read that failed: %q", stdout.String()+stderr.String())
	}
}

// TestStoragePreflightIsReachableFromTheCommandTree proves the verb is wired,
// not merely written. A function nobody can invoke is not an operator command.
func TestStoragePreflightIsReachableFromTheCommandTree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStorageCmd(&stdout, &stderr)
	found, _, err := cmd.Find([]string{storagePreflightVerb})
	if err != nil {
		t.Fatalf("resolving `gc storage %s`: %v", storagePreflightVerb, err)
	}
	if found.Name() != storagePreflightVerb {
		t.Fatalf("`gc storage %s` resolved to %q", storagePreflightVerb, found.Name())
	}
	if found.Short == "" {
		t.Error("the preflight verb has no Short, so `gc storage --help` lists it blank")
	}
}

// TestPreflightRefusesEveryCityTheMigrationRefuses is the drift guard for this
// verb's whole reason to exist.
//
// The file header claims every migration refusal has a line in the preflight.
// A comment cannot hold that claim — the first cut of the file made it while
// omitting three of them, and every preflight test passed, because each one
// asserts a check that IS there. Only running both legs over the same city
// catches a check that is not.
//
// Each fixture is built twice, once per leg, because the migration leg writes
// to the city it is handed. The migration leg is also the control: a fixture
// that fails to make the migration refuse would make the preflight assertion
// vacuous, so it fails the test rather than passing quietly.
//
// One migration refusal is deliberately absent from this table and cannot be
// added to it: infraCopyDepEdge refuses a DESTINATION that cannot carry an edge
// payload. The answer lives on the destination, and opening the destination is
// the one thing this rehearsal will not do — the opener creates the database, so
// a preflight that asked would answer its own question. It is the same class as
// the migration's ForeignIDCreator assertion, and safe for the same reason: in
// production openInfraDestination returns a *beads.SQLiteStore, which carries a
// compile-time beads.DepMetadataWriter assertion in sqlite_store_graph_apply.go.
// A destination that could reach an operator without one does not exist, so
// there is no window this omission could cost.
func TestPreflightRefusesEveryCityTheMigrationRefuses(t *testing.T) {
	cases := map[string]func(t *testing.T) (storageOperatorRequest, *config.City){
		"a served-binding note names another binding": func(t *testing.T) (storageOperatorRequest, *config.City) {
			request, cfg, _ := preflightReadyCity(t, 1)
			if err := writeBornSplitServedNote(request.CityPath, bornSplitServedNote{
				Binding:  "elsewhere",
				Provider: config.StorageProviderSQLiteBeads,
				Location: filepath.Join(t.TempDir(), "already-serving.db"),
			}); err != nil {
				t.Fatalf("writing the served-binding note: %v", err)
			}
			return request, cfg
		},
		"the source cannot be asked about edge payloads at all": func(t *testing.T) (storageOperatorRequest, *config.City) {
			request, cfg, _ := preflightReadyCity(t, 0)
			backing, _, _, _ := seedInfraEdgeSource(t)
			openInfraMigrationSource = func(string) (beads.Store, error) { return mutePayloadSource{Store: backing}, nil }
			return request, cfg
		},
		// The source answers the type assertion and then FAILS the read. This
		// is the fixture that makes infraSourceEdgePayloadRefusal's per-edge
		// loop load-bearing: with only the mute case above, deleting the loop
		// and keeping the assertion leaves every test in the tree green while
		// preflight starts clearing a city the migration refuses — the single
		// thing this table exists to prevent.
		"the source's edge-payload read fails": func(t *testing.T) (storageOperatorRequest, *config.City) {
			request, cfg, _ := preflightReadyCity(t, 0)
			backing, _, _, _ := seedInfraEdgeSource(t)
			source := &payloadCarryingSource{Store: backing, readErr: errors.New("the dependencies table is unreadable")}
			openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
			return request, cfg
		},
		"the destination already holds beads this migration did not write": func(t *testing.T) (storageOperatorRequest, *config.City) {
			request, cfg, _ := preflightReadyCity(t, 1)
			target := mustResolveInfraTarget(t, request.CityPath, cfg)
			destination, err := openInfraDestination(target)
			if err != nil {
				t.Fatalf("opening the destination to populate it: %v", err)
			}
			mustCreateInfraBead(t, destination, beads.Bead{Title: "a bead some other owner put here", Type: "session"})
			if err := closeBeadStoreHandle(destination); err != nil {
				t.Fatalf("closing the populated destination: %v", err)
			}
			return request, cfg
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			migrateRequest, migrateCfg := build(t)
			var said bytes.Buffer
			report := migrateInfraClasses(t, migrateRequest.CityPath, migrateCfg, &said)
			if report.Outcome == infraMigrationConverged {
				t.Fatalf("this fixture does not make the migration refuse, so it proves nothing about preflight: %s", said.String())
			}

			preflightRequest, _ := build(t)
			var stdout, stderr bytes.Buffer
			if code := doStoragePreflight(preflightRequest, &stdout, &stderr); code == 0 {
				t.Fatalf("preflight cleared a city the migration refuses with %v, which is the one thing this verb must never do.\nmigration said: %s\npreflight said: %s",
					report.Outcome, said.String(), stdout.String())
			}
		})
	}

	// The control both directions of the loop above are worthless without: a
	// preflight that blocked unconditionally would satisfy every case.
	t.Run("control: a city the migration accepts", func(t *testing.T) {
		migrateRequest, migrateCfg, _ := preflightReadyCity(t, 1)
		var said bytes.Buffer
		if report := migrateInfraClasses(t, migrateRequest.CityPath, migrateCfg, &said); report.Outcome != infraMigrationConverged {
			t.Fatalf("the control city does not migrate (%v), so the refusal cases above prove nothing: %s", report.Outcome, said.String())
		}
		preflightRequest, _, _ := preflightReadyCity(t, 1)
		var stdout, stderr bytes.Buffer
		if code := doStoragePreflight(preflightRequest, &stdout, &stderr); code != 0 {
			t.Fatalf("preflight refused a city the migration accepts:\n%s", stdout.String())
		}
	})
}

// TestPreflightCensusesRigScopesEvenOnAConvergedCity pins the one ordering that
// matters, against the one city that hides it.
//
// A converged city's migration is a no-op, so preflight reports it and returns
// early. But the rig census runs BEFORE the marker is read — in the migration it
// runs before the migration body is entered at all — and a converged city can
// still accumulate an infrastructure bead in a rig scope afterwards. Reading the
// marker first would clear that city while `gc storage migrate` refuses it by
// name, which is the exact divergence this verb exists to prevent.
func TestPreflightCensusesRigScopesEvenOnAConvergedCity(t *testing.T) {
	rigPath := t.TempDir()
	request, cfg, _ := preflightReadyCity(t, 2)

	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture city did not converge, so this proves nothing about a converged one: %s: %s", got.Outcome, log.String())
	}

	// The residue lands after the cutover, which is the only way this city can
	// exist: the migration would have refused it otherwise.
	cfg.Rigs = []config.Rig{{Name: "alpha", Prefix: "ga", Path: rigPath}}
	rig := beads.NewMemStore()
	stray := mustCreateInfraBead(t, rig, beads.Bead{Title: "a session written after the cutover", Type: "session"})
	prev := openStorageScopeStore
	openStorageScopeStore = func(storePath, cityPath string) (beads.Store, error) {
		if storePath == rigPath {
			return rig, nil
		}
		return prev(storePath, cityPath)
	}
	t.Cleanup(func() { openStorageScopeStore = prev })

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatalf("preflight cleared a converged city holding infrastructure residue in a rig scope, which the migration refuses by name: %q", stdout.String())
	}
	if out := stdout.String() + stderr.String(); !strings.Contains(out, stray.ID) {
		t.Errorf("preflight blocks without naming the stray bead: %q", out)
	}
}

// TestPreflightBlockIndentsAWrappedRefusalUnderTheDetailColumn covers the
// layout branch no current refusal reaches.
//
// These messages come from the migration, where they are read on their own
// rather than inside a column, so one of them growing a newline is a change to
// a different file that silently reshapes this report. Left at the margin, the
// wrapped lines read as further checks — a report whose entire job is "which of
// these would stop me" would appear to list checks that do not exist.
func TestPreflightBlockIndentsAWrappedRefusalUnderTheDetailColumn(t *testing.T) {
	var stdout bytes.Buffer
	if code := preflightBlock(&stdout, "destination", errors.New("first line\nsecond line")); code == 0 {
		t.Fatal("preflightBlock returned a clearing exit code")
	}
	lines := strings.Split(stdout.String(), "\n")
	if len(lines) < 2 {
		t.Fatalf("preflightBlock printed no continuation: %q", stdout.String())
	}
	if !strings.HasPrefix(lines[0], "  "+preflightBlockTag) {
		t.Fatalf("first line is not a check row: %q", lines[0])
	}
	// Both details must start at the same column, which is the whole claim.
	if got, want := strings.Index(lines[0], "first line"), len(preflightDetailIndent); got != want {
		t.Fatalf("the detail column is at %d but the continuation indent is %d wide, so a wrapped line lands out of alignment", got, want)
	}
	if lines[1] != preflightDetailIndent+"second line" {
		t.Errorf("continuation line = %q, want it indented to the detail column", lines[1])
	}
}
