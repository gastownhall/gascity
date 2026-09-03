package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/storeref/storereftest"
)

// convoyCityConfig loads the city config the convoy resolver takes, so a test
// can call resolveOwningStoreDir the way its production callers do.
func convoyCityConfig(t *testing.T, cityPath string) *config.City {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loading the city config at %s: %v", cityPath, err)
	}
	return cfg
}

// resolveThroughTheConvoyScan is the convoy arm's by-id resolution, as its
// callers reach it.
func resolveThroughTheConvoyScan(t *testing.T, cityPath, id string) (beads.Store, string) {
	t.Helper()
	store, dir, err := resolveOwningStoreDir(id, convoyCityConfig(t, cityPath), cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		t.Fatalf("resolving the store that owns %s: %v", id, err)
	}
	return store, dir
}

// TestConvoyResolutionServesTheBindingCopy adds the convoy arm to the
// cross-plane binding-wins property.
//
// This is the arm the property was missing, and it is missing for a structural
// reason rather than an oversight: the convoy resolver's work axis is a scan of
// the city's DIRECTORIES, and a relocated class binding is not one of them. So
// before the binding leg went in front, an infrastructure bead here was not
// merely unrouted — it was answered, successfully, by the copy `gc storage
// migrate` retained in the city store, and the close that followed wrote
// through it.
func TestConvoyResolutionServesTheBindingCopy(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	resident := classResidentWorkShapedBead(t, classStore, shadow.ID, "the class-binding copy")
	control, err := work.Create(beads.Bead{Title: "a work bead the binding never held", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the control: %v", err)
	}

	storereftest.RunBindingWins(t,
		storereftest.BindingWinsStores{
			Binding:       classStore,
			Work:          work,
			DualID:        resident.ID,
			BindingTitle:  "the class-binding copy",
			WorkOnlyID:    control.ID,
			WorkOnlyTitle: "a work bead the binding never held",
		},
		storereftest.BindingWinsSurface{
			Name: "the gc convoy by-id resolver",
			Get: func(t *testing.T, id string) beads.Bead {
				t.Helper()
				store, _ := resolveThroughTheConvoyScan(t, cityPath, id)
				b, err := store.Get(id)
				if err != nil {
					t.Fatalf("reading %s from the resolved store: %v", id, err)
				}
				return b
			},
			Close: func(t *testing.T, id string) {
				t.Helper()
				store, _ := resolveThroughTheConvoyScan(t, cityPath, id)
				if err := store.Close(id); err != nil {
					t.Fatalf("closing %s through the resolved store: %v", id, err)
				}
			},
		})
}

// TestConvoyResolutionReportsTheCityDirForABindingHit pins the store-ref
// argument the resolver's doc makes.
//
// The directory this returns is mapped to a store-ref that scopes molecule-root
// lookups. A relocated bead lived in the city store and carried the city's ref
// before the migration moved it, and a binding is not a rig and has no ref of
// its own — so reporting anything but the city path here would strand every
// root recorded before the move.
func TestConvoyResolutionReportsTheCityDirForABindingHit(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	resident := classResidentWorkShapedBead(t, classStore, "gc-relic1", "a relocated patrol root")

	store, dir := resolveThroughTheConvoyScan(t, cityPath, resident.ID)
	if store != classStore {
		t.Errorf("the convoy resolver returned %p for %s, want the class binding %p", store, resident.ID, classStore)
	}
	if dir != cityPath {
		t.Errorf("the convoy resolver reported dir %q for a binding hit, want the city path %q — the store-ref these beads carried before the migration is the city's", dir, cityPath)
	}
}

// TestConvoyResolutionDoesNotRefuseDualResidenceAsAmbiguous is the deliberate
// short-circuit, asserted.
//
// The scan REFUSES an id present in more than one candidate store, which is
// right when two ledgers disagree by accident. Dual residency is not that: the
// migration copies with ids preserved and deletes nothing, so a relocated bead
// is supposed to exist twice and has a known winner. A resolver that reached
// the uniqueness rule here would refuse every convoy command on exactly the
// cities that finished migrating.
func TestConvoyResolutionDoesNotRefuseDualResidenceAsAmbiguous(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	resident := classResidentWorkShapedBead(t, classStore, shadow.ID, "the class-binding copy")

	store, _, err := resolveOwningStoreDir(resident.ID, convoyCityConfig(t, cityPath), cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		t.Fatalf("a dual-resident id resolved to %v; dual residency is the migration working, not two ledgers disagreeing", err)
	}
	if store != classStore {
		t.Errorf("a dual-resident id resolved %p, want the class binding %p", store, classStore)
	}
}

// TestConvoyResolutionUnchangedOnACityThatRelocatesNothing is the compatibility
// row. A city with no [storage] binding plans no binding leg, so the scan runs
// exactly as it did — including its uniqueness refusal, which the binding
// short-circuit must not have disarmed for everyone.
func TestConvoyResolutionUnchangedOnACityThatRelocatesNothing(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	refuseInfraMigrationSource(t)
	captureCLIStorageStderr(t)
	work := workStoreFor(t, cityPath)
	bead, err := work.Create(beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}

	store, dir := resolveThroughTheConvoyScan(t, cityPath, bead.ID)
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("reading %s back: %v", bead.ID, err)
	}
	if got.Title != "an ordinary work bead" {
		t.Errorf("the scan served %q, want the work copy", got.Title)
	}
	if dir != cityPath {
		t.Errorf("the scan reported dir %q, want %q", dir, cityPath)
	}

	if _, _, err := resolveOwningStoreDir("gc-nothing-here", convoyCityConfig(t, cityPath), cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	}); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("an absent id resolved to %v, want beads.ErrNotFound — the scan's own miss shape", err)
	}
}

// TestConvoyResolutionSurfacesABindingFaultRatherThanAbsence is the
// classification the whole lane exists for, on this arm. A binding that cannot
// answer must not degrade into a scan of the ledger that holds the stale copy:
// that turns "I could not read the owner" into "the owner is the work store",
// and the write that follows lands where nothing reads.
func TestConvoyResolutionSurfacesABindingFaultRatherThanAbsence(t *testing.T) {
	cityPath := t.TempDir()
	boom := errors.New("binding unreachable")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(errStore{err: boom}))

	_, _, err := resolveOwningStoreDir("hq-1", nil, cityPath, func(string) (beads.Store, error) {
		return splittest.NewWorkStore(t, "hq"), nil
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("an unreadable binding resolved to err=%v, want the read failure", err)
	}
}

// TestAutocloseOwningStoreAnnouncesAFaultOnce pins the loud-skip.
//
// bd's on_close must not fail because a root could not be resolved, so this
// path swallows every error and falls back to the cwd-rooted store. That is
// right for absence and wrong for a fault, and the difference is invisible
// unless it is said out loud — but only once, because bd closes beads in bursts
// and a repeated line buries the log it wants to be seen in.
func TestAutocloseOwningStoreAnnouncesAFaultOnce(t *testing.T) {
	resetAutocloseFaultOnce(t)
	cityPath, _ := foreignProviderCity(t)
	sink := captureCLIStorageStderr(t)
	failClassBindingReads(t, cityPath, errors.New("the class binding is having a bad day"))

	for range 2 {
		if store, _, ok := autocloseOwningStore("hq-1", cityPath); ok {
			t.Fatalf("a failing binding resolved to %p; the fault must not be answered by the work ledger", store)
		}
	}

	warnings := bytes.Count(sink.Bytes(), []byte("gc autoclose: resolving the store that owns"))
	if warnings != 1 {
		t.Errorf("the fault was announced %d times over two closes, want exactly 1: %s", warnings, sink.String())
	}
	if !bytes.Contains(sink.Bytes(), []byte("bad day")) {
		t.Errorf("the announcement does not carry the store's own cause: %s", sink.String())
	}
}

// TestAutocloseOwningStoreStaysQuietOnAbsence is the control for the test
// above. Absence is the ordinary case — most closed beads are not molecule
// members — and announcing it would make the warning meaningless.
func TestAutocloseOwningStoreStaysQuietOnAbsence(t *testing.T) {
	resetAutocloseFaultOnce(t)
	cityPath, _ := foreignProviderCity(t)
	sink := captureCLIStorageStderr(t)

	if store, _, ok := autocloseOwningStore("hq-nothing-here", cityPath); ok {
		t.Fatalf("an absent id resolved to %p", store)
	}
	if bytes.Contains(sink.Bytes(), []byte("gc autoclose:")) {
		t.Errorf("absence was announced as a fault: %s", sink.String())
	}
}

// TestBeadsShowFallbackServesTheBindingCopy is the read half on the `gc beads
// show` arm: the same scan, taking the first hit rather than refusing a second
// one, and the same retained copy standing in front of the live one.
func TestBeadsShowFallbackServesTheBindingCopy(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	resident := classResidentWorkShapedBead(t, classStore, shadow.ID, "the class-binding copy")

	var stdout, stderr bytes.Buffer
	if code := doBeadsShowFallback(cityPath, resident.ID, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads show %s exited %d: %s", resident.ID, code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("the class-binding copy")) {
		t.Errorf("gc beads show served %s, want the binding's copy — the work store's is frozen at migration time", stdout.String())
	}
}

// TestBeadsShowFallbackScansForAnIdNoBindingHolds is the control: an id the
// binding never held is still served by the scan, which is what makes this
// about residence rather than about the binding winning everything.
func TestBeadsShowFallbackScansForAnIdNoBindingHolds(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	bead, err := work.Create(beads.Bead{Title: "a work bead the binding never held", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsShowFallback(cityPath, bead.ID, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads show %s exited %d: %s", bead.ID, code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("a work bead the binding never held")) {
		t.Errorf("gc beads show served %s, want the work copy", stdout.String())
	}
}

// TestBindingOwnerLeavesTheWorkResidualUnprobed is the sentinel's contract.
//
// cliByIDBindingOwner hands the plan a placeholder where the work leg goes,
// because this arm's work axis is a directory scan the resolver must not run.
// That is safe only while the residual is returned UNPROBED, so the placeholder
// reports being read as an internal error — and a clean ok=false here is the
// proof it was not.
func TestBindingOwnerLeavesTheWorkResidualUnprobed(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	refuseInfraMigrationSource(t)
	captureCLIStorageStderr(t)

	owner, ok, err := cliByIDBindingOwner(cityPath, "gc-1")
	if err != nil {
		t.Fatalf("a city that relocates nothing resolved to err=%v; the residual must come back unprobed, not read", err)
	}
	if ok {
		t.Errorf("a city that relocates nothing reported a binding owner %p", owner.Store)
	}

	residual := newUnprobedWorkResidual()
	got, err := residual.Get("gc-1")
	if err == nil {
		t.Error("the residual placeholder answered a Get; it must report the contract violation instead of a miss")
	}
	if !strings.Contains(err.Error(), "gc-1") {
		t.Errorf("the residual's Get refusal reads %v, want the probed id named", err)
	}
	_ = got

	// Get is the method the resolver reaches for, so it is the one with a
	// bespoke message — but a leg role that called anything else must not get a
	// nil-pointer panic, and must not get a clean empty answer either.
	if _, err := residual.List(beads.ListQuery{}); !errors.Is(err, errWorkResidualProbed) {
		t.Errorf("the residual answered List with err=%v, want the contract violation — an empty list here reads as absence", err)
	}
	if err := residual.Close("gc-1"); !errors.Is(err, errWorkResidualProbed) {
		t.Errorf("the residual answered Close with err=%v, want the contract violation", err)
	}
	if _, err := residual.Create(beads.Bead{Title: "written through a placeholder"}); !errors.Is(err, errWorkResidualProbed) {
		t.Errorf("the residual answered Create with err=%v, want the contract violation", err)
	}
}

// TestConvoyResolutionStillRefusesAnIdTwoLedgersBothHold covers the rule the
// binding short-circuit steps around, which until now nothing asserted anywhere
// in the tree.
//
// The claim "dual residency is not ambiguity" only means something if ambiguity
// is still refused when it is real. Two candidate stores holding the same id
// with no binding in play is two ledgers disagreeing by accident, and the scan
// must say so rather than resolve to whichever candidate it enumerated first —
// the close that followed would write the loser.
func TestConvoyResolutionStillRefusesAnIdTwoLedgersBothHold(t *testing.T) {
	cityPath := t.TempDir()
	seedCLIStorageRoutes(t, cityPath, &storageRoutes{stores: map[coordclass.Class]beads.Store{}})
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")}}}

	byDir := map[string]beads.Store{}
	openStore := func(dir string) (beads.Store, error) {
		if store, ok := byDir[dir]; ok {
			return store, nil
		}
		store := splittest.NewWorkStore(t, "hq")
		if _, err := store.Create(beads.Bead{Title: "a copy in " + dir, Type: "task"}); err != nil {
			t.Fatalf("seeding the candidate at %s: %v", dir, err)
		}
		byDir[dir] = store
		return store, nil
	}

	if len(convoyStoreCandidates(cfg, cityPath, "hq-1")) < 2 {
		t.Fatalf("the fixture offers %d candidate store(s); a uniqueness refusal needs at least two", len(convoyStoreCandidates(cfg, cityPath, "hq-1")))
	}
	_, _, err := resolveOwningStoreDir("hq-1", cfg, cityPath, openStore)
	if err == nil {
		t.Fatal("an id two candidate stores both hold resolved cleanly; the scan's uniqueness rule is gone and the close would write whichever store enumerated last")
	}
	if !strings.Contains(err.Error(), "uniquely addressable") {
		t.Errorf("the refusal reads %v, want the uniqueness contract named", err)
	}
}

// TestBeadForOwnerDoesNotReReadAProbedLeg pins the reason Owner carries a bead
// at all.
//
// A leg the resolver actually probed has already paid for the read, and the
// answer it returns IS that read. Fetching it again is not merely wasteful: it
// opens a window in which the second read disagrees with the one the resolver
// made its ownership decision from.
func TestBeadForOwnerDoesNotReReadAProbedLeg(t *testing.T) {
	counted := &countingGetStore{Store: splittest.NewWorkStore(t, "hq")}
	seeded, err := counted.Create(beads.Bead{Title: "the probed answer", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the counted store: %v", err)
	}

	got, err := beadForOwner(storeref.Owner{Store: counted, Bead: seeded, Read: true}, seeded.ID)
	if err != nil {
		t.Fatalf("reading a probed owner: %v", err)
	}
	if got.Title != "the probed answer" {
		t.Errorf("a probed owner served %q, want the bead the resolver already read", got.Title)
	}
	if counted.gets != 0 {
		t.Errorf("a probed owner cost %d further Get(s), want 0 — the leg's read is the caller's read", counted.gets)
	}

	if _, err := beadForOwner(storeref.Owner{Store: counted}, seeded.ID); err != nil {
		t.Fatalf("reading an unprobed owner: %v", err)
	}
	if counted.gets != 1 {
		t.Errorf("an unprobed owner cost %d Get(s), want exactly 1", counted.gets)
	}
}

// countingGetStore counts the reads a caller makes of its own accord, so a test
// can tell a served answer from a re-fetched one.
type countingGetStore struct {
	beads.Store
	gets int
}

func (s *countingGetStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

// resetAutocloseFaultOnce lets a test observe the once-per-process warning more
// than once per test binary, and leaves the gate closed again afterwards so an
// unrelated test cannot inherit a spent one.
func resetAutocloseFaultOnce(t *testing.T) {
	t.Helper()
	autocloseFaultOnce = sync.Once{}
	t.Cleanup(func() { autocloseFaultOnce = sync.Once{} })
}

// TestByIDPlanUsesTheRegisteredControllerRoutes pins which routes the by-id
// seam plans over.
//
// This resolver is not one-shot-only. A controller reaches it in-process —
// order dispatch's emit store resolves a molecule root through
// autocloseOwningStore, which lands in resolveOwningStoreDir — and a plan built
// from the one-shot funnel inside a process that already opened its binding at
// boot would open a SECOND handle on the same binding root: a duplicate
// managed-Dolt server or a second sqlite writer. residencyTopologyForCity
// exists to prevent exactly that, and this asserts the seam uses it.
//
// The funnel is seeded with an errStore rather than merely a different store,
// so a plan that took the wrong routes FAILS rather than quietly answering from
// the wrong handle. The second row is the one-shot fallback: with no
// registration the funnel must still be what answers, or every genuine one-shot
// command loses its binding leg.
func TestByIDPlanUsesTheRegisteredControllerRoutes(t *testing.T) {
	funnelFault := errors.New("the one-shot funnel opened a second handle")

	tests := []struct {
		name     string
		register bool
	}{
		{name: "a registered controller's routes win over the funnel", register: true},
		{name: "a one-shot command still falls through to the funnel", register: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()

			var wantStore beads.Store
			if tt.register {
				// Seeded FIRST: registerResidencyRoutes drops the derived
				// per-city binding memo, so the registration is what a later
				// resolution re-reads.
				seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(errStore{err: funnelFault}))
				registered := splittest.NewWorkStore(t, "hq")
				routes := messagingSplitRoutes(registered)
				registerResidencyRoutes(cityPath, routes, func() beads.Store { return registered })
				t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })
				wantStore = registered
			} else {
				funnel := splittest.NewWorkStore(t, "hq")
				seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(funnel))
				wantStore = funnel
			}

			resident, err := wantStore.Create(beads.Bead{Title: "the binding's copy", Type: "task"})
			if err != nil {
				t.Fatalf("seeding the binding that should answer: %v", err)
			}

			owner, ok, err := cliByIDBindingOwner(cityPath, resident.ID)
			if err != nil {
				t.Fatalf("resolving %s: %v — a fault here is the funnel's binding answering, which means a second handle on the binding root", resident.ID, err)
			}
			if !ok {
				t.Fatalf("no binding owned %s; the plan carried no binding leg at all", resident.ID)
			}
			if owner.Store != wantStore {
				t.Errorf("the by-id plan resolved %s through %p, want %p", resident.ID, owner.Store, wantStore)
			}

			// The production caller, with a scan that fails the test if it runs:
			// a binding hit must return before the directory scan, so the funnel
			// is never reached by this arm either.
			store, dir, err := resolveOwningStoreDir(resident.ID, nil, cityPath, func(storeDir string) (beads.Store, error) {
				t.Errorf("the convoy scan opened %q for an id the binding owns", storeDir)
				return nil, errors.New("the scan must not run for a binding hit")
			})
			if err != nil {
				t.Fatalf("resolveOwningStoreDir(%s): %v", resident.ID, err)
			}
			if store != wantStore {
				t.Errorf("the convoy resolver served %s from %p, want the binding %p", resident.ID, store, wantStore)
			}
			if dir != cityPath {
				t.Errorf("the convoy resolver reported dir %q, want the city path %q", dir, cityPath)
			}
		})
	}
}
