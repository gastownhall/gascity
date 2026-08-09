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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/beadsworkspace"
	sqlitebinding "github.com/gastownhall/gascity/internal/storebinding/sqlite"
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
	code, handled := maybeRouteBdByID(cityPath, []string{"show", bead.ID, "--json"}, &stdout, &stderr)
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
	if code, handled := maybeRouteBdByID(cityPath, []string{"update", subject.ID, "--claim"}, &stdout, &stderr); !handled || code != 0 {
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
	if code, handled := maybeRouteBdByID(cityPath, []string{"release-if-current", subject.ID, "by-id-tester"}, &stdout, &stderr); !handled || code != 0 {
		t.Fatalf("releasing %s = (%d, %t): %s", subject.ID, code, handled, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "released" {
		t.Errorf("the routed release printed %q, want %q", got, "released")
	}

	stdout.Reset()
	stderr.Reset()
	code, handled := maybeRouteBdByID(cityPath, []string{"dep", "list", subject.ID, "--json"}, &stdout, &stderr)
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

// TestBdByIDReservedPrefixAbsenceIsNotAFallThrough pins the rule that makes the
// routing safe: a reserved-prefix id is minted by the class store and nowhere
// else, so its absence there is genuine absence. Falling through would print a
// work-store answer about a bead the work store never held.
func TestBdByIDReservedPrefixAbsenceIsNotAFallThrough(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	missing := reservedClassID(t, "notthere")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, []string{"show", missing}, &stdout, &stderr)
	if !handled {
		t.Fatal("an absent reserved-prefix id fell through to the bd subprocess")
	}
	if code == 0 {
		t.Errorf("an absent bead exited 0 with stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), missing) {
		t.Errorf("the absence does not name %s: %q", missing, stderr.String())
	}
}

// TestBdByIDLeavesWorkStoreIDsToThePassthrough is the other half of the same
// rule. An ordinary work id the class store has never seen is still bd's to
// answer, and the passthrough answers it byte-identically.
func TestBdByIDLeavesWorkStoreIDsToThePassthrough(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)

	var stdout, stderr bytes.Buffer
	if code, handled := maybeRouteBdByID(cityPath, []string{"show", "gc-abc123"}, &stdout, &stderr); handled {
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
		if code, handled := maybeRouteBdByID(cityPath, args, &stdout, &stderr); handled {
			t.Errorf("%v was answered here (exit %d): %s%s", args, code, stdout.String(), stderr.String())
		}
	}
}

// TestBdByIDRefusesAnUnservedVerbOnAClassOwnedBead is the fail-closed floor. An
// operation this surface does not serve, whose subject the class binding owns,
// must not reach bd: bd opens the work workspace, cannot see the bead, and
// either blocks on it or mutates whatever its substring resolver found there.
func TestBdByIDRefusesAnUnservedVerbOnAClassOwnedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "not yours to close", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, []string{"close", bead.ID}, &stdout, &stderr)
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
		t.Error("the refused close reached the class binding anyway")
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
	code, handled := maybeRouteBdByID(cityPath, []string{"show", missing}, &stdout, &stderr)
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
	code, handled := maybeRouteBdByID(cityPath, []string{"show", present}, &stdout, &stderr)
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
	if code, handled := maybeRouteBdByID(cityPath, []string{"show", reservedClassID(t, "anything")}, &stdout, &stderr); handled {
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
		if ident, ok := call.Fun.(*ast.Ident); ok {
			counts[ident.Name]++
		}
		return true
	})
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
	if counts["cliStorageRoutes"] != 1 {
		t.Errorf("%s calls cliStorageRoutes %d time(s), want exactly 1: one store resolution per command", file, counts["cliStorageRoutes"])
	}
	if counts["graphClassBinding"] != 1 {
		t.Errorf("%s calls graphClassBinding %d time(s), want exactly 1", file, counts["graphClassBinding"])
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
	}
	for _, args := range [][]string{
		{"show", "gcg-1"},
		{"update", "gcg-1", "--claim"},
		{"release-if-current", "gcg-1", "someone"},
		{"dep", "list", "gcg-1"},
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
		{"update", "gcg-1"},
		{"dep", "tree", "gcg-1"},
		{"dep", "list"},
		{"dep", "list", "gcg-1", "--direction=sideways"},
		{"close", "gcg-1"},
		{"list"},
	} {
		if op, ok := parseBdByIDOp(args); ok {
			t.Errorf("%v was recognized as %q, want the caller's existing path", args, op.Verb)
		}
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
