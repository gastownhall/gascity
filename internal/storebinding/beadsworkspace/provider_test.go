package beadsworkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// testConfigRef is the workspace every test in this package binds to.
const testConfigRef = "infra"

// cityWithProvider puts the test in a fresh city directory and returns the
// facade one binding of this provider resolves to there, plus its
// specification.
//
// The chdir is the city: nothing carries a city root to a provider, so the
// working directory is what a relative binding location resolves against —
// for this provider exactly as for the built-in one's path. The returned city
// path is the working directory the process reports rather than the temp path
// handed to chdir, so an assertion about what the city holds reads the same
// directory the provider resolves against even when the temp root is a
// symlink.
func cityWithProvider(t *testing.T) (string, storebinding.Provider, storebinding.BindingSpec) {
	t.Helper()
	t.Chdir(t.TempDir())
	city, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	spec := storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: testConfigRef}
	provider, err := ProviderFactory{}.New(spec)
	if err != nil {
		t.Fatalf("constructing the provider for config_ref %q: %v", testConfigRef, err)
	}
	return city, provider, spec
}

// engineOpener is the seam a booting city serves a binding through. A provider
// that does not implement it cannot serve a class at all, so the assertion is
// a fatal one wherever it is made.
func engineOpener(t *testing.T, provider storebinding.Provider) storebinding.EngineOpener {
	t.Helper()
	opener, ok := provider.(storebinding.EngineOpener)
	if !ok {
		t.Fatal("the provider does not open a bead engine; nothing could serve a class from this binding")
	}
	return opener
}

// directoryEntries lists a directory's own names. It is a POSITIVE read: an
// unreadable directory fails the test rather than reading as empty, which is
// how a stat-based check turns a fault into evidence of absence.
func directoryEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestFactoryRegistersTheCompiledProviderID(t *testing.T) {
	if got := (ProviderFactory{}).ID(); got != ProviderID {
		t.Fatalf("factory ID = %q, want %q", got, ProviderID)
	}
	if ProviderID != "beads-workspace" {
		t.Fatalf("provider ID = %q; the compiled identifier is part of every city.toml that names this provider", ProviderID)
	}
}

// TestFactoryRefusesASpecificationThatIsNotThisProvidersScope pins what a
// binding of this provider must say before anything opens: it names this
// provider, and it names a workspace by configuration reference.
func TestFactoryRefusesASpecificationThatIsNotThisProvidersScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec storebinding.BindingSpec
	}{
		{"another provider's binding", storebinding.BindingSpec{Name: "infra", Provider: storebinding.ProviderID(config.StorageProviderSQLiteBeads), ConfigRef: "infra"}},
		{"no workspace named", storebinding.BindingSpec{Name: "infra", Provider: ProviderID}},
		{"a path instead of a reference", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, Path: ".gc/store"}},
		{"a reference that is not a directory name", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: ".."}},
		{"no binding name", storebinding.BindingSpec{Provider: ProviderID, ConfigRef: "infra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := ProviderFactory{}.New(tc.spec)
			if err == nil {
				t.Fatalf("the factory accepted %+v", tc.spec)
			}
			if provider != nil {
				t.Fatalf("the factory returned a provider alongside its error: %v", err)
			}
		})
	}
}

// TestWorkspaceRootIsTheCityRelativeStorageDirectory pins the layout a
// configuration reference resolves to, which is the whole of this provider's
// configuration surface.
func TestWorkspaceRootIsTheCityRelativeStorageDirectory(t *testing.T) {
	city, _, _ := cityWithProvider(t)

	root, err := WorkspaceRoot(testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	// The expectation is built from the working directory the process reports
	// rather than from the temp path, so a temp root reached through a symlink
	// (the default on macOS) compares as the directory the test stands in.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if want := filepath.Join(cwd, ".gc", "storage", "infra"); root != want {
		t.Errorf("workspace root = %s, want %s (city %s)", root, want, city)
	}
}

// TestInspectReportsTheWorkspaceAndCreatesNothing is the mutation-free half of
// the contract, proved as a negative from a directory listing rather than from
// the absence of an error.
func TestInspectReportsTheWorkspaceAndCreatesNothing(t *testing.T) {
	city, provider, spec := cityWithProvider(t)
	before := directoryEntries(t, city)

	inspection, err := storebinding.InspectBinding(context.Background(), provider, spec)
	if err != nil {
		t.Fatalf("inspecting an absent workspace: %v", err)
	}
	if inspection.Complete() {
		t.Error("the inspection reported a complete descriptor; nothing observed the workspace's format, schema or ABI")
	}
	if inspection.Target.Provider != ProviderID {
		t.Errorf("inspection provider = %q, want %q", inspection.Target.Provider, ProviderID)
	}
	if len(inspection.Target.Components) != 1 || inspection.Target.Components[0].ID != ComponentID {
		t.Fatalf("inspection components = %+v, want one %q", inspection.Target.Components, ComponentID)
	}
	for _, class := range coordclass.Classes() {
		if !inspection.Target.Classes.Has(class) {
			t.Errorf("the inspected scope excludes %s; one workspace serves every class its binding is assigned", class)
		}
	}
	if after := directoryEntries(t, city); len(after) != len(before) {
		t.Errorf("inspecting the binding changed the city directory: %v -> %v", before, after)
	}

	// The identity moves when the workspace appears, and only then: it is what
	// a stat can honestly claim and nothing more.
	absent := inspection.Target.Components[0].PhysicalIdentity
	root, err := WorkspaceRoot(testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	if err := os.MkdirAll(workspaceMetadataPath(root), 0o755); err != nil {
		t.Fatalf("creating the workspace fixture: %v", err)
	}
	present, err := storebinding.InspectBinding(context.Background(), provider, spec)
	if err != nil {
		t.Fatalf("inspecting a present workspace: %v", err)
	}
	if present.Target.Components[0].PhysicalIdentity == absent {
		t.Error("the component identity did not change when the workspace appeared")
	}
}

// TestInspectRefusesASpecificationOtherThanTheBoundOne keeps a facade bound to
// one binding: answering for a second would report on a workspace the caller
// did not name.
func TestInspectRefusesASpecificationOtherThanTheBoundOne(t *testing.T) {
	_, provider, spec := cityWithProvider(t)
	other := spec
	other.ConfigRef = "elsewhere"

	if _, err := provider.Inspect(context.Background(), other); !errors.Is(err, ErrInvalidWorkspaceBinding) {
		t.Fatalf("inspecting a different binding = %v, want %v", err, ErrInvalidWorkspaceBinding)
	}
}

// TestLifecycleArmsAreNotOffered pins the three optional lifecycles as absent
// rather than empty: a caller must be able to see that this provider installs
// no guard, activates no generation, and migrates no Work.
func TestLifecycleArmsAreNotOffered(t *testing.T) {
	_, provider, _ := cityWithProvider(t)

	if guards, ok := provider.RetainedGuards(); ok || guards != nil {
		t.Errorf("RetainedGuards = (%v, %t), want (nil, false)", guards, ok)
	}
	if migration, ok := provider.BindingMigration(); ok || migration != nil {
		t.Errorf("BindingMigration = (%v, %t), want (nil, false)", migration, ok)
	}
	if work, ok := provider.WorkMigration(); ok || work != nil {
		t.Errorf("WorkMigration = (%v, %t), want (nil, false)", work, ok)
	}
}

// TestFencedArmsRefuseRatherThanPretend pins the refusals. Each of these arms
// could be made to return a value; every such value would be a claim about
// writers this build does not control.
func TestFencedArmsRefuseRatherThanPretend(t *testing.T) {
	_, provider, _ := cityWithProvider(t)
	ctx := context.Background()

	fence, err := provider.AcquireFence(ctx, storebinding.MigrationGuardClaim{}, storebinding.FenceRequest{})
	if !errors.Is(err, ErrWorkspaceLifecycleUnavailable) {
		t.Errorf("AcquireFence = %v, want %v", err, ErrWorkspaceLifecycleUnavailable)
	}
	if fence != nil {
		t.Error("AcquireFence returned a fence alongside its refusal; a refused acquisition owns no reservation to clean up")
	}
	if _, err := provider.InspectFenced(ctx, storebinding.FencedInspectionRequest{}); !errors.Is(err, ErrWorkspaceLifecycleUnavailable) {
		t.Errorf("InspectFenced = %v, want %v", err, ErrWorkspaceLifecycleUnavailable)
	}
	opened, err := provider.Open(ctx, storebinding.OpenRequest{})
	if !errors.Is(err, ErrWorkspaceLifecycleUnavailable) {
		t.Errorf("Open = %v, want %v", err, ErrWorkspaceLifecycleUnavailable)
	}
	if opened != nil {
		t.Error("Open returned a binding alongside its refusal")
	}
}

// TestOpenEngineRefusesWithoutTouchingTheWorkspace covers every refusal that
// precedes the open, and proves none of them left a directory behind. An open
// that creates a workspace on the way to refusing it is the failure this
// ordering exists to prevent.
func TestOpenEngineRefusesWithoutTouchingTheWorkspace(t *testing.T) {
	city, provider, spec := cityWithProvider(t)
	opener := engineOpener(t, provider)
	all, err := workspaceClasses()
	if err != nil {
		t.Fatalf("building the served class set: %v", err)
	}
	foreign := spec
	foreign.ConfigRef = "elsewhere"

	for _, tc := range []struct {
		name    string
		spec    storebinding.BindingSpec
		classes storebinding.ClassSet
		want    error
	}{
		{"a binding this facade is not bound to", foreign, all, ErrInvalidWorkspaceBinding},
		{"no class to serve", spec, storebinding.ClassSet{}, ErrInvalidWorkspaceBinding},
		{"a workspace that is not there", spec, all, ErrWorkspaceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, closer, err := opener.OpenEngine(tc.spec, tc.classes)
			if !errors.Is(err, tc.want) {
				t.Fatalf("OpenEngine = %v, want %v", err, tc.want)
			}
			if store != nil || closer != nil {
				t.Fatal("a refused open returned a store or a closer")
			}
		})
	}
	if entries := directoryEntries(t, city); len(entries) != 0 {
		t.Errorf("a refused open left %v in the city directory", entries)
	}
}

// TestOpenEngineRequiresTheReservedClassPrefix pins the one property that
// cannot be imposed on a workspace and therefore has to be required: an id
// minted in this binding is never one the work store could have minted.
func TestOpenEngineRequiresTheReservedClassPrefix(t *testing.T) {
	_, provider, _ := cityWithProvider(t)
	workspace, ok := provider.(*workspaceProvider)
	if !ok {
		t.Fatalf("provider is %T, want the workspace facade", provider)
	}
	reserved, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || reserved == "" {
		t.Fatalf("no reserved id prefix is registered for the %q class", config.BeadClassGraph)
	}

	if err := workspace.mintsUnderReservedPrefix(reserved, reserved); err != nil {
		t.Fatalf("a workspace on the reserved prefix was refused: %v", err)
	}
	for _, observed := range []string{"", "gc", "gr"} {
		err := workspace.mintsUnderReservedPrefix(observed, reserved)
		if !errors.Is(err, ErrInvalidWorkspaceBinding) {
			t.Errorf("a workspace minting under %q = %v, want %v", observed, err, ErrInvalidWorkspaceBinding)
		}
	}
}
