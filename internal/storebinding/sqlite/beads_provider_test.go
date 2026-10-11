package sqlite

// Walking the provider lifecycle for real.
//
// Everything here drives the beads-over-SQLite provider through the generic
// entry points a composition root would use — storebinding.InspectBinding,
// AcquireWriterFence, InspectFenced, OpenBinding — never through the
// provider's own methods directly. That is the point of the file: those
// generic functions are where the contract lives, and until this provider
// existed nothing but a fake had ever been through them.
//
// The conformance run at the bottom closes the loop. The same class corpus
// that proves the canonical adapters and the deployed graph binding are
// substitutable runs here against front doors that arrived through the
// lifecycle, so "the provider hands back the settled shape" is a test and
// not an assertion in a comment.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storebinding/storebindingtest"
)

// beadsTestGeneration is the durable generation every lifecycle walk below
// runs under. Nothing here migrates, so one generation is enough.
const beadsTestGeneration = storebinding.Generation(1)

func beadsTestSpec(root string) storebinding.BindingSpec {
	return storebinding.BindingSpec{
		Name:     storebinding.BindingName("infra"),
		Provider: BeadsProviderID,
		Path:     root,
	}
}

// seedBeadsSource creates and cleanly closes the deployed Beads database below
// root, leaving a quiescent source a mutation-free census can complete on.
func seedBeadsSource(tb storebindingtest.TB, root string) {
	tb.Helper()
	store, err := beads.OpenSQLiteStore(filepath.Join(root, graphDirectoryName), beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		tb.Fatalf("seeding the SQLite Beads source: %v", err)
	}
	closer, ok := store.(interface{ CloseStore() error })
	if !ok {
		tb.Fatalf("seeded store %T cannot close its physical handle", store)
	}
	if err := closer.CloseStore(); err != nil {
		tb.Fatalf("closing the seeded SQLite Beads source: %v", err)
	}
}

// seedWALResidentBeadsSource creates a source whose committed content is still
// WAL-resident, the way a killed writer leaves one. The writer runs below a
// staging root and its component files are copied out while it is still open,
// so the source under root carries an authoritative WAL that no descriptor in
// this process holds open.
func seedWALResidentBeadsSource(tb storebindingtest.TB, root string) {
	tb.Helper()
	staging := filepath.Join(tb.TempDir(), graphDirectoryName)
	writer, err := beads.OpenSQLiteStore(staging, beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		tb.Fatalf("opening the staging Beads writer: %v", err)
	}
	if _, err := writer.Create(beads.Bead{Title: "WAL-resident beads source"}); err != nil {
		tb.Fatalf("creating a WAL-resident bead: %v", err)
	}
	component := filepath.Join(root, graphDirectoryName)
	if err := os.MkdirAll(component, 0o700); err != nil {
		tb.Fatalf("creating the Beads component directory: %v", err)
	}
	// The WAL index is derived state and is deliberately not copied: SQLite
	// rebuilds it during recovery, and a copied index would reference the
	// staging writer's shared memory.
	for _, name := range []string{graphFilename, graphFilename + "-wal", graphSequenceFloorFilename} {
		content, err := os.ReadFile(filepath.Join(staging, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			tb.Fatalf("reading staged component file %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(component, name), content, 0o600); err != nil {
			tb.Fatalf("writing component file %s: %v", name, err)
		}
	}
	if err := writer.(interface{ CloseStore() error }).CloseStore(); err != nil {
		tb.Fatalf("closing the staging Beads writer: %v", err)
	}
	if info, err := os.Stat(filepath.Join(component, graphFilename+"-wal")); err != nil || info.Size() == 0 {
		tb.Fatalf("the seeded source carries no authoritative WAL (stat error %v)", err)
	}
}

// newBeadsProvider constructs the provider exactly as a composition root would:
// through a frozen registry, resolved by exact provider ID.
func newBeadsProvider(tb storebindingtest.TB, spec storebinding.BindingSpec) storebinding.Provider {
	tb.Helper()
	registry := storebinding.NewProviderRegistry()
	if err := registry.Register(BeadsProviderFactory{}); err != nil {
		tb.Fatalf("registering the Beads provider factory: %v", err)
	}
	if err := registry.Freeze(); err != nil {
		tb.Fatalf("freezing the provider registry: %v", err)
	}
	provider, err := registry.New(spec)
	if err != nil {
		tb.Fatalf("constructing the Beads provider: %v", err)
	}
	return provider
}

// beadsOpenRequest builds the complete active-open request for one inspected
// descriptor.
func beadsOpenRequest(tb storebindingtest.TB, descriptor storebinding.Descriptor) storebinding.OpenRequest {
	tb.Helper()
	authority, err := storebinding.NewDurableActiveOpenAuthority(beadsTestGeneration, descriptor)
	if err != nil {
		tb.Fatalf("minting durable active-open authority: %v", err)
	}
	return storebinding.OpenRequest{
		Descriptor:             descriptor,
		AssignedClasses:        descriptor.Classes(),
		Mode:                   storebinding.OpenModeActive,
		ExpectedGeneration:     beadsTestGeneration,
		ExpectedContract:       descriptor.SemanticContractVersion,
		ExpectedComponents:     storebinding.PinnedComponentRequirements(descriptor),
		ClassRequirements:      beadsClassRequirements(),
		DurableActiveAuthority: authority,
	}
}

// beadsClassRequirements asks every class for transactions and claims.
// Requiring both is deliberate: a provider that over-declared either would be
// refused at Open rather than discovered mid-claim.
func beadsClassRequirements() []storebinding.ClassCapabilityRequirement {
	requirements := make([]storebinding.ClassCapabilityRequirement, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		requirements = append(requirements, storebinding.ClassCapabilityRequirement{
			Class:               class,
			RequireTransactions: true,
			RequireClaims:       true,
		})
	}
	return requirements
}

// openBeadsBinding walks Inspect then Open against a quiescent source and
// returns the opened binding, closed when the assertion ends.
func openBeadsBinding(tb storebindingtest.TB, root string) storebinding.OpenedBinding {
	tb.Helper()
	seedBeadsSource(tb, root)
	spec := beadsTestSpec(root)
	provider := newBeadsProvider(tb, spec)

	inspection, err := storebinding.InspectBinding(context.Background(), provider, spec)
	if err != nil {
		tb.Fatalf("inspecting the Beads binding: %v", err)
	}
	if !inspection.Complete() {
		tb.Fatalf("a quiescent Beads source did not complete mutation-free inspection")
	}
	opened, err := storebinding.OpenBinding(context.Background(), provider, beadsOpenRequest(tb, *inspection.Descriptor))
	if err != nil {
		tb.Fatalf("opening the Beads binding: %v", err)
	}
	tb.Cleanup(func() {
		if err := opened.Close(); err != nil {
			tb.Errorf("closing the Beads binding: %v", err)
		}
	})
	return opened
}

// TestBeadsProviderFactoryIsResourceFree proves New opens nothing. The
// resource-free construction contract is what lets a composition root build
// every compiled provider before it knows which one config selects.
func TestBeadsProviderFactoryIsResourceFree(t *testing.T) {
	root := t.TempDir()
	if _, err := (BeadsProviderFactory{}).New(beadsTestSpec(root)); err != nil {
		t.Fatalf("constructing the Beads provider: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the binding root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("constructing the provider created %v below the binding root", entries)
	}
}

// TestBeadsProviderFactoryRefusesAnotherProvidersBinding proves the factory
// answers only for its own exact provider ID.
func TestBeadsProviderFactoryRefusesAnotherProvidersBinding(t *testing.T) {
	spec := beadsTestSpec(t.TempDir())
	spec.Provider = ProviderID
	if _, err := (BeadsProviderFactory{}).New(spec); !errors.Is(err, ErrInvalidBeadsBinding) {
		t.Fatalf("New(provider=%q) error = %v, want ErrInvalidBeadsBinding", ProviderID, err)
	}
}

// TestBeadsProviderInspectRefusesAnUnboundSpecification proves a provider
// facade answers for exactly the binding it was constructed from. Answering
// for a second would inspect a database the caller never named.
func TestBeadsProviderInspectRefusesAnUnboundSpecification(t *testing.T) {
	root := t.TempDir()
	seedBeadsSource(t, root)
	provider := newBeadsProvider(t, beadsTestSpec(root))

	other := beadsTestSpec(t.TempDir())
	if _, err := storebinding.InspectBinding(context.Background(), provider, other); !errors.Is(err, ErrInvalidBeadsBinding) {
		t.Fatalf("InspectBinding(unbound spec) error = %v, want ErrInvalidBeadsBinding", err)
	}
}

// TestBeadsProviderDescribesOneSixClassComponent pins the shape of the
// descriptor: one physical component carrying every class, not six components
// and not a one-class Graph binding wearing a new provider ID.
func TestBeadsProviderDescribesOneSixClassComponent(t *testing.T) {
	root := t.TempDir()
	seedBeadsSource(t, root)
	provider := newBeadsProvider(t, beadsTestSpec(root))

	inspection, err := storebinding.InspectBinding(context.Background(), provider, beadsTestSpec(root))
	if err != nil {
		t.Fatalf("inspecting the Beads binding: %v", err)
	}
	if !inspection.Complete() {
		t.Fatal("a quiescent Beads source did not complete mutation-free inspection")
	}
	descriptor := *inspection.Descriptor
	if descriptor.Provider != BeadsProviderID {
		t.Fatalf("descriptor provider = %q, want %q", descriptor.Provider, BeadsProviderID)
	}
	if descriptor.SemanticContractVersion == graphContract {
		t.Fatal("the six-class Beads scope reports the one-class Graph contract")
	}
	if len(descriptor.Components) != 1 {
		t.Fatalf("descriptor has %d components, want exactly one Beads ledger", len(descriptor.Components))
	}
	for _, class := range coordclass.Classes() {
		if !descriptor.Classes().Has(class) {
			t.Errorf("descriptor does not serve class %s", class)
		}
		if !descriptor.Capabilities.For(class).Available {
			t.Errorf("descriptor declares class %s unavailable", class)
		}
	}
	// The locator and identity must remain the deployed component's, so an
	// overlap check still sees this binding and a Graph binding as one file.
	component := descriptor.Components[0]
	if component.ID != GraphComponentID {
		t.Fatalf("component ID = %q, want the deployed component %q", component.ID, GraphComponentID)
	}
	path, err := GraphPath(root)
	if err != nil {
		t.Fatalf("resolving the deployed database path: %v", err)
	}
	locator, err := graphLocator(path)
	if err != nil {
		t.Fatalf("resolving the deployed locator: %v", err)
	}
	if component.Locator != locator {
		t.Fatalf("component locator = %q, want the deployed component locator %q", component.Locator, locator)
	}
}

// TestBeadsTargetWideningIsExactlyInvertible proves the narrow/widen pair is a
// pure re-labeling: every physical fact survives a round trip, so nothing
// observed can be lost or invented between the provider and the component
// machinery it delegates to.
func TestBeadsTargetWideningIsExactlyInvertible(t *testing.T) {
	root := t.TempDir()
	seedBeadsSource(t, root)
	path, err := GraphPath(root)
	if err != nil {
		t.Fatalf("resolving the deployed database path: %v", err)
	}
	state, err := captureGraphSource(path)
	if err != nil {
		t.Fatalf("capturing the deployed source: %v", err)
	}
	component, err := newGraphTarget(path, state)
	if err != nil {
		t.Fatalf("building the component target: %v", err)
	}

	scope, err := widenBeadsTarget(component)
	if err != nil {
		t.Fatalf("widening the component target: %v", err)
	}
	if scope.Provider != BeadsProviderID {
		t.Fatalf("widened provider = %q, want %q", scope.Provider, BeadsProviderID)
	}
	narrowed, err := narrowBeadsTarget(scope)
	if err != nil {
		t.Fatalf("narrowing the scope target: %v", err)
	}
	if !narrowed.Equal(component) {
		t.Fatalf("narrow(widen(target)) = %+v, want the original component target %+v", narrowed, component)
	}
	if _, err := narrowBeadsTarget(component); !errors.Is(err, ErrInvalidBeadsBinding) {
		t.Fatalf("narrowBeadsTarget(component target) error = %v, want ErrInvalidBeadsBinding", err)
	}
	if _, err := widenBeadsTarget(scope); !errors.Is(err, ErrInvalidBeadsBinding) {
		t.Fatalf("widenBeadsTarget(scope target) error = %v, want ErrInvalidBeadsBinding", err)
	}
}

// TestBeadsProviderOpensEverySixClassFrontDoor walks Inspect then Open through
// the generic lifecycle and proves the opened binding hands back all six
// closed class contracts.
func TestBeadsProviderOpensEverySixClassFrontDoor(t *testing.T) {
	opened := openBeadsBinding(storebindingtest.Wrap(t), t.TempDir())

	if _, ok := opened.Work(); !ok {
		t.Error("the opened binding has no Work topology")
	}
	if _, ok := opened.Graph(); !ok {
		t.Error("the opened binding has no Graph front door")
	}
	if _, ok := opened.Sessions(); !ok {
		t.Error("the opened binding has no Sessions front door")
	}
	if _, ok := opened.Messaging(); !ok {
		t.Error("the opened binding has no Messaging front door")
	}
	if _, ok := opened.Orders(); !ok {
		t.Error("the opened binding has no Orders front door")
	}
	nudges, ok := opened.Nudges()
	if !ok {
		t.Fatal("the opened binding has no Nudge front doors")
	}
	if nudges.Queue == nil || nudges.Shadows == nil {
		t.Fatal("the opened Nudge front doors are incomplete")
	}
}

// TestBeadsProviderOpenRefusesAMovedComponent proves the pre-open identity
// check is load-bearing. Nothing holds a fence during an active open, so a
// descriptor that no longer describes what is on disk must be refused rather
// than opened optimistically.
func TestBeadsProviderOpenRefusesAMovedComponent(t *testing.T) {
	root := t.TempDir()
	seedBeadsSource(t, root)
	spec := beadsTestSpec(root)
	provider := newBeadsProvider(t, spec)

	inspection, err := storebinding.InspectBinding(context.Background(), provider, spec)
	if err != nil {
		t.Fatalf("inspecting the Beads binding: %v", err)
	}
	if !inspection.Complete() {
		t.Fatal("a quiescent Beads source did not complete mutation-free inspection")
	}
	request := beadsOpenRequest(storebindingtest.Wrap(t), *inspection.Descriptor)

	// Rename a different database over the component. The descriptor stays
	// internally valid and the path stays spelled the same; the file behind it
	// is simply no longer the one the census observed.
	replacement := t.TempDir()
	seedBeadsSource(t, replacement)
	if err := os.Rename(
		filepath.Join(replacement, graphDirectoryName, graphFilename),
		filepath.Join(root, graphDirectoryName, graphFilename),
	); err != nil {
		t.Fatalf("replacing the Beads component: %v", err)
	}

	opened, err := storebinding.OpenBinding(context.Background(), provider, request)
	if err == nil {
		if closeErr := opened.Close(); closeErr != nil {
			t.Errorf("closing the unexpectedly opened binding: %v", closeErr)
		}
		t.Fatal("OpenBinding opened a component whose physical identity changed after inspection")
	}
	if !errors.Is(err, storebinding.ErrFenceTargetMoved) {
		t.Fatalf("OpenBinding(moved component) error = %v, want ErrFenceTargetMoved", err)
	}
}

// TestBeadsProviderOpenRefusesModesItHasNoLifecycleFor proves the provider
// fails closed on the migration modes. It declares neither a binding- nor a
// Work-migration lifecycle, so admitting a migration open would enroll a
// participant that can never activate or hand back a receipt.
func TestBeadsProviderOpenRefusesModesItHasNoLifecycleFor(t *testing.T) {
	for _, mode := range []storebinding.OpenMode{
		storebinding.OpenModeAdmittedMigrationDestination,
		storebinding.OpenModeRetainedSource,
	} {
		if _, err := beadsOpenIsReadOnly(mode); !errors.Is(err, storebinding.ErrInvalidOpenMode) {
			t.Errorf("beadsOpenIsReadOnly(%v) error = %v, want ErrInvalidOpenMode", mode, err)
		}
	}
	readOnly, err := beadsOpenIsReadOnly(storebinding.OpenModeReadOnlySource)
	if err != nil || !readOnly {
		t.Fatalf("beadsOpenIsReadOnly(read-only source) = (%t, %v), want (true, nil)", readOnly, err)
	}
	readOnly, err = beadsOpenIsReadOnly(storebinding.OpenModeActive)
	if err != nil || readOnly {
		t.Fatalf("beadsOpenIsReadOnly(active) = (%t, %v), want (false, nil)", readOnly, err)
	}
}

// TestBeadsProviderDeclaresNoMigrationLifecycle pins the honest answer to the
// three optional lifecycle questions. Reporting a lifecycle this provider does
// not implement is the same defect class as over-declaring a capability.
func TestBeadsProviderDeclaresNoMigrationLifecycle(t *testing.T) {
	root := t.TempDir()
	provider := newBeadsProvider(t, beadsTestSpec(root))
	if _, ok := provider.RetainedGuards(); ok {
		t.Error("the provider reports a retained-guard lifecycle it does not implement")
	}
	if _, ok := provider.BindingMigration(); ok {
		t.Error("the provider reports a binding-migration lifecycle it does not implement")
	}
	if _, ok := provider.WorkMigration(); ok {
		t.Error("the provider reports a Work-migration lifecycle it does not implement")
	}
}

// TestBeadsCapabilityVerificationFailsClosed is the declare-versus-discover
// proof. The deployed engine genuinely has atomic transactions and a
// compare-and-swap claim; an engine that has neither must be refused for every
// class that declares them, so the loss surfaces at Open and never mid-claim.
func TestBeadsCapabilityVerificationFailsClosed(t *testing.T) {
	root := t.TempDir()
	seedBeadsSource(t, root)
	engine, err := beads.OpenSQLiteStore(filepath.Join(root, graphDirectoryName), beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		t.Fatalf("opening the deployed engine: %v", err)
	}
	t.Cleanup(func() {
		if err := engine.(interface{ CloseStore() error }).CloseStore(); err != nil {
			t.Errorf("closing the deployed engine: %v", err)
		}
	})

	if err := verifyBeadsCapabilities(engine, beadsCapabilities(sqliteWriterFencingSupported())); err != nil {
		t.Fatalf("the deployed engine failed its own capability declaration: %v", err)
	}

	// beads.MemStore has neither an atomic transaction nor a two-argument
	// claim, so every class that declares them must be refused.
	memory := beads.NewMemStore()
	err = verifyBeadsCapabilities(memory, beadsCapabilities(false))
	if !errors.Is(err, ErrBeadsCapabilityUndeclared) {
		t.Fatalf("verifyBeadsCapabilities(memory, full declaration) error = %v, want ErrBeadsCapabilityUndeclared", err)
	}

	// An honest declaration over the same store is accepted, which is what
	// makes the refusal above a capability check and not a store check.
	honest := storebinding.ClassCapabilities{}
	for _, class := range coordclass.Classes() {
		switch class {
		case coordclass.ClassWork:
			honest.Work = storebinding.ClassCapability{Available: true}
		case coordclass.ClassGraph:
			honest.Graph = storebinding.ClassCapability{Available: true}
		case coordclass.ClassSessions:
			honest.Sessions = storebinding.ClassCapability{Available: true}
		case coordclass.ClassMessaging:
			honest.Messaging = storebinding.ClassCapability{Available: true}
		case coordclass.ClassOrders:
			honest.Orders = storebinding.ClassCapability{Available: true}
		case coordclass.ClassNudges:
			honest.Nudges = storebinding.ClassCapability{Available: true}
		}
	}
	if err := verifyBeadsCapabilities(memory, honest); err != nil {
		t.Fatalf("verifyBeadsCapabilities(memory, honest declaration) = %v, want nil", err)
	}
}

// TestBeadsProviderCompletesInspectionOnlyUnderAFence proves the fenced leg of
// the lifecycle end to end. A source whose committed content is still
// WAL-resident cannot be censused mutation-free — reading it would mean
// recovering it — so Inspect must answer incomplete and the descriptor must
// arrive from a private snapshot taken under a held writer fence, reaching the
// deployed snapshot machinery through this provider's own fence projection.
func TestBeadsProviderCompletesInspectionOnlyUnderAFence(t *testing.T) {
	if !sqliteWriterFencingSupported() {
		t.Skip("SQLite writer fencing requires Linux OFD locks")
	}
	root := t.TempDir()
	seedWALResidentBeadsSource(storebindingtest.Wrap(t), root)

	spec := beadsTestSpec(root)
	provider := newBeadsProvider(t, spec)
	inspection, err := storebinding.InspectBinding(context.Background(), provider, spec)
	if err != nil {
		t.Fatalf("inspecting the Beads binding: %v", err)
	}
	if inspection.Complete() {
		t.Fatal("a source with rollback-journal residue completed inspection without a fence")
	}

	cityDir := filepath.Join(root, ".gc")
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("creating the city guard directory: %v", err)
	}
	scope, err := storebinding.NewMigrationGuardScope(cityDir)
	if err != nil {
		t.Fatalf("resolving the city guard scope: %v", err)
	}
	guard, err := storebinding.AcquireMigrationGuard(context.Background(), cityDir, beadsTestGeneration)
	if err != nil {
		t.Fatalf("acquiring the city migration guard: %v", err)
	}
	t.Cleanup(func() {
		if err := guard.Release(); err != nil {
			t.Errorf("releasing the city migration guard: %v", err)
		}
	})

	fence, err := storebinding.AcquireWriterFence(context.Background(), guard, provider, storebinding.FenceRequest{
		Target:             inspection.Target,
		GuardScope:         scope,
		ExpectedGeneration: beadsTestGeneration,
		Components:         []storebinding.ComponentID{GraphComponentID},
		Role:               storebinding.FenceRoleSource,
	})
	if err != nil {
		t.Fatalf("acquiring the Beads writer fence: %v", err)
	}
	defer func() {
		if err := fence.Release(context.Background()); err != nil {
			t.Errorf("releasing the Beads writer fence: %v", err)
		}
	}()
	if !fence.Target().Equal(inspection.Target) {
		t.Fatal("the acquired fence does not cover the inspected scope")
	}

	descriptor, err := storebinding.InspectFenced(context.Background(), provider, storebinding.FencedInspectionRequest{
		Target:             inspection.Target,
		Fence:              fence,
		ExpectedGeneration: beadsTestGeneration,
	})
	if err != nil {
		t.Fatalf("completing the fenced Beads inspection: %v", err)
	}
	if descriptor.Provider != BeadsProviderID || !descriptor.Classes().Equal(inspection.Target.Classes) {
		t.Fatalf("the fenced descriptor is not this provider's six-class scope: %+v", descriptor)
	}
	if _, err := descriptor.Identity(); err != nil {
		t.Fatalf("the fenced descriptor has no identity: %v", err)
	}

	// The fence excludes writers; it does not make the WAL-resident content
	// readable. A source open still has to answer from the file itself without
	// recovering it, so the shared quiescence gate refuses and the caller is
	// left with the snapshot route it already has.
	_, err = storebinding.OpenBinding(context.Background(), provider, storebinding.OpenRequest{
		Descriptor:         descriptor,
		AssignedClasses:    descriptor.Classes(),
		Mode:               storebinding.OpenModeReadOnlySource,
		ExpectedGeneration: beadsTestGeneration,
		ExpectedContract:   descriptor.SemanticContractVersion,
		ExpectedComponents: storebinding.PinnedComponentRequirements(descriptor),
		ClassRequirements:  beadsClassRequirements(),
		AdmissionFence:     fence,
	})
	if !errors.Is(err, ErrBeadsLiveWAL) {
		t.Fatalf("OpenBinding(read-only source over a WAL-resident database) error = %v, want ErrBeadsLiveWAL", err)
	}
}

// TestBeadsProviderOpenRefusesACallerShapedCapabilityDeclaration proves the
// capability declaration belongs to the provider. A descriptor that
// under-declares would open cleanly and then silently refuse callers that ask
// for a capability the engine genuinely has.
func TestBeadsProviderOpenRefusesACallerShapedCapabilityDeclaration(t *testing.T) {
	root := t.TempDir()
	seedBeadsSource(t, root)
	spec := beadsTestSpec(root)
	provider := newBeadsProvider(t, spec)

	inspection, err := storebinding.InspectBinding(context.Background(), provider, spec)
	if err != nil {
		t.Fatalf("inspecting the Beads binding: %v", err)
	}
	if !inspection.Complete() {
		t.Fatal("a quiescent Beads source did not complete mutation-free inspection")
	}

	tampered := inspection.Descriptor.Clone()
	tampered.Capabilities.Nudges.Claims = false
	authority, err := storebinding.NewDurableActiveOpenAuthority(beadsTestGeneration, tampered)
	if err != nil {
		t.Fatalf("minting authority for the tampered descriptor: %v", err)
	}
	requirements := make([]storebinding.ClassCapabilityRequirement, 0, len(coordclass.Classes()))
	for _, class := range coordclass.Classes() {
		requirements = append(requirements, storebinding.ClassCapabilityRequirement{Class: class})
	}

	opened, err := storebinding.OpenBinding(context.Background(), provider, storebinding.OpenRequest{
		Descriptor:             tampered,
		AssignedClasses:        tampered.Classes(),
		Mode:                   storebinding.OpenModeActive,
		ExpectedGeneration:     beadsTestGeneration,
		ExpectedContract:       tampered.SemanticContractVersion,
		ExpectedComponents:     storebinding.PinnedComponentRequirements(tampered),
		ClassRequirements:      requirements,
		DurableActiveAuthority: authority,
	})
	if err == nil {
		if closeErr := opened.Close(); closeErr != nil {
			t.Errorf("closing the unexpectedly opened binding: %v", closeErr)
		}
		t.Fatal("OpenBinding accepted a caller-shaped capability declaration")
	}
	if !errors.Is(err, ErrInvalidBeadsBinding) {
		t.Fatalf("OpenBinding(tampered capabilities) error = %v, want ErrInvalidBeadsBinding", err)
	}
}

// TestBeadsProviderClassConformance is the substitution proof for the
// provider-opened shape: the unchanged class corpus, run against front doors that
// arrived through Inspect and Open rather than through a directly constructed
// adapter set.
func TestBeadsProviderClassConformance(t *testing.T) {
	capability := storebinding.ClassCapability{Available: true, Transactions: true, Claims: true}

	t.Run("graph", func(t *testing.T) {
		storebindingtest.RunGraphStoreTests(storebindingtest.Wrap(t), storebindingtest.GraphSuite{
			NewStore: func(tb storebindingtest.TB) storebinding.GraphStore {
				graph, _ := openBeadsBinding(tb, tb.TempDir()).Graph()
				return graph
			},
			Capability: capability,
		})
	})
	t.Run("sessions", func(t *testing.T) {
		storebindingtest.RunSessionsStoreTests(storebindingtest.Wrap(t), storebindingtest.SessionsSuite{
			NewStore: func(tb storebindingtest.TB) storebinding.SessionsStore {
				sessions, _ := openBeadsBinding(tb, tb.TempDir()).Sessions()
				return sessions
			},
			Capability: capability,
		})
	})
	t.Run("orders", func(t *testing.T) {
		storebindingtest.RunOrdersStoreTests(storebindingtest.Wrap(t), storebindingtest.OrdersSuite{
			NewStore: func(tb storebindingtest.TB) storebinding.OrdersStore {
				orders, _ := openBeadsBinding(tb, tb.TempDir()).Orders()
				return orders
			},
			Capability: capability,
		})
	})
	t.Run("nudges", func(t *testing.T) {
		storebindingtest.RunNudgeFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.NudgesSuite{
			NewFrontDoors: func(tb storebindingtest.TB) storebinding.NudgeFrontDoors {
				nudges, _ := openBeadsBinding(tb, tb.TempDir()).Nudges()
				return nudges
			},
			Capability: capability,
		})
	})
	t.Run("messaging", func(t *testing.T) {
		storebindingtest.RunMessagingFrontDoorTests(storebindingtest.Wrap(t), storebindingtest.MessagingSuite{
			NewFrontDoors: func(tb storebindingtest.TB) storebinding.MessagingFrontDoors {
				return bindBeadsMessagingForConformance(tb, openBeadsBinding(tb, tb.TempDir()))
			},
			Capability: capability,
		})
	})
	t.Run("work", func(t *testing.T) {
		storebindingtest.RunWorkTopologyTests(storebindingtest.Wrap(t), storebindingtest.WorkTopologySuite{
			NewTopology: func(tb storebindingtest.TB) storebinding.WorkTopology {
				work, _ := openBeadsBinding(tb, tb.TempDir()).Work()
				return work
			},
			WantPhysicalWorkspaces: 1,
		})
	})
}

// bindBeadsMessagingForConformance completes the Messaging front doors the way
// a composition root does: the opened binding hands back an unbound persistence
// edge, and the caller supplies the Sessions address directory it resolves
// against.
func bindBeadsMessagingForConformance(tb storebindingtest.TB, opened storebinding.OpenedBinding) storebinding.MessagingFrontDoors {
	tb.Helper()
	binder, ok := opened.Messaging()
	if !ok {
		tb.Fatalf("the opened binding has no Messaging persistence")
	}
	sessions, ok := opened.Sessions()
	if !ok {
		tb.Fatalf("the opened binding has no Sessions front door")
	}
	directory, ok := sessions.(storebinding.SessionsAddressDirectory)
	if !ok {
		tb.Fatalf("the opened Sessions front door %T is not an address directory", sessions)
	}
	fronts, err := binder.BindSessions(directory)
	if err != nil {
		tb.Fatalf("binding Messaging to the Sessions directory: %v", err)
	}
	return fronts
}
