package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// resetCLIStorageRoutes gives a test the process-scoped funnel in its start
// state and closes whatever the test opens through it.
func resetCLIStorageRoutes(t *testing.T) {
	t.Helper()
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing routes left over from an earlier test: %v", err)
	}
	t.Cleanup(func() { _ = closeCLIStorageRoutes() })
}

// captureCLIStorageStderr points the one-shot gate's diagnostics somewhere the
// test can read, so a refusal's reasons do not land on the real terminal.
func captureCLIStorageStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := cliStorageStderr
	cliStorageStderr = &buf
	t.Cleanup(func() { cliStorageStderr = prev })
	return &buf
}

// writeOneShotCityTOML writes a city.toml, optionally carrying the whole-split
// [storage] section pointed at bindingRoot. An empty bindingRoot writes a city
// that authors no [storage] at all.
func writeOneShotCityTOML(t *testing.T, cityPath, bindingRoot string) {
	t.Helper()
	body := "[workspace]\nname = \"storage-cli-city\"\n"
	if bindingRoot != "" {
		body += fmt.Sprintf(`
[storage.classes]
work = %q
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = %q
path = %q
`, config.StorageWorkBinding, config.StorageProviderSQLiteBeads, bindingRoot)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// oneShotCLICity prepares a city an actual one-shot command can resolve and
// open: discovery finds it, and its work store is the ordinary file backend.
// bindingRoot is the whole-split binding, or "" for a city with no [storage].
func oneShotCLICity(t *testing.T, bindingRoot string) string {
	t.Helper()
	clearGCEnv(t)
	cityPath := t.TempDir()
	writeOneShotCityTOML(t, cityPath, bindingRoot)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", cityPath)
	resetCLIStorageRoutes(t)
	return cityPath
}

// migratedOneShotCLICity returns a city that has converged onto its binding,
// exactly as `gc storage migrate --from-work` leaves one.
//
// The work store is deliberately NOT stubbed: the claim these tests make is
// that a routed write is absent from the ledger the class was moved off, and a
// fake source cannot carry that claim.
func migratedOneShotCLICity(t *testing.T) (cityPath string, cfg *config.City) {
	t.Helper()
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cityPath = oneShotCLICity(t, bindingRoot)
	stubInfraControllerPing(t, 0)

	var err error
	cfg, err = loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading the split city config: %v", err)
	}
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture city did not converge (%s): %s", report.Outcome, log.String())
	}
	return cityPath, cfg
}

// TestCLIStorageRoutesBypassACityWithNoStorageSection is the compatibility
// contract for the one-shot tier, and it is the same NEGATIVE the boot gate
// asserts: a city that authors no [storage] constructs no provider registry,
// resolves no plan, and reads nothing from a work store to census.
//
// The counting constructor is what makes it a real assertion. "The routes came
// back nil" would still pass if the funnel had built a registry, resolved a plan
// and thrown both away, and every one of those steps is a refusal mode a city
// with no [storage] must not acquire by upgrading its binary.
func TestCLIStorageRoutesBypassACityWithNoStorageSection(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	registries := countStorageRegistryConstructions(t)
	refuseInfraMigrationSource(t)
	stderr := captureCLIStorageStderr(t)

	if routes := cliStorageRoutes(cityPath); routes != nil {
		t.Fatalf("a city with no [storage] resolved routes %+v, want none", routes)
	}

	// Through the production call site, not just the funnel: the class resolver
	// must hand back the EXACT store value it was given, because every one-shot
	// caller asserts optional capabilities on whatever comes back.
	work := beads.NewMemStore()
	if got := cliSessionStore(work, nil, cityPath); got != beads.Store(work) {
		t.Errorf("the one-shot session route returned %p, want the work store %p", got, work)
	}
	if *registries != 0 {
		t.Errorf("the no-config CLI path constructed %d provider registr(ies); the bypass must short-circuit before any of this runs", *registries)
	}
	if stderr.Len() != 0 {
		t.Errorf("a city with no [storage] wrote to stderr: %q", stderr.String())
	}
}

// TestCLIMailWritesAndReadsTheBindingOnAMigratedCity is the council's M3
// defect, pinned end to end through the real `gc mail` provider root.
//
// Before the funnel, every one-shot command passed a hardcoded nil for its
// routes, so on a migrated city `gc mail send` wrote a message bead into the
// work store while the controller read the binding — mail that is sent, and
// mail that is never delivered, at the same time. The three assertions below are
// that one defect from three sides: the write is IN the binding, it is NOT in
// the retained work store, and a second one-shot provider reads back what the
// first one wrote.
func TestCLIMailWritesAndReadsTheBindingOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)

	sender, code := openCityMailProvider(io.Discard, "gc mail send")
	if sender == nil {
		t.Fatalf("openCityMailProvider returned no provider (code=%d)", code)
	}
	sent, err := sender.Send("worker", "mayor", "PR ready", "please review the auth PR")
	if err != nil {
		t.Fatalf("gc mail send: %v", err)
	}

	// A second one-shot command, with its own provider over the same memoized
	// routes: read and write have to agree, or mail lands somewhere `gc mail
	// check` never looks.
	reader, code := openCityMailProvider(io.Discard, "gc mail check")
	if reader == nil {
		t.Fatalf("the reading openCityMailProvider returned no provider (code=%d)", code)
	}
	unread, err := reader.Check("mayor")
	if err != nil {
		t.Fatalf("gc mail check: %v", err)
	}
	found := false
	for _, msg := range unread {
		if msg.ID == sent.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("gc mail check does not see %s, the message gc mail send just wrote (%d unread)", sent.ID, len(unread))
	}

	// The funnel's own handle goes first, so the assertions below read durable
	// bytes rather than state an open connection is holding.
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	if _, err := binding.Get(sent.ID); err != nil {
		t.Errorf("the one-shot mail write did not land in the binding: %v", err)
	}
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the retained work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	if _, err := work.Get(sent.ID); err == nil {
		t.Errorf("the one-shot mail write also landed in the work store as %s; a relocated class must be served from its binding only", sent.ID)
	} else if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("reading the work store for %s: %v", sent.ID, err)
	}
}

// TestCLIStorageRoutesRefuseAnUnconvergedCity covers the third arm: a city whose
// config names a binding it has not migrated onto.
//
// Three things have to hold at once, and the first is the one that matters: the
// class must NOT resolve to the work store. That is the answer that looks like
// success while reading the ledger the class was moved off, and it is the whole
// defect. The other two are what makes the refusal usable — every operation on
// the routed store fails with the remedy in it, and the reason is on stderr once
// even for a caller that discards the error.
func TestCLIStorageRoutesRefuseAnUnconvergedCity(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	stderr := captureCLIStorageStderr(t)

	work := beads.NewMemStore()
	sessions := cliSessionStore(work, nil, cityPath)
	if sessions == beads.Store(work) {
		t.Fatal("a city that never converged onto its binding still serves the session class from the work store")
	}
	if _, err := sessions.Create(beads.Bead{Title: "a session", Type: "session"}); err == nil {
		t.Error("a write to a refused class store succeeded")
	} else if !strings.Contains(err.Error(), storageMigrationCommand) {
		t.Errorf("the refused write does not name %q: %v", storageMigrationCommand, err)
	}
	if _, err := sessions.List(beads.ListQuery{AllowScan: true}); err == nil {
		t.Error("a read from a refused class store succeeded")
	}
	if !strings.Contains(stderr.String(), storageMigrationCommand) {
		t.Errorf("the refusal never reached the operator: %q", stderr.String())
	}

	// Work is not the class that moved, so a refused city still reads and writes
	// its own backlog — refusing that would take a recoverable city offline.
	if got := resolveClassStore(cliStorageRoutes(cityPath), work, nil, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
		t.Errorf("work resolved to %p on a refused city, want the work store %p", got, work)
	}
}

// TestStorageStatusStaysOffTheOneShotFunnel pins the exemption the refusal
// depends on: the operator surface must keep working on exactly the city the
// refusal names.
//
// A `gc storage status` that resolved a class store would read its report out of
// a store that refuses, on exactly the city it exists to describe. The exemption
// is structural — cmd_storage.go resolves its own target — and the memo is what
// proves it held: entering the funnel at all leaves an entry behind, so an empty
// memo is positive evidence that no class resolver ran.
func TestStorageStatusStaysOffTheOneShotFunnel(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(request, &stdout, &stderr); code == 0 {
		t.Errorf("status exited 0 on an unconverged city; stdout=%q", stdout.String())
	}
	if !strings.Contains(stdout.String(), storageMigrationCommand) {
		t.Errorf("status does not name the command that would converge the city: %q", stdout.String())
	}
	cliStorageRoutesMu.Lock()
	entered := len(cliStorageRoutesByCity)
	cliStorageRoutesMu.Unlock()
	if entered != 0 {
		t.Errorf("gc storage status resolved one-shot routes for %d city(ies); the operator surface must not route through a class", entered)
	}
}

// TestCLIStorageRoutesOpenTheBindingOncePerProcess pins the memo and the closer
// together, because they are one mechanism: the open happens once no matter how
// many class resolvers a command touches, and the close releases it once.
//
// The re-open after the close is the part worth asserting. It proves the close
// detached the memo rather than leaving a closed store behind for the next
// caller to read through — which in the test binary, where run() is entered many
// times per process, is the difference between a fixture and a landmine.
func TestCLIStorageRoutesOpenTheBindingOncePerProcess(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	registries := countStorageRegistryConstructions(t)

	first := cliStorageRoutes(cityPath)
	if first == nil {
		t.Fatal("a converged city resolved no routes")
	}
	if second := cliStorageRoutes(cityPath); second != first {
		t.Errorf("the second call opened the binding again (%p, then %p)", first, second)
	}
	if *registries != 1 {
		t.Errorf("the funnel constructed %d provider registr(ies) for one city, want 1", *registries)
	}

	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	if err := closeCLIStorageRoutes(); err != nil {
		t.Errorf("the second close reported %v; it must close nothing", err)
	}
	if again := cliStorageRoutes(cityPath); again == nil {
		t.Error("the funnel resolved nothing after a close; the memo must reset rather than serve a closed store")
	}
	if *registries != 2 {
		t.Errorf("the funnel constructed %d provider registr(ies) across a close, want 2", *registries)
	}
}
