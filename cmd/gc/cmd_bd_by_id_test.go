package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
	"github.com/gastownhall/gascity/internal/storeref/storereftest"
)

// configRefEngineProviderID is the foreign provider the fixtures below serve
// their infrastructure classes from. It is not the built-in engine, so
// resolveInfraBindingTarget refuses it and the whole migration apparatus is out
// of the picture — which is exactly the city this surface used to answer with a
// bd subprocess against the work workspace.
const configRefEngineProviderID = storebinding.ProviderID("outoftree-configref-engine")

// configRefEngineProviderFactory re-registers this build's own engine factory
// under a foreign provider ID that is configured the way every non-built-in
// provider must be: by CONFIGURATION REFERENCE. city.toml admits `path` only for
// the built-in engine (config.validateStorageBindingConfig), so a fixture that
// has to survive a real config load cannot borrow the built-in spelling.
//
// The binding still opens a real bead engine minting real reserved-prefix ids,
// so these tests prove the routing rather than a mock of it.
type configRefEngineProviderFactory struct{}

func (configRefEngineProviderFactory) ID() storebinding.ProviderID { return configRefEngineProviderID }

func (configRefEngineProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	inner := sqlitebinding.BeadsProviderFactory{}
	provider, err := inner.New(configRefEngineSpec(spec))
	if err != nil {
		return nil, err
	}
	return configRefEngineProvider{Provider: provider}, nil
}

// configRefEngineSpec translates this provider's configuration reference into
// the path the inner engine is configured with, the way a real out-of-tree
// provider turns its own opaque reference into a location.
func configRefEngineSpec(spec storebinding.BindingSpec) storebinding.BindingSpec {
	spec.Provider = sqlitebinding.BeadsProviderID
	spec.Path = filepath.Join(spec.CityRoot, ".gc", "engine-"+string(spec.ConfigRef))
	spec.ConfigRef = ""
	return spec
}

// configRefEngineProvider forwards the provider facade and translates the spec
// on both seams a serving binding uses, so the inner engine's own "refuse a
// foreign spec" defense stays armed.
type configRefEngineProvider struct {
	storebinding.Provider
}

func (p configRefEngineProvider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	opener, ok := p.Provider.(storebinding.EngineOpener)
	if !ok {
		return nil, nil, errors.New("inner provider opens no engine")
	}
	return opener.OpenEngine(configRefEngineSpec(spec), classes)
}

func (p configRefEngineProvider) BindingLocation(spec storebinding.BindingSpec) (string, error) {
	locator, ok := p.Provider.(storebinding.BindingLocator)
	if !ok {
		return "", errors.New("inner provider reports no location")
	}
	return locator.BindingLocation(configRefEngineSpec(spec))
}

// writeForeignProviderCityTOML writes a city whose whole infrastructure split is
// served by a foreign provider, in the config-reference spelling.
func writeForeignProviderCityTOML(t *testing.T, cityPath, provider, ref string) {
	t.Helper()
	body := fmt.Sprintf(`[workspace]
name = "by-id-city"

[storage.classes]
work = %q
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = %q
config_ref = %q
`, config.StorageWorkBinding, provider, ref)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

// registerConfigRefEngineProvider freezes a registry carrying only the foreign
// engine provider, for the whole of one test.
func registerConfigRefEngineProvider(t *testing.T) {
	t.Helper()
	prev := newStorageRegistryForPlan
	newStorageRegistryForPlan = func() (*storebinding.ProviderRegistry, error) {
		registry := storebinding.NewProviderRegistry()
		if err := registry.Register(configRefEngineProviderFactory{}); err != nil {
			return nil, err
		}
		if err := registry.Freeze(); err != nil {
			return nil, err
		}
		return registry, nil
	}
	t.Cleanup(func() { newStorageRegistryForPlan = prev })
}

// foreignProviderCity prepares a city that SERVES its infrastructure classes
// from a non-built-in provider, and returns the class store the binding opened.
//
// The work store is stubbed empty because the born-split discipline is what
// admits such a city: a provider this build cannot migrate onto serves only
// while no infrastructure bead sits in the work store.
func foreignProviderCity(t *testing.T) (cityPath string, classStore beads.Store) {
	t.Helper()
	clearGCEnv(t)
	cityPath = t.TempDir()
	writeForeignProviderCityTOML(t, cityPath, string(configRefEngineProviderID), "infra")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", cityPath)
	registerConfigRefEngineProvider(t)
	stubInfraMigrationSource(t)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	store, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	if !relocated {
		t.Fatal("a city serving its classes from a foreign provider resolved no class binding")
	}
	return cityPath, store
}

// soleClassBindingStore resolves the city's sole relocated class binding as a
// STORE, for the fixtures that seed both sides of a migration.
//
// A fan-out — the only error cliSoleClassBinding returns — is a topology this
// build refuses to serve, so a fixture that met one has not built the city it
// meant to build and says so here rather than seeding half of it.
func soleClassBindingStore(t *testing.T, cityPath string) beads.Store {
	t.Helper()
	binding, relocated, err := cliSoleClassBinding(cityPath)
	if err != nil {
		t.Fatalf("resolving the city's class binding: %v", err)
	}
	if !relocated {
		t.Fatal("the fixture city resolved no class binding; it is not split")
	}
	return binding.Store
}

// recensusAfterSeedingARelic reopens the funnel so the binding's relic census
// runs again, and returns the class store on the far side of it.
//
// The census runs when the funnel OPENS a binding, and a fixture that plants a
// work-shaped id afterwards has produced a verdict that was true when it was
// taken and is false by the time the row asserts on it. The residence probe
// retires on that stale verdict — MintsReserved && !HasLegacyResidents — and the
// row then reads the retained work copy while claiming to test that the binding
// wins.
//
// Reopening is not a workaround for the ordering: it IS what production does.
// Relics are what `gc storage migrate` leaves behind, so every process that
// meets them starts after they exist, censuses once at boot, and sees them. A
// one-shot `gc` invocation against a migrated city is exactly this.
//
// The returned store is a NEW handle — the engine is a real sqlite database
// under .gc/, so the seeded relic is still there, but the pointer the fixture
// handed back before the reopen is closed and no longer the one the door
// resolves. Rows that compare store identity must use this one.
func recensusAfterSeedingARelic(t *testing.T, cityPath string) beads.Store {
	t.Helper()
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the funnel so the binding is censused again: %v", err)
	}
	return soleClassBindingStore(t, cityPath)
}

// dropDerivedResidencyMemo invalidates the grouping derived from these routes.
//
// Swapping routes.stores in place is invisible to cliResidencyBindings, which
// caches the class-to-store grouping it read out of those routes: anything that
// already resolved the grouping keeps handing out the stores that were there
// BEFORE the swap. A test that routed one command before installing its failing
// store would then exercise a healthy store while asserting on a fault path,
// and pass for the wrong reason. Dropping the memo here makes that ordering bug
// unwritable rather than merely currently absent.
func dropDerivedResidencyMemo(t *testing.T, cityPath string) {
	t.Helper()
	dropCLIResidencyBindings(filepath.Clean(cityPath))
	t.Cleanup(func() { dropCLIResidencyBindings(filepath.Clean(cityPath)) })
}

// mustCreateClassBead creates a bead in the class binding and proves it carries
// a reserved class prefix, which is what makes it unanswerable by any other
// store.
func mustCreateClassBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("creating %q in the class binding: %v", b.Title, err)
	}
	if !bdIDIsClassReserved(created.ID) {
		t.Fatalf("the class binding minted %q, which carries no reserved class prefix", created.ID)
	}
	return created
}

// TestBdByIDServesAClassBeadFromANonBuiltInProviderBinding is the regression.
//
// The city serves its infrastructure classes from a provider this build carries
// no migration discipline for — a beads workspace, a fork's own engine, anything
// that is not the built-in one. A by-ID read of a bead that lives in that
// binding must be answered from it. Before this, target resolution asked the
// MIGRATION whether it recognized the provider, that answer was no, and the read
// fell through to the bd subprocess pointed at the work workspace, which does
// not hold the bead and cannot say so.
func TestBdByIDServesAClassBeadFromANonBuiltInProviderBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "lives in the binding", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"show", bead.ID, "--json"}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-owned id fell through to the bd subprocess: stderr %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("showing %s exited %d: %s", bead.ID, code, stderr.String())
	}
	var shown []beads.Bead
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("decoding the routed show output %q: %v", stdout.String(), err)
	}
	if len(shown) != 1 || shown[0].ID != bead.ID {
		t.Fatalf("the routed show printed %+v, want exactly %s", shown, bead.ID)
	}
	if shown[0].Title != bead.Title {
		t.Errorf("the routed show printed title %q, want %q", shown[0].Title, bead.Title)
	}
}

// TestBdByIDServesClaimReleaseAndDepListFromTheClassBinding covers the other
// three verbs on the same foreign-provider city. They are the cascade-nudge
// order's reads and the orphan-recovery scripts' writes, and every one of them
// used to be answered by a subprocess against the wrong workspace.
func TestBdByIDServesClaimReleaseAndDepListFromTheClassBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	subject := mustCreateClassBead(t, classStore, beads.Bead{Title: "the subject", Type: "task"})
	blocker := mustCreateClassBead(t, classStore, beads.Bead{Title: "the blocker", Type: "task"})
	if err := classStore.DepAdd(subject.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("adding the dependency: %v", err)
	}

	t.Setenv("BEADS_ACTOR", "by-id-tester")
	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"update", subject.ID, "--claim"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("claiming %s = (%d, %t): %s", subject.ID, code, handled, stderr.String())
	}
	claimed, err := classStore.Get(subject.ID)
	if err != nil {
		t.Fatalf("re-reading the claimed bead: %v", err)
	}
	if claimed.Assignee != "by-id-tester" {
		t.Errorf("the routed claim recorded assignee %q, want %q", claimed.Assignee, "by-id-tester")
	}

	stdout.Reset()
	stderr.Reset()
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"release-if-current", subject.ID, "by-id-tester"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("releasing %s = (%d, %t): %s", subject.ID, code, handled, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "released" {
		t.Errorf("the routed release printed %q, want %q", got, "released")
	}

	stdout.Reset()
	stderr.Reset()
	code, handled := maybeRouteBdByID(cityPath, "", []string{"dep", "list", subject.ID, "--json"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("listing %s dependencies = (%d, %t): %s", subject.ID, code, handled, stderr.String())
	}
	var rows []bdByIDDepRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decoding the routed dep list %q: %v", stdout.String(), err)
	}
	if len(rows) != 1 || rows[0].ID != blocker.ID {
		t.Fatalf("the routed dep list printed %+v, want exactly %s", rows, blocker.ID)
	}
	if rows[0].DepType != "blocks" {
		t.Errorf("the routed dep list reported edge type %q, want %q", rows[0].DepType, "blocks")
	}
}

// bdByIDTreeWireRow is the tree row as a CONSUMER reads it, declared here rather
// than borrowed from the production type on purpose: what the pack scripts and
// pr_review.py parse is the JSON, and a test that decodes into the emitter's own
// struct would follow a field rename straight past them.
type bdByIDTreeWireRow struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Depth    int    `json:"depth"`
	Parent   string `json:"parent_id"`
	Edge     string `json:"edge_from_parent"`
	External bool   `json:"external"`
}

func routedDepTree(t *testing.T, cityPath string, args ...string) []bdByIDTreeWireRow {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", append([]string{"dep", "tree"}, args...), &stdout, &stderr)
	if !handled {
		t.Fatalf("dep tree %v fell through to the bd subprocess", args)
	}
	if code != 0 {
		t.Fatalf("dep tree %v = %d: %s", args, code, stderr.String())
	}
	var rows []bdByIDTreeWireRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decoding the routed dep tree %q: %v", stdout.String(), err)
	}
	return rows
}

// TestBdByIDServesDepTreeFromTheClassBinding is the recursive read, and it is
// here because a molecule's whole subtree is what every status summary asks for.
//
// `dep list` was already served and `dep tree` was not, so the one federated
// read that resolves a relocated root — pr_review.py's city_dep_subtree, which
// the pr-review label poller runs on every labeled PR — was refused on every
// tick. The tree walk is composed from the SAME contract the single-level read
// uses: DepList to a level, Get for each related bead. Nothing new is asked of
// the store, which is why serving it needs no contract change.
//
// The fixture is the shape a molecule actually has, and every leg of it is a
// case the walk gets wrong if it is written as a naive recursion:
//
//   - a two-level subtree, so pre-order depth and parent_id are observable;
//   - a `relates-to` edge, which bd's own walker excludes from the tree (it is a
//     loose knowledge-graph link, not structure) and which would otherwise drag
//     an unrelated bead into a molecule summary;
//   - an edge out of the class store, reported as external and NOT recursed,
//     the same boundary `dep list` draws;
//   - a cycle, which without a visited set is an infinite walk rather than a
//     wrong answer.
func TestBdByIDServesDepTreeFromTheClassBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	root := mustCreateClassBead(t, classStore, beads.Bead{Title: "molecule root", Type: "task"})
	step := mustCreateClassBead(t, classStore, beads.Bead{Title: "step", Type: "task"})
	leaf := mustCreateClassBead(t, classStore, beads.Bead{Title: "leaf", Type: "task"})
	aside := mustCreateClassBead(t, classStore, beads.Bead{Title: "loosely related", Type: "task"})
	external := reservedClassID(t, "notresident")

	for _, edge := range []struct{ from, to, kind string }{
		{root.ID, step.ID, "blocks"},
		{step.ID, leaf.ID, "parent-child"},
		{root.ID, aside.ID, "relates-to"},
		{leaf.ID, external, "blocks"},
		{leaf.ID, root.ID, "blocks"}, // the cycle
	} {
		if err := classStore.DepAdd(edge.from, edge.to, edge.kind); err != nil {
			t.Fatalf("adding %s -%s-> %s: %v", edge.from, edge.kind, edge.to, err)
		}
	}

	rows := routedDepTree(t, cityPath, root.ID, "--json")
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.ID)
	}
	want := []string{root.ID, step.ID, leaf.ID, external}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the routed dep tree walked %v, want the pre-order subtree %v (relates-to excluded, the cycle visited once)", got, want)
	}
	for i, want := range []bdByIDTreeWireRow{
		{ID: root.ID, Depth: 0},
		{ID: step.ID, Depth: 1, Parent: root.ID, Edge: "blocks"},
		{ID: leaf.ID, Depth: 2, Parent: step.ID, Edge: "parent-child"},
		{ID: external, Depth: 3, Parent: leaf.ID, Edge: "blocks", External: true},
	} {
		if rows[i].Depth != want.Depth || rows[i].Parent != want.Parent || rows[i].Edge != want.Edge {
			t.Errorf("row %d = %+v, want depth=%d parent=%q edge=%q", i, rows[i], want.Depth, want.Parent, want.Edge)
		}
		if rows[i].External != want.External {
			t.Errorf("row %d external = %t, want %t; an edge out of this class binding is a declared reference, not a resident bead", i, rows[i].External, want.External)
		}
	}
	if rows[0].Status != root.Status {
		t.Errorf("the root row carries status %q, want %q; the summary reads status off these rows", rows[0].Status, root.Status)
	}

	// --max-depth is bd's own safety limit and it CUTS: depth >= max stops, so
	// the deepest row a `--max-depth 2` walk emits is depth 1.
	shallow := routedDepTree(t, cityPath, root.ID, "--max-depth", "2", "--json")
	if len(shallow) != 2 || shallow[1].ID != step.ID {
		t.Errorf("--max-depth 2 walked %+v, want the root and its one child", shallow)
	}

	// --direction=up is the same walk over the reverse edges, and it must not
	// silently answer the down question.
	up := routedDepTree(t, cityPath, leaf.ID, "--direction=up", "--json")
	upIDs := make([]string, 0, len(up))
	for _, row := range up {
		upIDs = append(upIDs, row.ID)
	}
	if strings.Join(upIDs, ",") != strings.Join([]string{leaf.ID, step.ID, root.ID}, ",") {
		t.Errorf("--direction=up from the leaf walked %v, want the chain of beads that depend on it", upIDs)
	}
	if len(up) > 1 && up[1].Edge != "parent-child" {
		t.Errorf("the reverse walk reported edge %q from the leaf's dependent, want the edge that relates them", up[1].Edge)
	}
}

// TestBdByIDReservedMissFallsThroughToTheWorkAxis pins the rule that makes the
// routing correct: the binding is the namespace's AUTHORITY, not its only lawful
// holder.
//
// A reserved prefix is warned-and-allowed on a work store — config.ValidateRigs
// does not reject it and config.ReservedPrefixWarnings only advises — so a rig
// or HQ configured with one legitimately mints and holds ids inside the reserved
// namespace, and a stranded pre-split mint sits there too. The binding answering
// "not here" therefore settles the binding and nothing else, and the id goes to
// this surface's own work axis: the bd passthrough. Refusing it stranded every
// such bead on this one door, including the core pack's step-completion write.
//
// The control in the same test is what stops this passing by never routing.
func TestBdByIDReservedMissFallsThroughToTheWorkAxis(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	missing := reservedClassID(t, "notthere")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"show", missing}, &stdout, &stderr)
	if handled {
		t.Fatalf("a reserved-prefix id the binding does not hold was refused here (exit %d): %s%s", code, stdout.String(), stderr.String())
	}

	// The control: the same city, an id the binding DOES hold, still served.
	held := mustCreateClassBead(t, classStore, beads.Bead{Title: "held by the binding", Type: "task"})
	stdout.Reset()
	stderr.Reset()
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"show", held.ID, "--json"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("show %s = (%d, %t): %s — the fall-through above must not have become a door that never routes", held.ID, code, handled, stderr.String())
	}
	if !strings.Contains(stdout.String(), held.ID) {
		t.Errorf("the served show printed %q, want %s", stdout.String(), held.ID)
	}
}

// TestBdByIDRoutesAWorkShapedIDResidentInTheClassBinding pins the residence
// probe, which is the arm that has no reserved prefix to lean on.
//
// `gc storage migrate` copies the work store's infrastructure slice with its ids
// PRESERVED, so a converged city holds work-SHAPED ids inside the class binding.
// Deciding ownership by prefix alone would send exactly those reads back to the
// ledger they were moved off — and they are the beads a migrated city has the
// most of, because every one of them predates the split.
func TestBdByIDRoutesAWorkShapedIDResidentInTheClassBinding(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	migrated := beads.Bead{ID: "demo-premigration", Title: "carried across by the migration", Type: "task", Description: "work-shaped id, class-resident row"}
	created, err := migrationSeed(classStore, migrated)
	if err != nil {
		t.Fatalf("seeding a work-shaped id in the class binding: %v", err)
	}
	if bdIDIsClassReserved(created.ID) {
		t.Fatalf("the fixture id %q carries a reserved class prefix; it cannot exercise the residence probe", created.ID)
	}

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"show", created.ID, "--json"}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident work-shaped id fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("showing %s exited %d: %s", created.ID, code, stderr.String())
	}
	var shown []beads.Bead
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("decoding %q: %v", stdout.String(), err)
	}
	if len(shown) != 1 || shown[0].ID != created.ID {
		t.Fatalf("the routed show printed %+v, want %s from the class binding", shown, created.ID)
	}
}

// TestBdByIDServesTheStepCompletionWrite is the core-pack write:
// graph-worker.md closes a worked bead with
// `gc bd update <id> --set-metadata gc.outcome=pass --status closed`, and on a
// split city that bead is class-owned. The passthrough wrote the outcome into
// the work ledger and left the bead open in the binding — a molecule that stalls
// with no error anywhere.
func TestBdByIDServesTheStepCompletionWrite(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	step := mustCreateClassBead(t, classStore, beads.Bead{Title: "the step", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", step.ID, "--set-metadata", "gc.outcome=pass", "--status", "closed"}, &stdout, &stderr)
	if !handled {
		t.Fatalf("the step-completion write fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("the step-completion write exited %d: %s", code, stderr.String())
	}
	after, err := classStore.Get(step.ID)
	if err != nil {
		t.Fatalf("re-reading the closed step: %v", err)
	}
	if after.Status != "closed" {
		t.Errorf("status = %q, want closed", after.Status)
	}
	if after.Metadata["gc.outcome"] != "pass" {
		t.Errorf("gc.outcome = %q, want pass (metadata=%v)", after.Metadata["gc.outcome"], after.Metadata)
	}

	// The `--status=closed` spelling the formula uses must land identically.
	other := mustCreateClassBead(t, classStore, beads.Bead{Title: "the other step", Type: "task"})
	stdout.Reset()
	stderr.Reset()
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"update", other.ID, "--set-metadata", "gc.outcome=fail", "--set-metadata", "gc.failure_class=transient", "--status=closed"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("the inline-equals spelling = (%d, %t): %s", code, handled, stderr.String())
	}
	afterOther, err := classStore.Get(other.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	if afterOther.Status != "closed" || afterOther.Metadata["gc.failure_class"] != "transient" {
		t.Errorf("status=%q metadata=%v, want closed with both metadata keys", afterOther.Status, afterOther.Metadata)
	}
}

// TestBdByIDShowRendersTheWholeRecord is the second-order wrong answer.
//
// graph-worker.md tells an agent to `gc bd show <id>` and then "execute exactly
// that bead's description". A terse id/status/title line is a well-formed
// answer with the instruction silently missing — the agent reads it, finds
// nothing to do, and reports success. The routed text form therefore renders the
// whole record, and says where it came from.
func TestBdByIDShowRendersTheWholeRecord(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	priority := 1
	bead := mustCreateClassBead(t, classStore, beads.Bead{
		Title:       "do the thing",
		Type:        "task",
		Priority:    &priority,
		Assignee:    "worker-1",
		Description: "Line one of the instruction.\nLine two.",
		Labels:      []string{"gc:step"},
		Metadata:    beads.StringMap{"gc.outcome": "pending"},
	})

	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"show", bead.ID}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("showing %s = (%d, %t): %s", bead.ID, code, handled, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		bead.ID,
		"do the thing",
		"Line one of the instruction.",
		"Line two.",
		"gc.outcome=pending",
		"gc:step",
		"worker-1",
		"description:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the routed record omits %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "served in process") {
		t.Errorf("the routed record does not say where it came from: %q", stderr.String())
	}
}

// TestBdByIDLeavesWorkStoreIDsToThePassthrough is the other half of the same
// rule. An ordinary work id the class store has never seen is still bd's to
// answer, and the passthrough answers it byte-identically.
func TestBdByIDLeavesWorkStoreIDsToThePassthrough(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)

	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"show", "gc-abc123"}, &stdout, &stderr); handled {
		t.Fatalf("a work-store id was answered here (exit %d): %s%s", code, stdout.String(), stderr.String())
	}
}

// TestBdByIDDoesNotRouteAnIDInAValuePosition pins the value-position escape: a
// filter whose VALUE quotes a class id is a work question, and answering or
// refusing it here breaks the consumer that asks it.
func TestBdByIDDoesNotRouteAnIDInAValuePosition(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	quoted := reservedClassID(t, "quoted")

	for _, args := range [][]string{
		{"list", "--metadata-field", "workflow_id=" + quoted},
		{"list", "--label", quoted},
	} {
		var stdout, stderr bytes.Buffer
		if code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr); handled {
			t.Errorf("%v was answered here (exit %d): %s%s", args, code, stdout.String(), stderr.String())
		}
	}
}

// TestBdByIDRefusesAnUnservedVerbOnAClassOwnedBead is the fail-closed floor. An
// operation this surface does not serve, whose subject the class binding owns,
// must not reach bd: bd opens the work workspace, cannot see the bead, and
// either blocks on it or mutates whatever its substring resolver found there.
//
// `delete` is the example because it is the one write-mutation verb this
// surface deliberately still does not serve: it is destructive and rare, and an
// operator drains a class resident with close. close/reopen moved onto the
// served set with ga-axin6 (TestBdCloseReservedPrefixServedInProcess).
func TestBdByIDRefusesAnUnservedVerbOnAClassOwnedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "not yours to delete", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"delete", bead.ID}, &stdout, &stderr)
	if !handled {
		t.Fatal("an unserved mutation of a class-owned bead was handed to the bd subprocess")
	}
	if code == 0 {
		t.Error("an unserved mutation of a class-owned bead exited 0")
	}
	if !strings.Contains(stderr.String(), bead.ID) {
		t.Errorf("the refusal does not name the bead: %q", stderr.String())
	}
	if got, err := classStore.Get(bead.ID); err != nil {
		t.Fatalf("re-reading the refused bead: %v", err)
	} else if got.Status == "closed" {
		t.Error("the refused delete reached the class binding anyway")
	}
}

// TestBdByIDUnservedVerbReservedMissFallsThrough is the unserved half of the
// same rule, and it is the arm that decides whether an operator can reach their
// OWN bead at all.
//
// The refusal's claim is that the binding owns the bead and the work ledger does
// not hold it. For an id the binding cleanly misses that claim is false, and the
// spellings it captured are the whole unserved manifest — `show --long`,
// `heartbeat`, `delete` — on every bead a warned-and-allowed work-axis prefix
// mints. So ownership is proven by RESIDENCE here too, exactly as it already is
// for a work-shaped id, and a clean miss goes to the passthrough.
//
// The refusal for a bead the binding really holds is the control, and it lives
// in TestBdByIDRefusesAnUnservedVerbOnAClassOwnedBead.
func TestBdByIDUnservedVerbReservedMissFallsThrough(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	missing := reservedClassID(t, "notthere")

	for _, args := range [][]string{
		{"delete", missing},
		{"show", missing, "--long"},
		{"heartbeat", missing},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr); handled {
				t.Fatalf("%v was refused for a reserved id the binding does not hold (exit %d): %s%s", args, code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestBdByIDRefusesRatherThanFallsThroughWhenTheWorkspaceIsNotThere is the
// failure semantics against the real beads workspace provider: a binding whose
// workspace is missing produces the boot gate's own refusal, naming the
// directory, rather than a silent fall-through to bd.
//
// A read failure classified as absence is the root-loss shape this whole lane
// exists to prevent, and a fall-through is that classification in its most
// expensive form: the answer comes back confidently from the wrong ledger.
func TestBdByIDRefusesRatherThanFallsThroughWhenTheWorkspaceIsNotThere(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	writeForeignProviderCityTOML(t, cityPath, string(beadsworkspace.ProviderID), "infra")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", cityPath)
	stubInfraMigrationSource(t)
	resetCLIStorageRoutes(t)
	gateStderr := captureCLIStorageStderr(t)

	root, err := beadsworkspace.WorkspaceRoot(cityPath, "infra")
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}

	missing := reservedClassID(t, "unreachable")
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"show", missing}, &stdout, &stderr)
	if !handled {
		t.Fatal("a city whose infrastructure workspace is missing fell through to the bd subprocess")
	}
	if code == 0 {
		t.Errorf("an unreadable binding exited 0 with stdout %q", stdout.String())
	}
	// The command's OWN stderr, not the funnel's. The one-shot funnel prints
	// the boot refusal once when it takes the verdict, so a surface that
	// swallowed the read failure and reported absence would still leave the
	// workspace path on the terminal — and the test would pass while the
	// operator was told the bead does not exist.
	said := stderr.String()
	if !strings.Contains(said, root) {
		t.Errorf("the routed refusal does not name the workspace directory %s: %q", root, said)
	}
	if !strings.Contains(said, beadsworkspace.ErrWorkspaceUnavailable.Error()) {
		t.Errorf("the routed refusal does not carry %v: %q", beadsworkspace.ErrWorkspaceUnavailable, said)
	}
	if strings.Contains(said, "no issue found") {
		t.Errorf("a binding that could not be read was reported as an absent bead: %q", said)
	}
	if gateStderr.Len() == 0 {
		t.Error("the one-shot funnel took a refusing verdict without printing its reason")
	}
}

// TestBdByIDReadFailureIsAnErrorNotAbsence is the classification the whole lane
// turns on, isolated from any one provider: a class store that cannot answer
// must produce the store's error, and must never be reported as a bead that is
// not there.
//
// Absence and failure are indistinguishable to every consumer once they have
// been flattened, and the consumers act on absence — a graph-blind read that
// reported a live molecule root as missing is what produced a destructive
// restart. So the failing read is asserted twice: the cause must reach stderr,
// and bd's own absence shape must not.
func TestBdByIDReadFailureIsAnErrorNotAbsence(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	present := reservedClassID(t, "present")
	failure := errors.New("the class binding is having a bad day")
	failClassBindingReads(t, cityPath, failure)

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"show", present}, &stdout, &stderr)
	if !handled {
		t.Fatal("a failing class-store read fell through to the bd subprocess")
	}
	if code == 0 {
		t.Errorf("a failing read exited 0 with stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), failure.Error()) {
		t.Errorf("the failure does not carry the store's cause: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "no issue found") {
		t.Errorf("a failing read was reported as an absent bead: %q", stderr.String())
	}
}

// failClassBindingReads replaces this city's resolved class store with one whose
// every read fails, so a test can assert what the surface does with a store that
// cannot answer rather than one that answers emptily.
func failClassBindingReads(t *testing.T, cityPath string, cause error) {
	t.Helper()
	routes := cliStorageRoutes(cityPath)
	if routes == nil {
		t.Fatal("the city resolved no routes to fail")
	}
	store := refusedClassStore{err: cause}
	restore := make(map[coordclass.Class]beads.Store, len(routes.stores))
	for class, previous := range routes.stores {
		restore[class] = previous
		routes.stores[class] = store
	}
	dropDerivedResidencyMemo(t, cityPath)
	t.Cleanup(func() {
		for class, previous := range restore {
			routes.stores[class] = previous
		}
	})
}

// TestBdByIDLeavesAnUnrelocatedCityAlone is the compatibility claim: a city that
// authors no [storage] section routes nothing here, so every `gc bd` invocation
// takes the path it takes today.
func TestBdByIDLeavesAnUnrelocatedCityAlone(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	refuseInfraMigrationSource(t)
	captureCLIStorageStderr(t)

	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"show", reservedClassID(t, "anything")}, &stdout, &stderr); handled {
		t.Fatalf("an unrelocated city routed a by-id read here (exit %d): %s%s", code, stdout.String(), stderr.String())
	}
}

// TestBdByIDSurfaceNeverSpawnsAProcess is the property the whole surface exists
// for, asserted where it cannot rot: the file that answers a by-ID read imports
// nothing that can start one. The reported symptom was a `gc bd show` that never
// returned because the subprocess blocked on a work backend, so a routed read
// that reached for a subprocess would reintroduce it exactly.
func TestBdByIDSurfaceNeverSpawnsAProcess(t *testing.T) {
	const file = "cmd_bd_by_id.go"
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, spec := range parsed.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if path == "os/exec" || strings.HasPrefix(path, "os/exec/") {
			t.Errorf("%s imports %q; a routed by-ID read must never spawn a process", file, path)
		}
	}
}

// TestBdByIDSurfaceResolvesOneStoreNotAProviderPerOperation pins the shape of
// the resolution rather than its result: the surface asks the one-shot storage
// funnel where this city's classes are served from, exactly once per command,
// and never re-derives a destination of its own.
//
// The migration's target resolver is the one it must not use. That function
// answers "is this a binding this build can migrate onto", which is true only of
// the built-in engine — asking it is what made every other provider fall through
// — and it resolves a SQLite path this surface has no business opening.
func TestBdByIDSurfaceResolvesOneStoreNotAProviderPerOperation(t *testing.T) {
	const file = "cmd_bd_by_id.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	counts := map[string]int{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			counts[fn.Name]++
		case *ast.SelectorExpr:
			// pkg.Fn — without this arm every qualified entry in the forbidden
			// list below counted zero no matter what the file did, which made
			// the assertion about beads.OpenSQLiteStore vacuous.
			if pkg, ok := fn.X.(*ast.Ident); ok {
				counts[pkg.Name+"."+fn.Sel.Name]++
			}
			counts[fn.Sel.Name]++
		}
		return true
	})
	// The scanner must be able to see a qualified call at all, or the forbidden
	// list is a list of names nothing could ever match.
	if counts["storebinding.NewBeadsGraphStore"] == 0 {
		t.Fatal("the call scanner records no qualified calls; the forbidden list below cannot fail")
	}
	for _, forbidden := range []string{
		"resolveInfraBindingTarget",
		"openInfraDestination",
		"openStoreAtForCity",
		"openStoreAtForCityWithConfig",
		"beads.OpenSQLiteStore",
	} {
		if counts[forbidden] != 0 {
			t.Errorf("%s calls %s %d time(s); the by-ID surface must resolve its store through the storage funnel alone", file, forbidden, counts[forbidden])
		}
	}
	if counts["cliSoleClassBinding"] != 1 {
		t.Errorf("%s calls cliSoleClassBinding %d time(s), want exactly 1: one store resolution per command", file, counts["cliSoleClassBinding"])
	}
	// The two calls cliSoleClassBinding replaced. Asking the graph class
	// specifically cannot tell a whole split from a per-class fan-out, and
	// re-entering the funnel beside the resolver is how the two answers get to
	// disagree; both are now the resolver's job and neither belongs in this file.
	for _, replaced := range []string{"cliStorageRoutes", "graphClassBinding"} {
		if counts[replaced] != 0 {
			t.Errorf("%s calls %s %d time(s); the by-ID surface resolves its binding through cliSoleClassBinding alone", file, replaced, counts[replaced])
		}
	}
}

// TestBdByIDEntersTheFunnelOnlyForInvocationsThatCouldConcernAClassBead pins the
// cost gate, as a NEGATIVE about work that must not happen.
//
// Resolving the funnel loads the city config, resolves a storage plan, opens a
// binding, and on a born-split city re-proves the invariant with a full,
// unpaginated work-store census.
//
// The line this draws is SUBJECT, and it is drawn where correctness allows it
// rather than where it would be cheapest:
//
//   - A SERVED verb always enters, including on a work-shaped id, because the
//     residence probe is the only thing that finds a class-resident row: a
//     class store mints from its own workspace prefix and `gc storage migrate`
//     preserves ids, so a work-shaped id can be class-resident, and a WRITE
//     that skipped the probe would close the retained copy and leave the
//     binding's row open — the molecule stall this surface exists to remove.
//     That is why close and reopen entered the funnel with ga-axin6.
//   - An UNSERVED MUTATION enters when its positional ids can be scanned,
//     because the same id shape says nothing about residence there either, and
//     the answer decides whether the command is refused or forwarded to a
//     ledger that cannot hold the bead.
//   - A READ, a SELECTOR, and an argv whose scan is AMBIGUOUS pay nothing. The
//     first two address no subject; the third yields no ids to probe. These are
//     the hot per-tick invocations.
//
// The observer is the registry constructor, because "the routes came back nil"
// would still pass if the funnel had resolved a plan and thrown it away.
func TestBdByIDEntersTheFunnelOnlyForInvocationsThatCouldConcernAClassBead(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	reserved := reservedClassID(t, "addressed")

	for name, tc := range map[string]struct {
		args  []string
		enter bool
	}{
		"work list":                  {[]string{"list", "--status", "open"}, false},
		"quoted id in a value":       {[]string{"list", "--metadata-field", "workflow_id=" + reserved}, false},
		"ambiguous mutation scan":    {[]string{"delete", "gc-123", "--bogus", "v"}, false},
		"mutation with no id":        {[]string{"delete", "--from-file", "ids.txt"}, false},
		"class mutation":             {[]string{"update", reserved, "--status", "closed"}, true},
		"class close":                {[]string{"close", reserved}, true},
		"class id in an id flag":     {[]string{"list", "--parent", reserved}, true},
		"unserved dep tree spelling": {[]string{"dep", "tree", "--show-all-paths", "gc-123"}, false},
		"served read on a work id":   {[]string{"show", "gc-123"}, true},
		"served dep tree":            {[]string{"dep", "tree", "gc-123"}, true},
		"served write on a work id":  {[]string{"update", "gc-123", "--status", "closed"}, true},
		"served close on a work id":  {[]string{"close", "gc-123"}, true},
		"served reopen on a work id": {[]string{"reopen", "gc-123"}, true},
		"unserved work delete":       {[]string{"delete", "gc-123"}, true},
		"unserved update spelling":   {[]string{"update", "gc-123", "--notes", "x"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			resetCLIStorageRoutes(t)
			registries := countStorageRegistryConstructions(t)
			var stdout, stderr bytes.Buffer
			maybeRouteBdByID(cityPath, "", tc.args, &stdout, &stderr)
			entered := *registries > 0
			if entered != tc.enter {
				t.Errorf("%v entered the storage funnel = %t, want %t", tc.args, entered, tc.enter)
			}
		})
	}
}

// TestBdByIDSurfaceServesAClosedVerbSet pins what this surface answers. Growing
// it silently is how a partial imitation of bd ships: a verb parsed here but
// only half implemented answers a different question than the one asked.
func TestBdByIDSurfaceServesAClosedVerbSet(t *testing.T) {
	served := map[bdByIDVerb]bool{
		bdByIDShow:    true,
		bdByIDClaim:   true,
		bdByIDRelease: true,
		bdByIDDepList: true,
		bdByIDDepTree: true,
		bdByIDUpdate:  true,
		bdByIDClose:   true,
		bdByIDReopen:  true,
	}
	for _, args := range [][]string{
		{"show", "gcg-1"},
		{"update", "gcg-1", "--claim"},
		{"release-if-current", "gcg-1", "someone"},
		{"dep", "list", "gcg-1"},
		{"dep", "list", "gcg-1", "-t", "blocks"},
		{"dep", "list", "gcg-1", "--direction=up"},
		{"dep", "tree", "gcg-1"},
		{"dep", "tree", "gcg-1", "--json"},
		{"dep", "tree", "gcg-1", "--reverse"},
		{"dep", "tree", "gcg-1", "--direction=up", "--max-depth", "3"},
		{"update", "gcg-1", "--status", "closed"},
		{"update", "gcg-1", "--set-metadata", "gc.outcome=pass", "--status=closed"},
		{"close", "gcg-1"},
		{"close", "gcg-1", "--json"},
		{"reopen", "gcg-1"},
		{"reopen", "gcg-1", "--json"},
	} {
		op, ok := parseBdByIDOp(args)
		if !ok {
			t.Fatalf("%v is not recognized by the by-ID parser", args)
		}
		if !served[op.Verb] {
			t.Errorf("%v parsed to unserved verb %q", args, op.Verb)
		}
	}
	for _, args := range [][]string{
		{"show"},
		{"show", "gcg-1", "gcg-2"},
		{"show", "gcg-1", "--unknown"},
		{"show", "gcg-1", "--long"},
		{"show", "--id", "gcg-1"},
		{"update", "gcg-1"},
		{"update", "gcg-1", "--notes", "hello"},
		{"dep", "list"},
		{"dep", "list", "gcg-1", "--direction=sideways"},
		// dep tree spellings this walk does not implement. Serving them by
		// dropping the flag would answer a different question than the one asked:
		// --show-all-paths asks for every path to a bead the walk visits once,
		// --status and --format reshape the result, and --direction=both merges
		// two walks. Each stays unserved so it meets the ownership refusal.
		{"dep", "tree"},
		{"dep", "tree", "gcg-1", "gcg-2"},
		{"dep", "tree", "gcg-1", "--show-all-paths"},
		{"dep", "tree", "gcg-1", "--status", "open"},
		{"dep", "tree", "gcg-1", "--format", "dot"},
		{"dep", "tree", "gcg-1", "--direction=both"},
		{"dep", "tree", "gcg-1", "--direction=sideways"},
		{"dep", "tree", "gcg-1", "--max-depth", "0"},
		{"close"},
		{"close", "gcg-1", "gcg-2"},
		{"close", "gcg-1", "--reason", "done"},
		{"close", "gcg-1", "--force"},
		{"reopen", "gcg-1", "-r", "wrong call"},
		{"delete", "gcg-1"},
		{"list"},
	} {
		if op, ok := parseBdByIDOp(args); ok {
			t.Errorf("%v was recognized as %q, want the caller's existing path", args, op.Verb)
		}
	}
}

// TestBdByIDRefusesUnservedSpellingsOfAClassOwnedBead is the flagged-spelling
// fallthrough, which is the hole the closed verb set opens if ownership is
// decided by "can this be served".
//
// Each of these is a real bd spelling from the tree's own manifest
// (internal/bdflags) that the served parsers reject. While a rejection meant
// fall-through, every one of them ran against the work ledger — so the flags an
// operator reaches for when the terse answer is not enough were exactly the ones
// that answered about the wrong workspace.
//
// The `--metadata-field` row is the boundary: an id quoted in a filter VALUE is
// a work question and must still reach bd, or the consumer that asks it
// exec-fails on every tick.
func TestBdByIDRefusesUnservedSpellingsOfAClassOwnedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "addressed", Type: "task"})
	quoted := reservedClassID(t, "quoted")

	refused := [][]string{
		{"show", bead.ID, "--long"},
		{"show", bead.ID, "--short"},
		{"show", bead.ID, "--children"},
		{"show", bead.ID, "--refs"},
		{"show", bead.ID, "--thread"},
		{"show", bead.ID, "--include-comments"},
		{"show", bead.ID, "--include-dependents"},
		{"show", bead.ID, "--as-of", "2026-01-01"},
		{"show", "--id", bead.ID},
		{"dep", "tree", bead.ID, "--show-all-paths"},
		{"dep", "tree", bead.ID, "--status", "open"},
		{"dep", "tree", bead.ID, "--format", "dot"},
		{"dep", "tree", bead.ID, "--direction=both"},
		{"delete", bead.ID},
		{"update", bead.ID, "--notes", "a note"},
		{"close", bead.ID, "--reason", "done"},
		// A root flag BEFORE the verb is bd's own spelling and gc forwards argv
		// verbatim, so the served parsers — which key on argv[0] — do not
		// recognize it. Ownership still does, which is the whole point of
		// deciding the two questions separately.
		{"--json", "close", bead.ID},
		{"list", "--parent", bead.ID},
	}
	for _, args := range refused {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr)
			if !handled {
				t.Fatalf("%v fell through to the bd subprocess", args)
			}
			if code == 0 {
				t.Errorf("%v exited 0; stdout=%q", args, stdout.String())
			}
			if !strings.Contains(stderr.String(), bead.ID) {
				t.Errorf("the refusal does not name the bead: %q", stderr.String())
			}
		})
	}

	// dep list's own selectors are SERVED rather than refused — all three
	// spellings bd accepts, including the short `-t`.
	for _, args := range [][]string{
		{"dep", "list", bead.ID, "-t", "blocks"},
		{"dep", "list", bead.ID, "--type", "blocks"},
		{"dep", "list", bead.ID, "--direction", "up"},
		{"dep", "list", bead.ID, "-t=blocks"},
	} {
		t.Run("served "+strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr)
			if !handled {
				t.Fatalf("%v fell through to the bd subprocess", args)
			}
			if code != 0 {
				t.Errorf("%v exited %d: %s", args, code, stderr.String())
			}
		})
	}

	for _, args := range [][]string{
		{"list", "--metadata-field", "workflow_id=" + quoted},
		{"list", "--label", quoted},
		{"list", "--metadata-field=workflow_id=" + quoted},
	} {
		t.Run("passthrough "+strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr); handled {
				t.Errorf("%v was answered here (exit %d): %s%s", args, code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestBdByIDRefusalNamesTheUnrepresentableFlag pins the actionable half of the
// update refusal. `--notes` has no representation in the object model — there is
// no notes field on beads.Bead and no notes write on beads.UpdateOpts — so the
// step-completion write the core pack makes cannot be served faithfully, and an
// operator has to be told WHICH flag stopped it rather than that `gc bd update`
// is "not served", which is false in general.
func TestBdByIDRefusalNamesTheUnrepresentableFlag(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "step", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", bead.ID, "--set-metadata", "gc.outcome=pass", "--status=closed", "--notes", "done"}, &stdout, &stderr)
	if !handled || code == 0 {
		t.Fatalf("the unrepresentable update was not refused: (%d, %t) %s", code, handled, stderr.String())
	}
	if !strings.Contains(stderr.String(), bdByIDUpdateUnrepresentable) {
		t.Errorf("the refusal does not name %s: %q", bdByIDUpdateUnrepresentable, stderr.String())
	}
	// Refused means refused: nothing may have been written.
	after, err := classStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading the refused bead: %v", err)
	}
	if after.Status == "closed" || len(after.Metadata) != 0 {
		t.Errorf("the refused update wrote anyway: status=%q metadata=%v", after.Status, after.Metadata)
	}
}

// reservedClassID builds an id in the reserved class namespace, so a test can
// name a bead only a class store could own without minting one.
func reservedClassID(t *testing.T, suffix string) string {
	t.Helper()
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		t.Fatalf("no reserved id prefix is registered for the %q class", config.BeadClassGraph)
	}
	return prefix + "-" + suffix
}

// The tests below cover the write-orphaned class resident: a bead whose id
// carries a WORK prefix but whose only row lives in the class binding.
//
// A converged city holds this population two ways — the migration carried the
// pre-split rows across with their ids PRESERVED, and a class-store Create with
// no id mints from the binding workspace's own prefix, which on such a city is
// still a work prefix. Either way it is write-unreachable without a residence
// lane: reads reach it (the door probes the class leg for every id) while writes
// route by prefix at a ledger that never held the row.

// classResidentWorkShapedBead seeds a bead with an explicit WORK-shaped id into
// the class binding only, and proves the id carries no reserved prefix so the
// test exercises the residence probe rather than the prefix rule.
//
// It seeds through the foreign-id create because that is the only way a pinned
// foreign id enters a binding — the ordinary create fences them out, which is
// what stops a live subsystem from writing one there by accident. Pinning the id
// rather than taking a minted one is what lets the tests name it.
func classResidentWorkShapedBead(t *testing.T, classStore beads.Store, id, title string) beads.Bead {
	t.Helper()
	created, err := migrationSeed(classStore, beads.Bead{ID: id, Title: title, Type: "task"})
	if err != nil {
		t.Fatalf("seeding %s in the class binding: %v", id, err)
	}
	if bdIDIsClassReserved(created.ID) {
		t.Fatalf("the fixture id %q carries a reserved class prefix; it cannot exercise the residence probe", created.ID)
	}
	return created
}

// migrationSeed writes a bead into a binding the way `gc storage migrate` does:
// through the store's foreign-id create, which keeps a preserved id that the
// ordinary create fences out.
func migrationSeed(store beads.Store, b beads.Bead) (beads.Bead, error) {
	creator, ok := store.(beads.ForeignIDCreator)
	if !ok {
		return beads.Bead{}, fmt.Errorf("%T cannot create with a foreign id, so it cannot hold a bead the migration carried across", store)
	}
	return creator.CreateWithForeignID(b)
}

// workStoreFor opens the city's own work ledger — the store the bd subprocess
// is pointed at — so a test can assert what a routed write did NOT do there.
func workStoreFor(t *testing.T, cityPath string) beads.Store {
	t.Helper()
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("opening the work store at %s: %v", cityPath, err)
	}
	return store
}

// TestBdCloseServesClassResidentWorkPrefixedBead is the ga-axin6 regression.
//
// 793 open beads on the measured city carry a work prefix and reside only in
// the coordination-class store. `gc bd close` was not a by-ID verb at all, so
// the invocation never opened the class door: it fell through to a bd
// subprocess pointed at the prefix store, which reported "no issue found" after
// a managed-backend connect. There was no residency lane for close — the miss
// was structural, not a misfire — and every such bead was a permanent
// ready-frontier polluter with no supported drain path.
func TestBdCloseServesClassResidentWorkPrefixedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", relic.ID}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident work-shaped id fell through to the bd subprocess, which opens the ledger that never held it: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("closing %s exited %d: %s", relic.ID, code, stderr.String())
	}
	after, err := classStore.Get(relic.ID)
	if err != nil {
		t.Fatalf("re-reading the closed relic: %v", err)
	}
	if after.Status != "closed" {
		t.Errorf("status = %q, want closed", after.Status)
	}
	if !strings.Contains(stderr.String(), "served in process") {
		t.Errorf("the routed close does not say where it was served from: %q", stderr.String())
	}
	// The work ledger is where the passthrough sent this command. It never held
	// the bead, and a routed close must not have minted one there either.
	if _, err := workStoreFor(t, cityPath).Get(relic.ID); err == nil {
		t.Errorf("the work store holds %s after a routed close; the write reached the ledger the bead was never in", relic.ID)
	}
}

// TestBdClosePrefixStoreBeadKeepsPassthrough is T1's control, and it must fail
// DIFFERENTLY: T1 asserts the door answered, this asserts the passthrough is
// still reached. A residence probe that turned into an unconditional route to
// the binding would pass T1 and fail here.
func TestBdClosePrefixStoreBeadKeepsPassthrough(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	normal, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	if _, err := classStore.Get(normal.ID); err == nil {
		t.Fatalf("the class binding also holds %s; the control proves nothing", normal.ID)
	}

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", normal.ID}, &stdout, &stderr)
	if handled {
		t.Fatalf("a work-resident id was answered by the door (exit %d): %s%s", code, stdout.String(), stderr.String())
	}
	for _, b := range allBeads(t, classStore) {
		t.Errorf("the class binding holds %s (%q) after a passthrough close; the probe must read, never write", b.ID, b.Title)
	}
}

// TestBdCloseReservedPrefixServedInProcess flips the reserved-prefix arm from a
// refusal to a served close. The old behavior is pinned by
// TestBdByIDRefusesUnservedSpellingsOfAClassOwnedBead, which loses its `close`
// row here.
//
// A binding MISS takes the residual rule instead: the binding is the namespace's
// authority and not its only lawful holder, so a close of an id it does not hold
// goes to the bd passthrough, whose own not-found is then the answer.
func TestBdCloseReservedPrefixServedInProcess(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "a reserved-prefix step", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", bead.ID}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("closing the reserved-prefix bead %s = (%d, %t): %s", bead.ID, code, handled, stderr.String())
	}
	after, err := classStore.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", bead.ID, err)
	}
	if after.Status != "closed" {
		t.Errorf("status = %q, want closed", after.Status)
	}

	missing := reservedClassID(t, "notthere")
	stdout.Reset()
	stderr.Reset()
	if code, handled := maybeRouteBdByID(cityPath, "", []string{"close", missing}, &stdout, &stderr); handled {
		t.Fatalf("closing a reserved-prefix id the binding does not hold was answered here (exit %d): %s%s", code, stdout.String(), stderr.String())
	}
}

// TestBdCloseUnrepresentableFlagStaysOffTheDoor pins the closed-contract floor.
//
// storebinding.GraphStore.Close takes an id and nothing else, so bd's
// --reason/--reason-file/--session carry text the store has nowhere to put, and
// --claim-next/--continue/--force/--no-auto/--suggest-next name workflow this
// arm does not run. Serving any of them by dropping it would report a command
// as executed after silently changing what it meant — the partial-write failure
// this whole surface exists to remove.
//
// Unserved is not forwarded. Residency proves ownership for these too, so each
// one is refused with the offending spelling named rather than handed to a
// ledger that would answer bd's misleading not-found about it.
func TestBdCloseUnrepresentableFlagStaysOffTheDoor(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")

	for _, tc := range []struct {
		args []string
		flag string
	}{
		{[]string{"close", "--reason", "done", relic.ID}, "--reason"},
		{[]string{"close", relic.ID, "--reason-file", "/tmp/why"}, "--reason-file"},
		{[]string{"close", relic.ID, "--claim-next"}, "--claim-next"},
		{[]string{"close", relic.ID, "--force"}, "--force"},
		{[]string{"reopen", relic.ID, "-r", "wrong call"}, "-r"},
		// A batch names no flag: the shape is what kept it off the surface, and
		// naming an id there would read as a claim about that id.
		{[]string{"close", relic.ID, "gc-relic2"}, ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if op, ok := parseBdByIDOp(tc.args); ok {
				t.Fatalf("%v was recognized as %q; a spelling the closed contract cannot represent must not be served", tc.args, op.Verb)
			}
			var stdout, stderr bytes.Buffer
			code, handled := maybeRouteBdByID(cityPath, "", tc.args, &stdout, &stderr)
			if !handled {
				t.Fatalf("%v fell through to the bd subprocess, which cannot hold %s", tc.args, relic.ID)
			}
			if code == 0 {
				t.Errorf("%v exited 0: %s", tc.args, stdout.String())
			}
			if !strings.Contains(stderr.String(), relic.ID) {
				t.Errorf("the refusal does not name the resident bead: %q", stderr.String())
			}
			if tc.flag != "" && !strings.Contains(stderr.String(), tc.flag) {
				t.Errorf("the refusal does not name %s: %q", tc.flag, stderr.String())
			}
			after, err := classStore.Get(relic.ID)
			if err != nil {
				t.Fatalf("re-reading %s: %v", relic.ID, err)
			}
			if after.Status == "closed" {
				t.Errorf("%v wrote to the class binding anyway", tc.args)
			}
		})
	}
}

// TestBdReopenServesClassResident is T1's mirror. reopen is the undo of the
// sweep this fix enables, and a drain that can close a resident but not reopen
// one is a one-way door.
func TestBdReopenServesClassResident(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "closed too eagerly")
	if err := classStore.Close(relic.ID); err != nil {
		t.Fatalf("pre-closing the relic: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"reopen", relic.ID}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident reopen fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("reopening %s exited %d: %s", relic.ID, code, stderr.String())
	}
	after, err := classStore.Get(relic.ID)
	if err != nil {
		t.Fatalf("re-reading the reopened relic: %v", err)
	}
	if after.Status == "closed" {
		t.Errorf("status = %q; the routed reopen did not land", after.Status)
	}
}

// TestBdCloseAlreadyClosedIsStoreContractNoOp pins that the already-closed
// answer is the STORE's and is not re-derived here. The routed arm calls
// Close and renders what the store then holds; a CLI-side "is it already
// closed" check would be a second implementation of a contract the store
// already has, and the two would drift.
func TestBdCloseAlreadyClosedIsStoreContractNoOp(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "closed twice")

	for i := 1; i <= 2; i++ {
		var stdout, stderr bytes.Buffer
		code, handled := maybeRouteBdByID(cityPath, "", []string{"close", relic.ID}, &stdout, &stderr)
		if !handled {
			t.Fatalf("close #%d fell through to the bd subprocess: %q", i, stderr.String())
		}
		if code != 0 {
			t.Fatalf("close #%d exited %d: %s", i, code, stderr.String())
		}
		after, err := classStore.Get(relic.ID)
		if err != nil {
			t.Fatalf("re-reading after close #%d: %v", i, err)
		}
		if after.Status != "closed" {
			t.Fatalf("status after close #%d = %q, want closed (no status regression)", i, after.Status)
		}
	}
}

// TestBdCloseDualResidentWritesServingCopy pins the multi-residency rule: a
// write lands in the first store of the surface's OWN by-id probe order whose
// Get answers — the same order that surface's reads use.
//
// The door's order is [class binding, work-store-via-subprocess], so the class
// copy is both the copy `gc bd show` serves and the copy `gc bd close` writes.
// Read/write coherence on one surface is the property being pinned here.
//
// The API surface now reaches the same copy: its ByID plan leads with the
// binding as a residence probe (internal/api's residency_by_id.go), so both
// surfaces read and write the row the controller reads rather than the
// migration's retained one. Draining the duplicate copies is still the sweep's
// job — agreement is about which copy is authoritative, not about there being
// one.
//
// The clauses live in storereftest so this test and the API's
// TestBeadDualResidentAnswersFromTheBinding assert the SAME sentences. That is
// the whole point: two surfaces can each pass their own pin and still disagree
// about which copy of one id is real, and a shared property is the only thing
// that catches it.
func TestBdCloseDualResidentWritesServingCopy(t *testing.T) {
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
			Name: "the gc bd by-id class door",
			Get:  showThroughTheClassDoor(cityPath, work),
			Close: func(t *testing.T, id string) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				code, handled := maybeRouteBdByID(cityPath, "", []string{"close", id}, &stdout, &stderr)
				if !handled || code != 0 {
					t.Fatalf("closing %s = (%d, %t): %s", id, code, handled, stderr.String())
				}
			},
		})
}

// showThroughTheClassDoor adapts `gc bd show --json` to the shared property's
// Get.
//
// The door answers only for ids it resolves to a binding; for anything else it
// returns handled=false and the real command falls through to a bd subprocess
// pointed at the city's work ledger. A test cannot run that subprocess, so this
// reads the work store the subprocess would have been given — which is what
// makes the control clause an assertion about the DOOR rather than about bd: it
// holds because the door DECLINED, and a door that started claiming ids no
// binding holds would answer here from the binding and fail.
func showThroughTheClassDoor(cityPath string, work beads.Store) func(*testing.T, string) beads.Bead {
	return func(t *testing.T, id string) beads.Bead {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code, handled := maybeRouteBdByID(cityPath, "", []string{"show", id, "--json"}, &stdout, &stderr)
		if !handled {
			bead, err := work.Get(id)
			if err != nil {
				t.Fatalf("the door declined %s and the work ledger it falls through to cannot serve it either: %v", id, err)
			}
			return bead
		}
		if code != 0 {
			t.Fatalf("the routed show of %s exited %d: %s", id, code, stderr.String())
		}
		var shown []beads.Bead
		if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
			t.Fatalf("decoding the routed show %q: %v", stdout.String(), err)
		}
		if len(shown) != 1 {
			t.Fatalf("the routed show of %s returned %d beads, want exactly one", id, len(shown))
		}
		return shown[0]
	}
}

// TestBdCloseRigFlagRefusedForClassResident keeps the new verbs inside the
// existing --rig rule: the flag names a WORK scope, a relocated class is not
// partitioned by rig, and serving it anyway would ignore a flag the operator
// reached for to be MORE specific.
func TestBdCloseRigFlagRefusedForClassResident(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")

	for _, args := range [][]string{{"close", relic.ID}, {"reopen", relic.ID}} {
		t.Run(args[0], func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := maybeRouteBdByID(cityPath, "r1", args, &stdout, &stderr)
			if !handled || code == 0 {
				t.Fatalf("%v under --rig r1 = (%d, %t): %s", args, code, handled, stderr.String())
			}
			if !strings.Contains(stderr.String(), "--rig r1") {
				t.Errorf("the refusal does not name the pinned rig: %q", stderr.String())
			}
			after, err := classStore.Get(relic.ID)
			if err != nil {
				t.Fatalf("re-reading %s: %v", relic.ID, err)
			}
			if after.Status == "closed" {
				t.Errorf("the refused %v wrote to the class binding anyway", args)
			}
		})
	}
}

// TestBdCloseSingleStoreCityByteIdentical is the compatibility row: a city that
// authors no [storage] routes nothing here, so `gc bd close` reaches the
// passthrough exactly as it does today — and pays nothing for the privilege.
// The registry counter is the observer, because "handled=false" would also hold
// if the funnel had resolved a plan and thrown it away.
func TestBdCloseSingleStoreCityByteIdentical(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	refuseInfraMigrationSource(t)
	captureCLIStorageStderr(t)
	registries := countStorageRegistryConstructions(t)

	for _, args := range [][]string{{"close", "gc-1"}, {"reopen", "gc-1"}, {"close", reservedClassID(t, "anything")}} {
		var stdout, stderr bytes.Buffer
		if code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr); handled {
			t.Errorf("an unrelocated city answered %v here (exit %d): %s%s", args, code, stdout.String(), stderr.String())
		}
	}
	if *registries != 0 {
		t.Errorf("an unrelocated city constructed %d provider registr(ies) for a close; the bypass must short-circuit first", *registries)
	}
}

// TestBdCloseUnopenableBindingSurfacesNotAbsence is invariant 3 on the write
// path. A class store that cannot answer must produce the store's own error:
// reading "the binding could not be read" as "the bead is not there" is the
// root-loss shape, and on a WRITE it additionally means the command silently
// moves to the ledger that cannot hold the bead.
func TestBdCloseUnopenableBindingSurfacesNotAbsence(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")
	failure := errors.New("the class binding is having a bad day")
	failClassBindingReads(t, cityPath, failure)

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", relic.ID}, &stdout, &stderr)
	if !handled {
		t.Fatal("a failing class-store read fell through to the bd subprocess, which cannot hold the bead")
	}
	if code == 0 {
		t.Errorf("a failing read exited 0 with stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), failure.Error()) {
		t.Errorf("the failure does not carry the store's cause: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "no issue found") {
		t.Errorf("a failing read was reported as an absent bead: %q", stderr.String())
	}
}

// TestBdCloseClassResidentEnforcesWorkRecordGate proves the class-door close
// runs the ADR-0009 work-record gate against the class copy it is about to
// write. A class-resident work step (a plain task bead, no gc.kind) that lacks a
// typed gc.work_outcome must be BLOCKED under enforcement rather than retired
// with no outcome — and the block must land BEFORE the write, so the class row
// stays open. This is the codex major finding: the routed close returned before
// the gate at cmd_bd.go:372 could run.
func TestBdCloseClassResidentEnforcesWorkRecordGate(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	// Set enforcement AFTER foreignProviderCity: clearGCEnv wipes live GC_* keys.
	t.Setenv(workRecordEnforceEnvVar, "1")
	relic := classResidentWorkShapedBead(t, classStore, "gc-nooutcome1", "closed without a work outcome")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", relic.ID}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident close fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 1 {
		t.Fatalf("close of an outcome-less work step exited %d, want 1 (blocked): %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "work-record gate (enforced)") {
		t.Errorf("the block does not carry the enforced-gate marker: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing "+beadmeta.WorkOutcomeMetadataKey) {
		t.Errorf("the block does not name the missing outcome: %q", stderr.String())
	}
	after, err := classStore.Get(relic.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", relic.ID, err)
	}
	if after.Status == "closed" {
		t.Errorf("the blocked close wrote to the class binding anyway; status=%q", after.Status)
	}
}

// TestBdUpdateStatusClosedClassResidentEnforcesWorkRecordGate is the same gate
// on the other close spelling — `gc bd update <id> --status closed`, the form
// the worker formulas use to stamp metadata and close in one call. It must be
// gated identically: an outcome-less update-close is blocked and does not write.
func TestBdUpdateStatusClosedClassResidentEnforcesWorkRecordGate(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	t.Setenv(workRecordEnforceEnvVar, "1")
	relic := classResidentWorkShapedBead(t, classStore, "gc-nooutcome2", "update-closed without an outcome")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", relic.ID, "--status", "closed"}, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident update-close fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 1 {
		t.Fatalf("update --status closed on an outcome-less work step exited %d, want 1 (blocked): %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing "+beadmeta.WorkOutcomeMetadataKey) {
		t.Errorf("the block does not name the missing outcome: %q", stderr.String())
	}
	after, err := classStore.Get(relic.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", relic.ID, err)
	}
	if after.Status == "closed" {
		t.Errorf("the blocked update-close wrote to the class binding anyway; status=%q", after.Status)
	}
}

// TestBdUpdateAtomicNoOpClassResidentPassesWorkRecordGate proves the gate does
// not over-block: the documented worker close — stamp the typed outcome and
// close in one atomic update — is ALLOWED under enforcement, and the class row
// is retired with the outcome persisted. Validating the pre-update bead alone
// would wrongly reject this, so the gate must project the submitted metadata.
func TestBdUpdateAtomicNoOpClassResidentPassesWorkRecordGate(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	t.Setenv(workRecordEnforceEnvVar, "1")
	relic := classResidentWorkShapedBead(t, classStore, "gc-outcome1", "closed with a no-op outcome")

	args := []string{
		"update", relic.ID,
		"--set-metadata", beadmeta.WorkOutcomeMetadataKey + "=" + beadmeta.WorkOutcomeNoOp,
		"--status", "closed",
	}
	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr)
	if !handled {
		t.Fatalf("a class-resident atomic close fell through to the bd subprocess: %q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("a compliant atomic close was blocked (exit %d): %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "work-record gate") {
		t.Errorf("a compliant close still tripped the gate: %q", stderr.String())
	}
	after, err := classStore.Get(relic.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", relic.ID, err)
	}
	if after.Status != "closed" {
		t.Errorf("the compliant close did not retire the class row; status=%q", after.Status)
	}
	if got := after.Metadata[beadmeta.WorkOutcomeMetadataKey]; got != beadmeta.WorkOutcomeNoOp {
		t.Errorf("the atomic close did not stamp the outcome; %s=%q", beadmeta.WorkOutcomeMetadataKey, got)
	}
}

// TestBdCloseClassResidentWarnsOnlyByDefault keeps the migration default: with
// enforcement OFF, an outcome-less close WARNS but still proceeds, so the open
// work steps a migrated city already holds drain without breakage.
func TestBdCloseClassResidentWarnsOnlyByDefault(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	// Enforcement deliberately unset (foreignProviderCity's clearGCEnv left it so).
	relic := classResidentWorkShapedBead(t, classStore, "gc-warnonly1", "closed without an outcome, warn-only")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", relic.ID}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("warn-only close = (%d, %t): %s", code, handled, stderr.String())
	}
	if !strings.Contains(stderr.String(), "work-record gate (warn-only)") {
		t.Errorf("the warn-only close did not warn: %q", stderr.String())
	}
	after, err := classStore.Get(relic.ID)
	if err != nil {
		t.Fatalf("re-reading %s: %v", relic.ID, err)
	}
	if after.Status != "closed" {
		t.Errorf("warn-only did not proceed with the close; status=%q", after.Status)
	}
}

// TestBdUpdateUnservedWorkResidentFailsLoudOnClassProbeFault pins, deliberately,
// the blast-radius minor: an ordinary WORK-bead unserved mutation fails loud
// when the class-store residence probe faults with a NON-refusal read error,
// rather than silently falling through to bd. It is the unserved-path mirror of
// TestBdCloseUnopenableBindingSurfacesNotAbsence — a mid-query fault is a failure
// to DECIDE ownership, and treating it as absence is the root-loss shape this
// lane exists to prevent. A standing refusal is the one error treated as a miss;
// a fault is not, so the command surfaces it instead of guessing the ledger.
func TestBdUpdateUnservedWorkResidentFailsLoudOnClassProbeFault(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	resident, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	failure := errors.New("the class binding faulted mid-probe")
	failClassBindingReads(t, cityPath, failure)

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"update", resident.ID, "--notes", "x"}, &stdout, &stderr)
	if !handled {
		t.Fatal("a class-probe fault let an unserved work mutation fall through to the bd subprocess")
	}
	if code == 0 {
		t.Errorf("a class-probe fault exited 0 with stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), failure.Error()) {
		t.Errorf("the failure does not carry the store's cause: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "no issue found") {
		t.Errorf("a class-probe fault was reported as an absent bead: %q", stderr.String())
	}
}

// TestBdUpdateUnservedSpellingOnResidentRefusesNamingFlag is the residency half
// of "ownership is decided before servability".
//
// Ownership used to be provable only from a RESERVED prefix in the argv, so a
// work-prefixed class resident got no protection at all: its unserved mutation
// fell through to a ledger that cannot hold it and died with bd's not-found —
// the same wrong-store error, the same wall-clock cost, and a diagnosis that
// sends an operator to look for a bead that is not missing. Residency proves
// ownership just as well, and the refusal names the flag.
func TestBdUpdateUnservedSpellingOnResidentRefusesNamingFlag(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")
	normal, err := workStoreFor(t, cityPath).Create(beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}

	// Every unserved mutation verb, not just update. `delete` is the one this
	// surface deliberately never serves, and it carries no flag to name — the
	// VERB is what cannot be represented — so it is the row that proves the
	// refusal comes from residency rather than from flag rejection.
	for _, tc := range []struct {
		args []string
		flag string
	}{
		{[]string{"update", relic.ID, "--notes", "x"}, bdByIDUpdateUnrepresentable},
		{[]string{"delete", relic.ID}, ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := maybeRouteBdByID(cityPath, "", tc.args, &stdout, &stderr)
			if !handled {
				t.Fatalf("an unserved mutation of a class resident fell through to the bd subprocess: %q", stderr.String())
			}
			if code == 0 {
				t.Errorf("the refusal exited 0: %q", stdout.String())
			}
			for _, want := range []string{relic.ID, "class binding"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the refusal does not name %q: %q", want, stderr.String())
				}
			}
			if tc.flag != "" && !strings.Contains(stderr.String(), tc.flag) {
				t.Errorf("the refusal does not name %s: %q", tc.flag, stderr.String())
			}
			if strings.Contains(stderr.String(), "no issue found") {
				t.Errorf("the refusal wears bd's absence shape: %q", stderr.String())
			}
			after, err := classStore.Get(relic.ID)
			if err != nil {
				t.Fatalf("re-reading %s: %v", relic.ID, err)
			}
			if len(after.Metadata) != 0 || after.Status == "closed" {
				t.Errorf("the refused mutation wrote anyway: status=%q metadata=%v", after.Status, after.Metadata)
			}
		})

		// The control: the same spelling against a bead the class binding does
		// NOT hold keeps its existing path, so the residency check cannot have
		// become a blanket refusal of unserved mutations.
		control := append([]string{}, tc.args...)
		for i, arg := range control {
			if arg == relic.ID {
				control[i] = normal.ID
			}
		}
		t.Run("control "+strings.Join(control, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code, handled := maybeRouteBdByID(cityPath, "", control, &stdout, &stderr); handled {
				t.Errorf("an unserved mutation of a work-resident bead was refused here (exit %d): %s%s", code, stdout.String(), stderr.String())
			}
		})
	}
}

// TestBdMutationAmbiguousScanNeverEntersFunnel keeps the widened cost gate off
// the one argv shape it cannot read.
//
// An unrecognized flag may or may not consume the next token, so
// bdMutationWriteIDs reports ambiguity rather than a guess — and an ambiguous
// scan yields no ids to probe residence for. Entering the funnel to probe
// nothing would be pure cost, and doBd's own fail-closed guard is what answers
// that argv.
func TestBdMutationAmbiguousScanNeverEntersFunnel(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")

	args := []string{"close", relic.ID, "--bogus", "value"}
	if _, _, ambiguous := bdMutationWriteIDs(args); !ambiguous {
		t.Fatalf("bdMutationWriteIDs(%v) is not ambiguous; this fixture cannot exercise the gate", args)
	}

	resetCLIStorageRoutes(t)
	registries := countStorageRegistryConstructions(t)
	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr); handled {
		t.Errorf("%v was answered here (exit %d): %s%s", args, code, stdout.String(), stderr.String())
	}
	if *registries != 0 {
		t.Errorf("an ambiguous mutation scan constructed %d provider registr(ies); it has no ids to probe", *registries)
	}
}

// TestBdSelectorVerbsNeverEnterFunnel is the other half of the widened gate,
// and the line it draws: a MUTATION addressing ids enters, a selector or a read
// this surface does not serve never does.
//
// A selector quotes ids rather than addressing them — `--metadata-field
// workflow_id=<id>` selects rows by a field they carry — so there is no subject
// whose residence could decide anything, and these are the hot per-tick
// invocations that must keep paying nothing.
//
// A SERVED read is not in this list and never was: `show` addresses its subject
// and has to probe residence to find a class-resident row, which is why
// TestBdByIDEntersTheFunnelOnlyForInvocationsThatCouldConcernAClassBead expects
// it to enter. `dep tree` moved into that company under ga-pxppl, so what stands
// here for it is the spelling the in-process walk does NOT implement — still a
// read, still free.
func TestBdSelectorVerbsNeverEnterFunnel(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")

	for _, args := range [][]string{
		{"list", "--metadata-field", "workflow_id=" + relic.ID},
		{"list", "--label", relic.ID},
		{"list", "--status", "open"},
		{"search", relic.ID},
		{"dep", "tree", relic.ID, "--show-all-paths"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetCLIStorageRoutes(t)
			registries := countStorageRegistryConstructions(t)
			var stdout, stderr bytes.Buffer
			if code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr); handled {
				t.Errorf("%v was answered here (exit %d): %s%s", args, code, stdout.String(), stderr.String())
			}
			if *registries != 0 {
				t.Errorf("%v constructed %d provider registr(ies); a read or selector must pay nothing", args, *registries)
			}
		})
	}
}

// getOnlyClassStore answers reads from a real store and fails every write with
// a fixed error, which is the shape of a binding whose read leg and write leg
// resolve against different tiers (a cache/SQLite read in front of a
// bd-exec/Dolt write that misses the row).
type getOnlyClassStore struct {
	beads.Store
	writeErr error
}

func (s getOnlyClassStore) Update(string, beads.UpdateOpts) error { return s.writeErr }
func (s getOnlyClassStore) Close(string) error                    { return s.writeErr }
func (s getOnlyClassStore) Reopen(string) error                   { return s.writeErr }

// stubClassBindingStore replaces this city's resolved class stores with store,
// for the whole of one test.
func stubClassBindingStore(t *testing.T, cityPath string, store beads.Store) {
	t.Helper()
	routes := cliStorageRoutes(cityPath)
	if routes == nil {
		t.Fatal("the city resolved no routes to substitute")
	}
	restore := make(map[coordclass.Class]beads.Store, len(routes.stores))
	for class, previous := range routes.stores {
		restore[class] = previous
		routes.stores[class] = store
	}
	dropDerivedResidencyMemo(t, cityPath)
	t.Cleanup(func() {
		for class, previous := range restore {
			routes.stores[class] = previous
		}
	})
}

// TestDoorUpdateAfterFoundSurfacesStoreErrorVerbatim pins the OTHER proximate
// cause of the reported write failure, which is not a routing defect at all.
//
// If a binding's Get answers from one tier while its Update resolves through
// another that misses the row, the door engages, the store reports bd's own
// not-found wording, and an operator reads it as the routing bug. The
// distinguishing signal is that the failure is LOUD and store-named: exit 1
// with the store's error verbatim, never a silent success and never a
// fall-through to the subprocess that would re-run the command elsewhere.
func TestDoorUpdateAfterFoundSurfacesStoreErrorVerbatim(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	relic := classResidentWorkShapedBead(t, classStore, "gc-relic1", "an orphaned patrol root")
	skew := fmt.Errorf("resolving issue: no issue found matching %q: %w", relic.ID, beads.ErrNotFound)
	stubClassBindingStore(t, cityPath, getOnlyClassStore{Store: classStore, writeErr: skew})

	for _, args := range [][]string{
		{"update", relic.ID, "--status", "closed"},
		{"close", relic.ID},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := maybeRouteBdByID(cityPath, "", args, &stdout, &stderr)
			if !handled {
				t.Fatalf("%v fell through to the bd subprocess after the class store answered its Get", args)
			}
			if code == 0 {
				t.Fatalf("%v exited 0 on a store whose write reported not-found: %q", args, stdout.String())
			}
			if !strings.Contains(stderr.String(), skew.Error()) {
				t.Errorf("the failure does not carry the store's error verbatim: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), relic.ID) {
				t.Errorf("the failure does not name the bead: %q", stderr.String())
			}
		})
	}
}
