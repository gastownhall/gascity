package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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

	if _, err := (unprobedWorkResidual{}).Get("gc-1"); err == nil {
		t.Error("the residual placeholder answered a Get; it must report the contract violation instead of a miss")
	}
}

// resetAutocloseFaultOnce lets a test observe the once-per-process warning more
// than once per test binary, and leaves the gate closed again afterwards so an
// unrelated test cannot inherit a spent one.
func resetAutocloseFaultOnce(t *testing.T) {
	t.Helper()
	autocloseFaultOnce = sync.Once{}
	t.Cleanup(func() { autocloseFaultOnce = sync.Once{} })
}
