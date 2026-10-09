package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The by-id door's funnel skip (bdByIDAnswerIsThePassthroughForEveryVerdict).
//
// Entering the funnel is the boot gate, and on a split city that gate lists
// the whole work store's infrastructure slice to prove containment. These rows
// pin two things: a by-id read of a twin-free work id does not pay that
// listing, and skipping it changes no answer — every id answers exactly what
// it answers when the funnel is resolved first.

// recordingSourceRunner is a fake bd runner for the work store the boot gate
// censuses. It records every bd invocation and answers the census reads with an
// empty slice, so a row can tell "the gate listed the work store" from "it did
// not" by the calls alone.
type recordingSourceRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingSourceRunner) run(_, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(args, " "))
	r.mu.Unlock()
	if len(args) > 0 && (args[0] == "list" || args[0] == "query") {
		return []byte("[]"), nil
	}
	return nil, beads.ErrNotFound
}

func (r *recordingSourceRunner) listings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, call := range r.calls {
		if strings.HasPrefix(call, "list ") || strings.HasPrefix(call, "query ") {
			out = append(out, call)
		}
	}
	return out
}

// funnelSkipCity is a converged whole-split city whose binding holds one
// migration-preserved, work-shaped relic. stranded additionally leaves an
// infrastructure bead in the work store after the cutover, so the boot gate
// REFUSES the city (the maintainer-city shape). The work store the gate reads
// is an in-memory store; the binding is the production sqlite opener.
func funnelSkipCity(t *testing.T, stranded bool) (cityPath string, relic beads.Bead) {
	t.Helper()
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cityPath = oneShotCLICity(t, bindingRoot)
	captureCLIStorageStderr(t)
	stubInfraControllerPing(t, 0)
	source := stubInfraMigrationSource(t)
	relic = mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}})
	if bdIDIsClassReserved(relic.ID) {
		t.Fatalf("the fixture relic %q carries a reserved prefix; it cannot exercise the residence probe", relic.ID)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading the split city config: %v", err)
	}
	var log bytes.Buffer
	if report := migrateInfraClasses(t, cityPath, cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("the fixture city did not converge (%s): %s", report.Outcome, log.String())
	}
	if stranded {
		mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	}
	resetCLIStorageRoutes(t)
	return cityPath, relic
}

// freshByIDProcess drops every per-process memo the by-id door reads, as a new
// `gc bd` process starts with none.
func freshByIDProcess(t *testing.T) {
	t.Helper()
	resetCLIStorageRoutes(t)
}

// TestGcBdShowOfAWorkIDMakesNoWorkStoreListing is the maintainer-city cost,
// pinned with a fake bd runner as the work store the gate censuses. A plain
// `gc bd show <work-id>` on a split city used to resolve the funnel, whose
// containment check lists the work store's whole infrastructure slice through
// bd (`list --all --include-infra …` and `query ephemeral=true …`) — ~6 s of a
// ~14 s command whose own bd call takes ~5 s. The id is twin-free, so every
// verdict falls through to the passthrough: the only bd call is the
// passthrough's, and the work store is never listed.
//
// The control is the same command for the binding-resident relic: it does
// depend on the verdict, so it enters the funnel, and the same runner records
// the listing. That is what makes the zero above a measurement rather than a
// runner nobody calls.
func TestGcBdShowOfAWorkIDMakesNoWorkStoreListing(t *testing.T) {
	cityPath, relic := funnelSkipCity(t, false)
	runner := &recordingSourceRunner{}
	gateSource := beads.NewBdStore(cityPath, runner.run)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return gateSource, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	log := passthroughFakeBd(t, cityPath)
	freshByIDProcess(t)
	const workID = "gc-work1"
	args := []string{"show", workID, "--json"}
	var stdout, stderr bytes.Buffer
	if code := doBd(args, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(%v) = %d, want the passthrough's 0; stderr=%q", args, code, stderr.String())
	}
	if got := runner.listings(); len(got) != 0 {
		t.Fatalf("gc bd show of a twin-free work id listed the work store through bd: %q", got)
	}
	if calls := singleExecBdCalls(t, log); len(calls) != 1 || calls[0] != strings.Join(args, " ") {
		t.Fatalf("bd invocations = %q, want exactly the passthrough %q", calls, strings.Join(args, " "))
	}
	if cliStorageRoutesResolved(cityPath) {
		t.Fatal("the funnel was resolved for a read whose answer no verdict can change")
	}

	freshByIDProcess(t)
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"show", relic.ID, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(show %s) = %d on a served city whose binding holds it; stderr=%q", relic.ID, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), relic.ID) {
		t.Fatalf("the binding-resident relic was not answered from the binding: stdout=%q", stdout.String())
	}
	if got := runner.listings(); len(got) == 0 {
		t.Fatal("the control entered no funnel: a binding-resident id must take the verdict, and the gate lists the work store to reach it")
	}
	if calls := singleExecBdCalls(t, log); len(calls) != 1 {
		t.Fatalf("bd invocations after the control = %q; a binding-resident read is answered in process", calls)
	}
}

// funnelSkipManifest is the copy manifest shape one equivalence row runs on.
type funnelSkipManifest int

const (
	skipManifestRead funnelSkipManifest = iota
	skipManifestMissing
	skipManifestUnreadable
)

// TestByIDFunnelSkipChangesNoAnswer is the safety argument as a table. For
// each city shape and each id, the door is asked twice from a fresh process:
// once free to skip, and once with the funnel already resolved (which turns the
// skip off, so the answer is the one the verdict produces). The two must agree
// on exit code, handling and output — the skip may only ever save the listing.
//
// The ids cover every answer the door has: a twin-free work id (passthrough
// everywhere), the binding-resident relic (served from the binding, or denied
// on a refused city as a frozen twin), an id the copy manifest records
// delivering after the binding collected it (denied on a refused city), and a
// reserved-prefix id the binding does not hold (absent when served, the
// authority leg's refusal when refused), each through a served verb and an
// unserved one, plus a bulk close. Only argvs whose every id is twin-free may
// skip, and only while a manifest is there to read: when it is missing or
// unreadable the binding-wide rule denies the work id on the refused city, so
// it must take the verdict on every city.
func TestByIDFunnelSkipChangesNoAnswer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stranded bool
		// manifest is the copy manifest's shape after the cutover. Only
		// skipManifestRead lets the per-id rule decide; missing (deleted) and
		// unreadable (a directory where the file belongs, which a read fails
		// on even as root) leave it nothing to read, and the binding-wide rule
		// denies every non-reserved id on a refused city whose binding holds a
		// relic — so even the plain work id must take the verdict.
		manifest funnelSkipManifest
	}{
		{name: "served", stranded: false, manifest: skipManifestRead},
		{name: "refused", stranded: true, manifest: skipManifestRead},
		{name: "served without a manifest", stranded: false, manifest: skipManifestMissing},
		{name: "refused without a manifest", stranded: true, manifest: skipManifestMissing},
		{name: "served with an unreadable manifest", stranded: false, manifest: skipManifestUnreadable},
		{name: "refused with an unreadable manifest", stranded: true, manifest: skipManifestUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, relic := funnelSkipCity(t, tc.stranded)
			const collected = "gc-collected1"
			target := bindingTargetOf(t, cityPath)
			switch tc.manifest {
			case skipManifestRead:
				delivered, _, err := readInfraCopyManifest(target)
				if err != nil {
					t.Fatalf("reading the copy manifest: %v", err)
				}
				ids := make([]string, 0, len(delivered)+1)
				for id := range delivered {
					ids = append(ids, id)
				}
				if err := writeInfraCopyManifest(target, append(ids, collected)); err != nil {
					t.Fatalf("recording the collected id in the copy manifest: %v", err)
				}
			case skipManifestMissing:
				if err := os.Remove(target.ManifestPath()); err != nil {
					t.Fatalf("removing the copy manifest: %v", err)
				}
			case skipManifestUnreadable:
				if err := os.Remove(target.ManifestPath()); err != nil {
					t.Fatalf("removing the copy manifest: %v", err)
				}
				if err := os.Mkdir(target.ManifestPath(), 0o700); err != nil {
					t.Fatalf("putting a directory where the copy manifest belongs: %v", err)
				}
				if _, _, err := readInfraCopyManifest(target); err == nil {
					t.Fatal("the unreadable-manifest fixture read cleanly; it would exercise the per-id rule, not the fallback")
				}
			}
			maySkip := tc.manifest == skipManifestRead
			for _, row := range []struct {
				argv    []string
				maySkip bool
			}{
				{argv: []string{"show", "gc-work1", "--json"}, maySkip: maySkip},
				{argv: []string{"show", relic.ID, "--json"}},
				{argv: []string{"show", collected, "--json"}},
				{argv: []string{"show", "gcg-absent1", "--json"}},
				{argv: []string{"update", "gc-work1", "--notes", "n"}, maySkip: maySkip},
				{argv: []string{"update", relic.ID, "--notes", "n"}},
				{argv: []string{"close", "gc-work1", "gc-work2"}, maySkip: maySkip},
				{argv: []string{"close", "gc-work1", relic.ID}},
			} {
				freshByIDProcess(t)
				skipCode, skipHandled, skipOut := routeByIDForTest(cityPath, row.argv)
				skipped := !cliStorageRoutesResolved(cityPath)

				freshByIDProcess(t)
				cliStorageRoutes(cityPath)
				gateCode, gateHandled, gateOut := routeByIDForTest(cityPath, row.argv)

				if skipCode != gateCode || skipHandled != gateHandled || skipOut != gateOut {
					t.Errorf("%v: skip-free door answered (code=%d handled=%v out=%q), the verdict answers (code=%d handled=%v out=%q)",
						row.argv, skipCode, skipHandled, skipOut, gateCode, gateHandled, gateOut)
				}
				if skipped != row.maySkip {
					t.Errorf("%v: skipped the funnel = %v, want %v", row.argv, skipped, row.maySkip)
				}
			}
		})
	}
}

// TestByIDFunnelSkipNeverReadsABindingFaultAsAbsence pins the verdict the skip
// keys on. A binding Get that fails other than not-found denies on a refused
// city and is a read failure on a served one, so it must never be twinNone; a
// leg with no store decided nothing, so it must not be twinNone either. Only a
// not-found the rule does not deny is.
func TestByIDFunnelSkipNeverReadsABindingFaultAsAbsence(t *testing.T) {
	faulting := storeref.ClassBinding{Leg: storeref.Leg{Store: faultingGetStore{beads.NewMemStore()}}}
	if got := bindingTwinVerdict(faulting, "gc-work1", map[string]bool{}, true); got != twinDeny {
		t.Fatalf("a binding Get fault answered verdict %d, want twinDeny", got)
	}
	if got := bindingTwinVerdict(storeref.ClassBinding{}, "gc-work1", map[string]bool{}, true); got != twinUndecided {
		t.Fatalf("a binding leg with no store answered verdict %d, want twinUndecided", got)
	}
	clean := storeref.ClassBinding{Leg: storeref.Leg{Store: beads.NewMemStore()}}
	if got := bindingTwinVerdict(clean, "gc-work1", map[string]bool{}, true); got != twinNone {
		t.Fatalf("a clean not-found with a read manifest that does not list the id answered verdict %d, want twinNone", got)
	}
	proof := bindingsTwinProof([]storeref.ClassBinding{clean, faulting}, "gc-work1", map[string]bool{}, true)
	if proof.verdict != twinDeny {
		t.Fatalf("one clean miss and one faulting binding folded to verdict %d, want twinDeny", proof.verdict)
	}
	if proof := bindingsTwinProof(nil, "gc-work1", map[string]bool{}, true); proof.verdict != twinUndecided {
		t.Fatalf("no bindings at all folded to verdict %d, want twinUndecided", proof.verdict)
	}
	// One binding answered absence and one decided nothing: absence is not
	// proven for the binding that never answered, so the fold is not twinNone.
	if proof := bindingsTwinProof([]storeref.ClassBinding{clean, {}}, "gc-work1", map[string]bool{}, true); proof.verdict != twinUndecided {
		t.Fatalf("one clean miss and one storeless binding folded to verdict %d, want twinUndecided", proof.verdict)
	}
}

// TestByIDFunnelSkipTakesTheVerdictWhenTheConfigNoLongerNamesTheBinding pins
// the two no-split shapes that are not twin-free. A city that served a split
// and was pointed back at work (the served-binding note holds the revert), or
// edited into an arrangement this build cannot serve, still has a binding
// holding every preserved relic — the config just no longer names it. The work
// store answers those ids from frozen copies on every verdict, and the funnel's
// refusal is the only thing that tells the operator so. The census can prove
// nothing there, so the skip must take the verdict (which refuses before any
// listing), and the answer must still be the verdict's.
func TestByIDFunnelSkipTakesTheVerdictWhenTheConfigNoLongerNamesTheBinding(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
	}{
		{name: "reverted with the served-binding note", toml: ""},
		{name: "an unsupported arrangement", toml: "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, relic := funnelSkipCity(t, false)
			// Serve the split once, as the controller's boot does, so the
			// served-binding note records the history a revert is held by.
			cliStorageRoutes(cityPath)
			freshByIDProcess(t)
			if tc.toml == "" {
				writeOneShotCityTOML(t, cityPath, "")
				if _, held := revertHoldingNote(storageSplitNone, cityPath); !held {
					t.Fatal("the fixture's revert is not held by a served-binding note; it would not exercise the hazard")
				}
			} else {
				// A partial move back: sessions on work, the rest still on the
				// binding, which stays defined.
				tomlPath := filepath.Join(cityPath, "city.toml")
				body, err := os.ReadFile(tomlPath)
				if err != nil {
					t.Fatalf("reading city.toml: %v", err)
				}
				edited := strings.Replace(string(body), `sessions = "infra"`, `sessions = "work"`, 1)
				if edited == string(body) {
					t.Fatal("the fixture's city.toml carries no sessions class line to move")
				}
				if err := os.WriteFile(tomlPath, []byte(edited), 0o644); err != nil {
					t.Fatalf("writing city.toml: %v", err)
				}
				cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
				if err != nil {
					t.Fatalf("loading the edited city: %v", err)
				}
				if shape, _ := storageSplitShapeOf(cfg.EffectiveStorage()); shape != storageSplitUnsupported {
					t.Fatalf("the edited city's shape is %d, want storageSplitUnsupported", shape)
				}
			}
			for _, argv := range [][]string{
				{"show", "gc-work1", "--json"},
				{"show", relic.ID, "--json"},
			} {
				freshByIDProcess(t)
				if bdByIDAnswerIsThePassthroughForEveryVerdict(cityPath, argv[1:2]) {
					t.Errorf("%v: skipped the funnel on a city whose config no longer names the binding that may hold its relics", argv)
				}
				freshByIDProcess(t)
				skipCode, skipHandled, skipOut := routeByIDForTest(cityPath, argv)
				freshByIDProcess(t)
				cliStorageRoutes(cityPath)
				gateCode, gateHandled, gateOut := routeByIDForTest(cityPath, argv)
				if skipCode != gateCode || skipHandled != gateHandled || skipOut != gateOut {
					t.Errorf("%v: door answered (code=%d handled=%v out=%q), the verdict answers (code=%d handled=%v out=%q)",
						argv, skipCode, skipHandled, skipOut, gateCode, gateHandled, gateOut)
				}
			}
		})
	}
}

// TestByIDFunnelSkipNeverOpensABindingTheFunnelHolds pins the second-handle
// rule. Once the funnel is resolved in this process it may hold the binding
// open (a served city), and the skip's census would open a second handle on
// the same root — a second sqlite writer, a duplicate managed-Dolt server. So a
// resolved funnel turns the skip off and the door answers from the routes.
func TestByIDFunnelSkipNeverOpensABindingTheFunnelHolds(t *testing.T) {
	cityPath, _ := funnelSkipCity(t, false)
	cliStorageRoutes(cityPath)
	plans := countStoragePlanResolutions(t)
	if bdByIDAnswerIsThePassthroughForEveryVerdict(cityPath, []string{"gc-work1"}) {
		t.Fatal("the skip answered for a city whose funnel this process already resolved")
	}
	if *plans != 0 {
		t.Fatalf("the skip resolved %d storage plan(s) with the funnel already open; that is a second handle on the binding root", *plans)
	}
}

// TestByIDFunnelSkipTakesOneCensusForABulkArgv pins the bulk shape: a
// maintenance close of many stale work ids asks about every one, and the
// census opens the binding ONCE for all of them, not once per id.
func TestByIDFunnelSkipTakesOneCensusForABulkArgv(t *testing.T) {
	cityPath, _ := funnelSkipCity(t, true)
	freshByIDProcess(t)
	plans := countStoragePlanResolutions(t)
	ids := []string{"gc-work1", "gc-work2", "gc-work3", "gc-work4"}
	if !bdByIDAnswerIsThePassthroughForEveryVerdict(cityPath, ids) {
		t.Fatal("four twin-free work ids did not skip the funnel")
	}
	if *plans != 1 {
		t.Fatalf("the census resolved %d storage plans for one argv; want 1 (one binding open for every id)", *plans)
	}
}

// TestByIDFunnelSkipAnswersACityWithNoSplitWithoutABinding pins the cheap end:
// a city that configures no [storage] split has no binding to hold a preserved
// id, so a work id skips with one config read and no plan at all.
func TestByIDFunnelSkipAnswersACityWithNoSplitWithoutABinding(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	plans := countStoragePlanResolutions(t)
	if !bdByIDAnswerIsThePassthroughForEveryVerdict(cityPath, []string{"gc-work1"}) {
		t.Fatal("a work id on a city with no [storage] split did not skip the funnel")
	}
	if *plans != 0 {
		t.Fatalf("the skip resolved %d storage plan(s) for a city with no split", *plans)
	}
	if bdByIDAnswerIsThePassthroughForEveryVerdict(cityPath, []string{"gcg-1"}) {
		t.Fatal("a reserved-prefix id skipped the funnel; the prefix makes the binding its authority, so its answer is the verdict's")
	}
}

// bindingTargetOf resolves the fixture city's infra binding target.
func bindingTargetOf(t *testing.T, cityPath string) infraBindingTarget {
	t.Helper()
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loading the fixture city: %v", err)
	}
	target, configured, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !configured {
		t.Fatalf("the fixture resolved no infra binding target (configured=%v): %v", configured, err)
	}
	return target
}

// routeByIDForTest runs the door and folds its output into one comparable
// string. The funnel's own refusal goes to cliStorageStderr, which the fixture
// captures, so what is compared here is exactly the door's answer.
func routeByIDForTest(cityPath string, argv []string) (int, bool, string) {
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", argv, &stdout, &stderr)
	return code, handled, stdout.String() + "|" + stderr.String()
}
