package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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

// TestPreflightPublishesNoEvent keeps a diagnostic out of the verdict stream.
//
// storage.binding.* events are what a deploy gate reads to decide whether a
// city is serving. Preflight reaches no such verdict — it reports what a
// migration WOULD find — and publishing one would let a command an operator ran
// to plan a window answer a question they did not ask. Opening the recorder is
// itself a write: it appends to the city's .gc/events.jsonl.
func TestPreflightPublishesNoEvent(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 1)

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflightWithRecorder(request, rec, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight refused a ready city: exit %d stderr=%q", code, stderr.String())
	}
	if len(rec.Events) != 0 {
		t.Errorf("preflight published %+v; a diagnostic must not inject a serving verdict into the stream a deploy gate reads", rec.Events)
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
	if !strings.Contains(stdout.String(), "controller: none") {
		t.Errorf("preflight reports nothing about the controller when none is live, so its silence in the live case would be indistinguishable from a check that never ran: %q", stdout.String())
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
// A converged city's migration is a no-op that exits zero, so preflight clears
// it — but clearing it silently would read as "your cutover is pending and will
// go fine", which is the opposite of the truth. It says the cutover already
// happened and points at the read-only report that describes it.
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
