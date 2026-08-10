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

// TestBdByIDReservedPrefixAbsenceIsNotAFallThrough pins the rule that makes the
// routing safe: a reserved-prefix id is minted by the class store and nowhere
// else, so its absence there is genuine absence. Falling through would print a
// work-store answer about a bead the work store never held.
func TestBdByIDReservedPrefixAbsenceIsNotAFallThrough(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	missing := reservedClassID(t, "notthere")

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"show", missing}, &stdout, &stderr)
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
	created, err := classStore.Create(migrated)
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
func TestBdByIDRefusesAnUnservedVerbOnAClassOwnedBead(t *testing.T) {
	cityPath, classStore := foreignProviderCity(t)
	bead := mustCreateClassBead(t, classStore, beads.Bead{Title: "not yours to close", Type: "task"})

	var stdout, stderr bytes.Buffer
	code, handled := maybeRouteBdByID(cityPath, "", []string{"close", bead.ID}, &stdout, &stderr)
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
	if counts["cliStorageRoutes"] != 1 {
		t.Errorf("%s calls cliStorageRoutes %d time(s), want exactly 1: one store resolution per command", file, counts["cliStorageRoutes"])
	}
	if counts["graphClassBinding"] != 1 {
		t.Errorf("%s calls graphClassBinding %d time(s), want exactly 1", file, counts["graphClassBinding"])
	}
}

// TestBdByIDEntersTheFunnelOnlyForInvocationsThatCouldConcernAClassBead pins the
// cost gate, as a NEGATIVE about work that must not happen.
//
// Resolving the funnel loads the city config, resolves a storage plan, opens a
// binding, and on a born-split city re-proves the invariant with a full,
// unpaginated work-store census.
//
// The line this draws is servability, and it is drawn where correctness allows
// it rather than where it would be cheapest:
//
//   - An UNSERVED verb enters only when the argv addresses a reserved-prefix
//     id. `gc bd close gc-123` and `gc bd delete gc-123` are pure work
//     invocations — this surface would refuse them or nothing — so they pay
//     nothing and keep the exact-ID collision guard doBd already applies.
//   - A SERVED verb always enters, including on a work-shaped id, because the
//     residence probe is the only thing that finds a migrated row: `gc storage
//     migrate` preserves ids, so a work-shaped id can be class-resident, and a
//     WRITE that skipped the probe would close the retained copy and leave the
//     binding's row open — the molecule stall this surface exists to remove.
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
		"unserved work close":       {[]string{"close", "gc-123"}, false},
		"unserved work delete":      {[]string{"delete", "gc-123"}, false},
		"unserved work reopen":      {[]string{"reopen", "gc-123"}, false},
		"unserved update spelling":  {[]string{"update", "gc-123", "--notes", "x"}, false},
		"work list":                 {[]string{"list", "--status", "open"}, false},
		"work dep tree":             {[]string{"dep", "tree", "gc-123"}, false},
		"quoted id in a value":      {[]string{"list", "--metadata-field", "workflow_id=" + reserved}, false},
		"class mutation":            {[]string{"update", reserved, "--status", "closed"}, true},
		"class close":               {[]string{"close", reserved}, true},
		"class id in an id flag":    {[]string{"list", "--parent", reserved}, true},
		"served read on a work id":  {[]string{"show", "gc-123"}, true},
		"served write on a work id": {[]string{"update", "gc-123", "--status", "closed"}, true},
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
		bdByIDUpdate:  true,
	}
	for _, args := range [][]string{
		{"show", "gcg-1"},
		{"update", "gcg-1", "--claim"},
		{"release-if-current", "gcg-1", "someone"},
		{"dep", "list", "gcg-1"},
		{"dep", "list", "gcg-1", "-t", "blocks"},
		{"dep", "list", "gcg-1", "--direction=up"},
		{"update", "gcg-1", "--status", "closed"},
		{"update", "gcg-1", "--set-metadata", "gc.outcome=pass", "--status=closed"},
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
		{"dep", "tree", bead.ID},
		{"close", bead.ID},
		{"delete", bead.ID},
		{"update", bead.ID, "--notes", "a note"},
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
