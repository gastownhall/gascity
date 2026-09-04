package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// workOnlyStorageConfig is the rollback spelling the runbook documents: an
// explicit [storage.classes] map with every class on the reserved work binding,
// and no binding definition left over for a class to fail to select.
func workOnlyStorageConfig() *config.City {
	return &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     config.StorageWorkBinding,
			Sessions:  config.StorageWorkBinding,
			Messaging: config.StorageWorkBinding,
			Orders:    config.StorageWorkBinding,
			Nudges:    config.StorageWorkBinding,
		},
	}}
}

// decodeStorageBindingOutcome reads back the one event a gate recorded, failing
// on anything but exactly one.
func decodeStorageBindingOutcome(t *testing.T, rec *events.Fake) (events.Event, storebinding.StorageBindingOutcomePayload) {
	t.Helper()
	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d event(s) %+v, want exactly one", len(rec.Events), rec.Events)
	}
	var payload storebinding.StorageBindingOutcomePayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("decoding the %s payload: %v", rec.Events[0].Type, err)
	}
	return rec.Events[0], payload
}

// TestEveryMigrationOutcomeReachesARegisteredEventType is the exhaustiveness
// property, and it is what stops the next outcome added to this taxonomy from
// being invisible.
//
// recordStorageBindingOutcome publishes nothing for an outcome its map does not
// name, and it does so silently — so an outcome added without a mapping does not
// fail, it just never appears in the event stream. A subscriber gating a deploy
// on those events would read that silence as "no verdict was reached", which is
// the one thing that never happened.
//
// The walk is driven by String()'s own fallback rather than a hand-kept list, so
// an outcome added to the const block is covered here the moment it exists
// without anyone remembering to add it — but only as far as String() names it.
// A constant added with no String() arm ends the walk early and every outcome
// after it goes unvisited, which is the same invisibility with one more step in
// front of it. So the walk's length is cross-checked against the const block
// itself, read from source: the two counts disagree exactly when a constant
// exists that String() does not name.
func TestEveryMigrationOutcomeReachesARegisteredEventType(t *testing.T) {
	declared := declaredMigrationOutcomeCount(t)
	visited := 0
	for i := 0; ; i++ {
		outcome := infraMigrationOutcome(i)
		if strings.HasPrefix(outcome.String(), "infraMigrationOutcome(") {
			if i == 0 {
				t.Fatal("the walk found no named outcome at all, so it proves nothing")
			}
			break
		}
		visited++
		eventType, mapped := storageBindingEventTypes[outcome]
		if !mapped {
			t.Errorf("outcome %v reaches no event type, so a process concluding it publishes nothing and a subscriber cannot tell that verdict from no verdict", outcome)
			continue
		}
		if !slices.Contains(events.KnownEventTypes, eventType) {
			t.Errorf("outcome %v maps to %q, which is missing from events.KnownEventTypes, so the SSE projection would carry it untyped", outcome, eventType)
		}
	}
	if visited != declared {
		t.Errorf("the walk visited %d outcome(s) but the const block declares %d: an outcome String() does not name stops the walk, and everything at or after it was never checked for an event type", visited, declared)
	}
}

// declaredMigrationOutcomeCount counts the constants in the infraMigrationOutcome
// const block, read from source.
//
// Source rather than reflection because Go gives an int-based enum no runtime
// membership at all: nothing at run time can distinguish a declared constant
// from any other int, which is the reason String()'s fallback exists and the
// reason a walk driven by it cannot see past a missing arm. The declaration is
// the only place the true count lives.
func declaredMigrationOutcomeCount(t *testing.T) int {
	t.Helper()
	const source = "infra_class_migrate.go"
	file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", source, err)
	}
	declared := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		block := 0
		typed := false
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if name, ok := value.Type.(*ast.Ident); ok && name.Name == "infraMigrationOutcome" {
				typed = true
			}
			block += len(value.Names)
		}
		if typed {
			declared += block
		}
	}
	if declared == 0 {
		t.Fatalf("found no infraMigrationOutcome constants in %s, so the cross-check proves nothing", source)
	}
	return declared
}

// TestNotConfiguredCityPublishesItsVerdictRatherThanSilence pins the fourth
// outcome onto the wire.
//
// A city with no infrastructure split used to leave the gate having recorded
// nothing at all, and nothing is ambiguous: it reads the same as a gate that
// crashed before it decided, or a build too old to have the gate. "This city has
// no split" is a verdict, and a deploy gate reading these events has to be able
// to see it as one.
//
// Both spellings are covered because both reach the bypass and an operator can
// author either: no [storage] section at all, and an explicit map that puts
// every class back on the work binding.
func TestNotConfiguredCityPublishesItsVerdictRatherThanSilence(t *testing.T) {
	refuseInfraMigrationSource(t)

	for name, cfg := range map[string]*config.City{
		"no [storage] section": {},
		"every class on work":  workOnlyStorageConfig(),
	} {
		t.Run(name, func(t *testing.T) {
			rec := events.NewFake()
			var stderr bytes.Buffer
			routes, err := storageBootGate(t.TempDir(), cfg, "gc start", rec, &stderr)
			if err != nil {
				t.Fatalf("a city with no split was refused: %v", err)
			}
			if routes != nil {
				t.Fatalf("a city with no split resolved routes %+v", routes)
			}
			event, payload := decodeStorageBindingOutcome(t, rec)
			if event.Type != events.StorageBindingNotConfigured {
				t.Fatalf("recorded %s, want %s", event.Type, events.StorageBindingNotConfigured)
			}
			if payload.Outcome != infraMigrationNotConfigured.String() {
				t.Errorf("payload outcome = %q, want %q", payload.Outcome, infraMigrationNotConfigured.String())
			}
			if payload.Binding != "" || payload.Database != "" {
				t.Errorf("a city with no split named binding %q at %q; nothing was resolved, so naming one would be an invention", payload.Binding, payload.Database)
			}
			if payload.Invariant != "" {
				t.Errorf("a city with nothing wrong carries a blocking invariant: %q", payload.Invariant)
			}
		})
	}
}

// TestNotConfiguredVerdictStillCreatesNothing keeps the compatibility contract
// intact across the new event.
//
// The bypass exists so a city that authored no [storage] reaches no registry, no
// plan and no binding read. Publishing a verdict must not walk any of that back:
// the event is a statement about the config that was already loaded, not a
// finding that required looking anywhere.
func TestNotConfiguredVerdictStillCreatesNothing(t *testing.T) {
	registries := countStorageRegistryConstructions(t)
	refuseInfraMigrationSource(t)

	cityPath := t.TempDir()
	rec := events.NewFake()
	var stderr bytes.Buffer
	if _, err := storageBootGate(cityPath, &config.City{}, "gc start", rec, &stderr); err != nil {
		t.Fatalf("a city with no [storage] was refused: %v", err)
	}
	if entries := directoryEntryNames(t, cityPath); len(entries) != 0 {
		t.Errorf("publishing the not-configured verdict left %v in the city", entries)
	}
	if *registries != 0 {
		t.Errorf("publishing the not-configured verdict constructed %d provider registr(ies)", *registries)
	}
	if stderr.Len() != 0 {
		t.Errorf("the bypass wrote to stderr: %q", stderr.String())
	}
}

// TestConvergedVerdictCarriesTheProvenCopySize puts a number on the converged
// event.
//
// "Converged" on its own does not distinguish a city serving its whole
// infrastructure slice from the binding from one whose proven copy is empty, and
// those are the two situations an operator watching a cutover most needs to tell
// apart. The count is the size of the proven-copy manifest the verdict already
// read, so it costs the boot path nothing: no census, no second store opened.
//
// Two sizes rather than one, because one size cannot tell a manifest read from
// a constant: a field hardcoded to whatever the single fixture carries passes
// every assertion that fixture can make. Two cities carrying different slices
// leave no constant that satisfies both.
func TestConvergedVerdictCarriesTheProvenCopySize(t *testing.T) {
	for _, carried := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d carried", carried), func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
			source := stubInfraMigrationSource(t)
			for i := 0; i < carried; i++ {
				mustCreateInfraBead(t, source, beads.Bead{Title: fmt.Sprintf("session %d", i), Type: "session"})
			}

			var log bytes.Buffer
			if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
			}

			rec := events.NewFake()
			var stderr bytes.Buffer
			routes, err := storageBootGate(cityPath, cfg, "gc start", rec, &stderr)
			if err != nil {
				t.Fatalf("a converged city was refused: %v", err)
			}
			t.Cleanup(func() { _ = routes.close() })

			event, payload := decodeStorageBindingOutcome(t, rec)
			if event.Type != events.StorageBindingConverged {
				t.Fatalf("recorded %s, want %s", event.Type, events.StorageBindingConverged)
			}
			if payload.ProvenBeads != carried {
				t.Errorf("ProvenBeads = %d, want %d; the converged verdict does not say how big the copy it rests on is", payload.ProvenBeads, carried)
			}
		})
	}
}

// TestGenesisVerdictReportsAnEmptyProvenCopyHonestly is the control the count is
// worthless without.
//
// A genesis city converged on a copy that carried nothing, and zero is the true
// answer for it. Without this case a count that was simply never populated would
// pass every genesis assertion, so this pins that zero here is the manifest's
// size and not an unset field — which the converged case above, with three, is
// what makes checkable.
func TestGenesisVerdictReportsAnEmptyProvenCopyHonestly(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	stubInfraMigrationSource(t)

	rec := events.NewFake()
	var stderr bytes.Buffer
	routes, err := storageBootGate(cityPath, cfg, "gc start", rec, &stderr)
	if err != nil {
		t.Fatalf("a genesis city was refused: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })

	event, payload := decodeStorageBindingOutcome(t, rec)
	if event.Type != events.StorageBindingGenesis {
		t.Fatalf("recorded %s, want %s", event.Type, events.StorageBindingGenesis)
	}
	if payload.ProvenBeads != 0 {
		t.Errorf("ProvenBeads = %d on a city that had nothing to move, want 0", payload.ProvenBeads)
	}
}

// TestUncheckableVerdictNamesTheReadThatFailed puts the fault on the wire.
//
// "Could not be verified (reason above)" is an instruction to look somewhere
// this reader cannot go. A subscriber holds one event and no terminal, so a
// refusal that defers to stderr tells it only that something went wrong — which
// is the half of the answer it already had from the event's own type.
//
// The city here converged and then lost the database out from under its own
// marker, which is the uncheckable state that matters most: the marker forbids
// the revert, so the only way forward is the one named by the read that failed.
func TestUncheckableVerdictNamesTheReadThatFailed(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "carried across", Type: "session"})

	var log bytes.Buffer
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		t.Fatalf("resolving the binding target: ok=%t err=%v", ok, err)
	}
	if err := os.Remove(target.Database); err != nil {
		t.Fatalf("removing the binding database: %v", err)
	}

	rec := events.NewFake()
	var stderr bytes.Buffer
	if _, err := storageBootGate(cityPath, cfg, "gc start", rec, &stderr); err == nil {
		t.Fatal("a city whose binding database vanished under its own convergence marker was served")
	}
	event, payload := decodeStorageBindingOutcome(t, rec)
	if event.Type != events.StorageBindingUncheckable {
		t.Fatalf("recorded %s, want %s", event.Type, events.StorageBindingUncheckable)
	}
	if !strings.Contains(payload.Invariant, target.Database) {
		t.Errorf("the uncheckable payload does not name the read that failed, so a subscriber holding only this event cannot tell which fault to fix: %q", payload.Invariant)
	}
}

// TestStorageStatusReportsBothSidesOfTheCutover covers the pair of numbers an
// operator reads to decide whether a cutover finished.
//
// The source census alone cannot answer it: the source is RETAINED verbatim, so
// its count is identical before and after a successful migration. What changes
// is the binding, and until now `gc storage status` reported the manifest — a
// record of what the copy was proven to deliver at cutover — rather than what
// the binding holds today. Those diverge the moment the binding's own GC runs.
//
// So the fixture makes them diverge, rather than asserting two numbers that
// happen to be equal. A bead the copy delivered is dropped from the binding
// afterwards — the reap this line exists to make visible — leaving source 2 and
// binding 1. Equal counts would let status print the source's number on the
// binding's line and pass, which is precisely the regression described above.
func TestStorageStatusReportsBothSidesOfTheCutover(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "carried across", Type: "session"})
	reaped := mustCreateInfraBead(t, source, beads.Bead{Title: "reaped after cutover", Type: "session"}).ID

	var log bytes.Buffer
	if got := migrateInfraClasses(t, request.CityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("the migration reported %s: %s", got.Outcome, log.String())
	}
	dropFromInfraBinding(t, request.CityPath, cfg, reaped)

	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(request, &stdout, &stderr); code != 0 {
		t.Fatalf("status exited %d on a converged city: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "source: 2 infrastructure bead(s) retained") {
		t.Errorf("status does not report the retained source census: %q", out)
	}
	if !strings.Contains(out, "binding: 1 infrastructure bead(s)") {
		t.Errorf("status does not report what the binding itself holds, so the only count an operator gets is the source's — which is unchanged by a successful cutover: %q", out)
	}
}

// dropFromInfraBinding removes one bead from the binding, standing in for the
// binding's own garbage collection reaping a row the copy delivered.
func dropFromInfraBinding(t *testing.T, cityPath string, cfg *config.City, id string) {
	t.Helper()
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		t.Fatalf("resolving the binding target: ok=%t err=%v", ok, err)
	}
	store, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding: %v", err)
	}
	defer closeBeadStoreHandle(store) //nolint:errcheck // best-effort close
	if err := store.Delete(id); err != nil {
		t.Fatalf("dropping %s from the binding: %v", id, err)
	}
}

// TestStorageStatusCensusOfAnAbsentBindingCreatesNothing pins the read-only
// contract on the census, and the refusal to guess when it cannot run.
//
// An unconverged city has no binding root, and a whole missing root is the one
// state that reads the same whether the city never cut over or its volume is
// not mounted right now. So the line reports the fault instead of a number: a
// confident "0 held now" would sit next to "converged: no" as a second piece of
// positive-looking evidence for a city that may well hold everything.
//
// Learning the count by opening the database would CREATE it — the report would
// answer its own question and leave a store behind on a city that never cut
// over, which is exactly the mutation the whole status path is defined against.
func TestStorageStatusCensusOfAnAbsentBindingCreatesNothing(t *testing.T) {
	bindingParent := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(bindingParent, "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})

	before := treeFingerprint(t, bindingParent)
	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(request, &stdout, &stderr); code == 0 {
		t.Fatalf("status exited 0 on an unconverged city: %q", stdout.String())
	}
	if got := treeFingerprint(t, bindingParent); !equalStrings(before, got) {
		t.Errorf("the binding census changed the binding tree:\n before %v\n after  %v", before, got)
	}
	out := stdout.String()
	if !strings.Contains(out, "binding: unknown") {
		t.Errorf("status does not report the binding side at all on an unconverged city, so the two sides cannot be compared where comparing them matters most: %q", out)
	}
	if strings.Contains(out, "binding: 0 infrastructure bead(s)") {
		t.Errorf("status reports a confident zero for a binding root nobody could look inside: %q", out)
	}
	if !strings.Contains(out, filepath.Join(bindingParent, "store")) {
		t.Errorf("the unknown census does not name the root it could not read, so an operator cannot tell an unmounted volume from a city that never cut over: %q", out)
	}
}

// TestStorageStatusCountsABindingItCanRead is the control the refusal above is
// worthless without.
//
// "Unknown" is the safe answer, and a census that always answered unknown would
// pass every assertion in that test while reporting nothing. This is the same
// unconverged city with its binding root present and empty — the state a genesis
// boot leaves — where zero is a real observation and must be printed as one.
func TestStorageStatusCountsABindingItCanRead(t *testing.T) {
	bindingParent := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(bindingParent, "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	if err := os.MkdirAll(filepath.Join(bindingParent, "store"), 0o755); err != nil {
		t.Fatalf("creating the binding root: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doStorageStatus(request, &stdout, &stderr); code == 0 {
		t.Fatalf("status exited 0 on an unconverged city: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "binding: 0 infrastructure bead(s)") {
		t.Errorf("status withholds a count it could take: an empty readable root holds zero beads, and that zero is an observation: %q", stdout.String())
	}
}

// TestStorageStatusLeavesNothingBehindWhenItOpensTheBinding covers the one
// census path where the read-only claim is not obviously true, and it is the
// status analog of TestPreflightLeavesNothingBehindWhenItOpensTheDestination.
//
// The two tests above only reach the arms that open nothing: an absent root
// reports a fault and an absent database counts zero from a stat. Here the
// database is already on disk, so infraBindingCensus opens a bead engine to
// count what it holds. A deploy gate is allowed to run this on a serving city
// as often as it likes, so nothing the engine does while answering may touch
// what the binding already holds. assertReadOnlyDiagnosticResidue is that
// claim, and its doc comment carries why the WAL and SHM sidecars are the one
// thing a mode=ro connection may leave behind.
func TestStorageStatusLeavesNothingBehindWhenItOpensTheBinding(t *testing.T) {
	bindingParent := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(bindingParent, "store"))
	request := storageTestRequest(t, cfg)
	source := stubInfraMigrationSource(t)
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})

	// The database has to exist for the census to open anything, and it is
	// created the only way production creates it.
	target := mustResolveInfraTarget(t, request.CityPath, cfg)
	destination, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding to populate it: %v", err)
	}
	mustCreateInfraBead(t, destination, beads.Bead{Title: "a bead the binding already holds", Type: "session"})
	if err := closeBeadStoreHandle(destination); err != nil {
		t.Fatalf("closing the populated binding: %v", err)
	}

	before := treeFingerprint(t, bindingParent)
	var stdout, stderr bytes.Buffer
	doStorageStatus(request, &stdout, &stderr)

	// The exit code is not the assertion — this city is unconverged either way.
	// What makes the fingerprint mean something is that the census reported a
	// count, which it can only do by opening the database.
	if !strings.Contains(stdout.String(), "binding: 1 infrastructure bead(s)") {
		t.Fatalf("the census did not count the binding, so it never opened it and the fingerprint below proves nothing: %q", stdout.String())
	}
	assertReadOnlyDiagnosticResidue(t, before, treeFingerprint(t, bindingParent), target.Database)
}
