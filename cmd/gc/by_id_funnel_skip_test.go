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
	"github.com/gastownhall/gascity/internal/coordclass"
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

// migratedFunnelSkipCity is a converged whole-split city whose binding holds
// one migration-preserved, work-shaped relic, as the migration leaves it:
// nothing has served the split yet, so it carries no served-binding note. The
// work store the gate reads is source, an in-memory store; the binding is the
// production sqlite opener.
func migratedFunnelSkipCity(t *testing.T) (cityPath string, relic beads.Bead, source beads.Store) {
	t.Helper()
	bindingRoot := filepath.Join(t.TempDir(), "store")
	cityPath = oneShotCLICity(t, bindingRoot)
	captureCLIStorageStderr(t)
	stubInfraControllerPing(t, 0)
	source = stubInfraMigrationSource(t)
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
	resetCLIStorageRoutes(t)
	return cityPath, relic, source
}

// funnelSkipCity is migratedFunnelSkipCity once its split has been served, so
// it carries the served-binding note every served city has. stranded then
// leaves an infrastructure bead in the work store, so the boot gate REFUSES the
// city from there on (the maintainer-city shape).
func funnelSkipCity(t *testing.T, stranded bool) (cityPath string, relic beads.Bead) {
	t.Helper()
	cityPath, relic, source := migratedFunnelSkipCity(t)
	cliStorageRoutes(cityPath)
	if _, present, err := readBornSplitServedNote(cityPath); err != nil || !present {
		t.Fatalf("serving the fixture city left no served-binding note (present=%v): %v", present, err)
	}
	resetCLIStorageRoutes(t)
	if stranded {
		mustCreateInfraBead(t, source, beads.Bead{Title: "order vote", Type: "task", Labels: []string{"order-tracking"}})
	}
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
// The boot gate's own stderr is compared too, against what the funnel prints
// on its own in a fresh process: its diagnostics and refusal on a refused city,
// nothing on a served one. A read that enters the funnel must print exactly
// that and a skipped read nothing, so that boot output is the one thing a skip
// may drop. The refused rows must show that trade at least once, or the
// comparison saw nothing to compare.
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
			shapeFunnelSkipManifest(t, bindingTargetOf(t, cityPath), tc.manifest, collected)
			maySkip := tc.manifest == skipManifestRead
			bootStderr := captureCLIStorageStderr(t)
			funnelBoot, refused := funnelBootStderr(t, cityPath, bootStderr)
			droppedBoot := false
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
				skipped := compareFunnelSkipRow(t, cityPath, row.argv, bootStderr, funnelBoot)
				if skipped != row.maySkip {
					t.Errorf("%v: skipped the funnel = %v, want %v", row.argv, skipped, row.maySkip)
				}
				droppedBoot = droppedBoot || (skipped && refused)
			}
			if refused && maySkip && !droppedBoot {
				t.Error("no skipped read on the refused city dropped the funnel's boot output, so the stderr rows compared nothing")
			}
		})
	}
}

// shapeFunnelSkipManifest leaves the binding's copy manifest in the shape an
// equivalence case runs on. A read manifest also records collected, an id the
// binding does not hold, as delivered.
func shapeFunnelSkipManifest(t *testing.T, target infraBindingTarget, shape funnelSkipManifest, collected string) {
	t.Helper()
	switch shape {
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
}

// funnelBootStderr resolves the funnel in a fresh process and returns what its
// boot gate printed to bootStderr, and whether it refused the city. It must
// print exactly when it refuses.
func funnelBootStderr(t *testing.T, cityPath string, bootStderr *bytes.Buffer) (out string, refused bool) {
	t.Helper()
	freshByIDProcess(t)
	bootStderr.Reset()
	sessions, _ := cliStorageRoutes(cityPath).storeFor(coordclass.ClassSessions)
	_, refused = sessions.(refusedClassStore)
	out = bootStderr.String()
	if (out != "") != refused {
		t.Fatalf("the funnel's own boot stderr is %q (refused=%v); want boot output exactly when it refuses", out, refused)
	}
	return out, refused
}

// compareFunnelSkipRow asks the door for argv twice from a fresh process: once
// free to skip, and once with the funnel already resolved. It reports whether
// the first read skipped the funnel. The two answers must agree; the boot
// gate's stderr must be funnelBoot, the funnel's own boot output, on the
// verdict's read and on an unskipped one, and nothing on a skipped one.
func compareFunnelSkipRow(t *testing.T, cityPath string, argv []string, bootStderr *bytes.Buffer, funnelBoot string) (skipped bool) {
	t.Helper()
	freshByIDProcess(t)
	bootStderr.Reset()
	skipCode, skipHandled, skipOut := routeByIDForTest(cityPath, argv)
	skipped = !cliStorageRoutesResolved(cityPath)
	skipBoot := bootStderr.String()

	freshByIDProcess(t)
	bootStderr.Reset()
	cliStorageRoutes(cityPath)
	gateCode, gateHandled, gateOut := routeByIDForTest(cityPath, argv)
	gateBoot := bootStderr.String()

	if skipCode != gateCode || skipHandled != gateHandled || skipOut != gateOut {
		t.Errorf("%v: skip-free door answered (code=%d handled=%v out=%q), the verdict answers (code=%d handled=%v out=%q)",
			argv, skipCode, skipHandled, skipOut, gateCode, gateHandled, gateOut)
	}
	if gateBoot != funnelBoot {
		t.Errorf("%v: boot stderr through the verdict = %q, want the funnel's own %q", argv, gateBoot, funnelBoot)
	}
	wantBoot := funnelBoot
	if skipped {
		wantBoot = ""
	}
	if skipBoot != wantBoot {
		t.Errorf("%v: boot stderr = %q, want %q (skipped the funnel = %v)", argv, skipBoot, wantBoot, skipped)
	}
	return skipped
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

// TestByIDFunnelSkipWaitsForTheServedBindingNote pins the one write the skip
// would otherwise starve. A split nothing has served since its cutover has no
// served-binding note, and the funnel is the note's only writer. Were twin-free
// reads to skip it there, the note would never be written, and the holds that
// read it would pass: here, a city rolled back to work by the runbook, whose
// cleared note the operator has removed, would read as one that never split
// and be served from the work store with no refusal.
//
// So the first twin-free read takes the funnel, which writes the note; the next
// one skips; and the rollback that follows is held: a read of the relic the
// binding holds enters the funnel, which refuses on the served-binding note.
func TestByIDFunnelSkipWaitsForTheServedBindingNote(t *testing.T) {
	cityPath, relic, _ := migratedFunnelSkipCity(t)
	bootStderr := captureCLIStorageStderr(t)
	if _, present, err := readBornSplitServedNote(cityPath); err != nil || present {
		t.Fatalf("the migrated fixture already carries a served-binding note (present=%v, err=%v); it would not exercise the first read", present, err)
	}

	argv := []string{"show", "gc-work1", "--json"}
	freshByIDProcess(t)
	if code, handled, out := routeByIDForTest(cityPath, argv); code != 0 || handled {
		t.Fatalf("%v before the first serve = (code=%d handled=%v out=%q), want the passthrough", argv, code, handled, out)
	}
	if !cliStorageRoutesResolved(cityPath) {
		t.Error("a twin-free read skipped the funnel on a split with no served-binding note; the funnel is the note's only writer")
	}
	if _, present, err := readBornSplitServedNote(cityPath); err != nil || !present {
		t.Fatalf("the first twin-free read left no served-binding note (present=%v): %v", present, err)
	}

	freshByIDProcess(t)
	if code, handled, out := routeByIDForTest(cityPath, argv); code != 0 || handled {
		t.Fatalf("%v once served = (code=%d handled=%v out=%q), want the passthrough", argv, code, handled, out)
	}
	if cliStorageRoutesResolved(cityPath) {
		t.Error("a twin-free read took the funnel on a split whose served-binding note is on disk")
	}

	// The runbook's rollback: the classes back on work, the backup restored,
	// and the cleared note removed as the operator's attestation. The hold
	// reads notes, not rows, so only the attestation is modeled.
	writeOneShotCityTOML(t, cityPath, "")
	if err := os.Remove(infraClearedNotePath(cityPath)); err != nil {
		t.Fatalf("removing the cleared note: %v", err)
	}
	if blocked, held := revertHoldingNote(storageSplitNone, cityPath); !held || blocked.ServedNotePath != bornSplitServedNotePath(cityPath) {
		t.Fatalf("the rollback is held=%v by %q; the served-binding note must be its only hold", held, blocked.ServedNotePath)
	}
	freshByIDProcess(t)
	bootStderr.Reset()
	routeByIDForTest(cityPath, []string{"show", relic.ID, "--json"})
	if !cliStorageRoutesResolved(cityPath) || !strings.Contains(bootStderr.String(), bornSplitServedNotePath(cityPath)) {
		t.Fatalf("the rollback's read of the binding's relic did not take the funnel and print its served-binding note refusal: funnel resolved=%v, boot stderr=%q",
			cliStorageRoutesResolved(cityPath), bootStderr.String())
	}
}

// TestByIDFunnelSkipTakesTheFunnelWhenTheServedBindingNoteWillNotRead pins the
// unreadable arm of servedBindingNoteIsOnDisk. A note that exists but will not
// read still holds the city, and the funnel's hold is what says so. A
// twin-free read must therefore take the funnel, and its stderr must name the
// note. Skipping would answer the same and drop that report.
func TestByIDFunnelSkipTakesTheFunnelWhenTheServedBindingNoteWillNotRead(t *testing.T) {
	cityPath, _ := funnelSkipCity(t, false)
	bootStderr := captureCLIStorageStderr(t)
	notePath := bornSplitServedNotePath(cityPath)
	if err := os.Remove(notePath); err != nil {
		t.Fatalf("removing the served-binding note: %v", err)
	}
	if err := os.Mkdir(notePath, 0o755); err != nil {
		t.Fatalf("putting a directory at the served-binding note's path: %v", err)
	}
	if _, _, err := readBornSplitServedNote(cityPath); err == nil {
		t.Fatal("a directory at the note's path reads cleanly; the fixture would not exercise an unreadable note")
	}

	argv := []string{"show", "gc-work1", "--json"}
	freshByIDProcess(t)
	if code, handled, out := routeByIDForTest(cityPath, argv); code != 0 || handled {
		t.Fatalf("%v with an unreadable note = (code=%d handled=%v out=%q), want the passthrough", argv, code, handled, out)
	}
	if !cliStorageRoutesResolved(cityPath) || !strings.Contains(bootStderr.String(), notePath) {
		t.Fatalf("a twin-free read on a city whose served-binding note will not read did not take the funnel and print the note's hold: funnel resolved=%v, boot stderr=%q",
			cliStorageRoutesResolved(cityPath), bootStderr.String())
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

// TestByIDDoorCostOnAServedSplit pins what one `gc bd show` pays on a served
// split, counted in storage plan resolutions (each is a registry built to open
// the binding). A twin-free id pays the census alone and skips the funnel: one.
// The binding-resident relic pays the census, then the funnel, which resolves
// the plan again to open the binding it serves from: two. The by-id read the
// funnel plans adds none, because a served city takes no relic proof.
func TestByIDDoorCostOnAServedSplit(t *testing.T) {
	cityPath, relic := funnelSkipCity(t, false)
	plans := countStoragePlanResolutions(t)
	for _, row := range []struct {
		id     string
		plans  int
		funnel bool
	}{
		{id: "gc-work1", plans: 1, funnel: false},
		{id: relic.ID, plans: 2, funnel: true},
	} {
		freshByIDProcess(t)
		*plans = 0
		routeByIDForTest(cityPath, []string{"show", row.id, "--json"})
		if *plans != row.plans {
			t.Errorf("gc bd show %s on a served split resolved %d storage plan(s), want %d", row.id, *plans, row.plans)
		}
		if got := cliStorageRoutesResolved(cityPath); got != row.funnel {
			t.Errorf("gc bd show %s on a served split resolved the funnel = %v, want %v", row.id, got, row.funnel)
		}
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
// string. The funnel's boot output goes to cliStorageStderr, which the fixture
// captures and a row that compares it captures separately, so what is folded
// here is exactly the door's answer.
func routeByIDForTest(cityPath string, argv []string) (int, bool, string) {
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", argv, &stdout, &stderr)
	return code, handled, stdout.String() + "|" + stderr.String()
}
