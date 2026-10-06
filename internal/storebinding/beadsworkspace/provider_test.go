package beadsworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// testConfigRef is the workspace every test in this package binds to.
const testConfigRef = "infra"

// cityWithProvider returns a fresh city directory and the facade one binding of
// this provider resolves to there.
//
// No test in this package changes the working directory, and that is a
// property under test as much as a convenience: this provider resolves against
// the city root its specification carries, so a test that stood inside the
// city would pass equally well against a provider that resolved against the
// process — which is exactly the defect the city root exists to remove.
func cityWithProvider(t *testing.T) (string, storebinding.Provider, storebinding.BindingSpec) {
	t.Helper()
	city := t.TempDir()
	spec := storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: testConfigRef, CityRoot: city}
	provider, err := ProviderFactory{}.New(spec)
	if err != nil {
		t.Fatalf("constructing the provider for config_ref %q: %v", testConfigRef, err)
	}
	return city, provider, spec
}

// provisionWorkspaceConfig writes the workspace's own configuration file, which
// is what makes a directory a provisioned workspace rather than one the linked
// library would populate with defaults.
func provisionWorkspaceConfig(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(workspaceStatePath(root), 0o755); err != nil {
		t.Fatalf("creating %s: %v", workspaceStatePath(root), err)
	}
	if err := os.WriteFile(workspaceConfigPath(root), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", workspaceConfigPath(root), err)
	}
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

// treeContents fingerprints a directory tree by path AND by content, so a
// comparison across an operation catches a file rewritten in place as well as
// one that was added. An unreadable tree fails the test rather than comparing
// equal to nothing.
func treeContents(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if entry.IsDir() {
			lines = append(lines, "dir  "+rel)
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		lines = append(lines, "file "+rel+" "+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(lines)
	return lines
}

func sameTree(before, after []string) bool {
	if len(before) != len(after) {
		return false
	}
	for index := range before {
		if before[index] != after[index] {
			return false
		}
	}
	return true
}

// recordingEngine is an opened workspace whose prefix is dictated and whose
// close is observable. It exists so the admission decision — above all the
// close that has to follow a refusal — is proved without a live workspace.
type recordingEngine struct {
	beads.Store
	prefix string
	closes int
}

func (e *recordingEngine) IDPrefix() string { return e.prefix }

func (e *recordingEngine) CloseStore() error {
	e.closes++
	return nil
}

func reservedGraphPrefix(t *testing.T) string {
	t.Helper()
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		t.Fatalf("no reserved id prefix is registered for the %q class", config.BeadClassGraph)
	}
	return prefix
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
// provider, it names a workspace by configuration reference, and it says which
// city that workspace belongs to.
func TestFactoryRefusesASpecificationThatIsNotThisProvidersScope(t *testing.T) {
	city := t.TempDir()
	for _, tc := range []struct {
		name string
		spec storebinding.BindingSpec
	}{
		{"another provider's binding", storebinding.BindingSpec{Name: "infra", Provider: storebinding.ProviderID(config.StorageProviderSQLiteBeads), ConfigRef: "infra", CityRoot: city}},
		{"no workspace named", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, CityRoot: city}},
		{"a path instead of a reference", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, Path: ".gc/store", CityRoot: city}},
		{"a reference that is not a directory name", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: "..", CityRoot: city}},
		{"no binding name", storebinding.BindingSpec{Provider: ProviderID, ConfigRef: "infra", CityRoot: city}},
		{"no city to resolve against", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: "infra"}},
		{"a city root that is not absolute", storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: "infra", CityRoot: "relative/city"}},
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

// TestWorkspaceRootIsUnderTheCityTheBindingNames pins the layout, and pins that
// the city comes from the binding rather than from the process.
func TestWorkspaceRootIsUnderTheCityTheBindingNames(t *testing.T) {
	city := t.TempDir()
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	// The expectation is canonicalized the same way the resolution is, so a
	// temp root reached through a symlink (the default on macOS) compares as
	// the city it is.
	canonical, err := filepath.EvalSymlinks(city)
	if err != nil {
		t.Fatalf("resolving %s: %v", city, err)
	}
	if want := filepath.Join(canonical, ".gc", "storage", testConfigRef); root != want {
		t.Errorf("workspace root = %s, want %s", root, want)
	}
}

// TestWorkspaceRootCanonicalizesTheCitySpelling pins the one thing a recorded
// location cannot survive without: two spellings of one city resolve to one
// string. A city reached through a symlink and the same city reached directly
// would otherwise record and recompute different locations, and the difference
// reads as a re-point that holds the boot.
func TestWorkspaceRootCanonicalizesTheCitySpelling(t *testing.T) {
	parent := t.TempDir()
	direct := filepath.Join(parent, "city")
	if err := os.MkdirAll(direct, 0o755); err != nil {
		t.Fatalf("creating the city: %v", err)
	}
	linked := filepath.Join(parent, "city-link")
	if err := os.Symlink(direct, linked); err != nil {
		t.Skipf("this filesystem does not support symlinks: %v", err)
	}

	fromDirect, err := WorkspaceRoot(direct, testConfigRef)
	if err != nil {
		t.Fatalf("resolving through the direct spelling: %v", err)
	}
	fromLink, err := WorkspaceRoot(linked, testConfigRef)
	if err != nil {
		t.Fatalf("resolving through the symlinked spelling: %v", err)
	}
	if fromDirect != fromLink {
		t.Errorf("the same city resolved %s through its path and %s through a symlink to it", fromDirect, fromLink)
	}
}

// TestWorkspaceRootRefusesWithoutACityRatherThanGuessingOne is the whole reason
// the city root is carried: the tempting default is the working directory, and
// under a supervisor that hosts every registered city from one process, that
// directory is nobody's city.
func TestWorkspaceRootRefusesWithoutACityRatherThanGuessingOne(t *testing.T) {
	root, err := WorkspaceRoot("", testConfigRef)
	if !errors.Is(err, ErrInvalidWorkspaceBinding) {
		t.Fatalf("WorkspaceRoot with no city = (%q, %v), want %v", root, err, ErrInvalidWorkspaceBinding)
	}
	if root != "" {
		t.Errorf("a refused resolution returned the path %q", root)
	}
	if !strings.Contains(err.Error(), "CityRoot") {
		t.Errorf("the refusal does not name the field that is missing: %v", err)
	}
	if _, err := WorkspaceRoot("relative/city", testConfigRef); !errors.Is(err, ErrInvalidWorkspaceBinding) {
		t.Errorf("WorkspaceRoot with a relative city = %v, want %v", err, ErrInvalidWorkspaceBinding)
	}
}

// TestTwoCitiesSharingAConfigRefResolveDifferentWorkspaces is the defect this
// design exists to make impossible, asserted as a divergence rather than as a
// property of one city: the same configuration reference in two cities is two
// workspaces, never one shared directory.
func TestTwoCitiesSharingAConfigRefResolveDifferentWorkspaces(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()

	firstProvider, err := ProviderFactory{}.New(storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: testConfigRef, CityRoot: first})
	if err != nil {
		t.Fatalf("constructing the first city's provider: %v", err)
	}
	secondProvider, err := ProviderFactory{}.New(storebinding.BindingSpec{Name: "infra", Provider: ProviderID, ConfigRef: testConfigRef, CityRoot: second})
	if err != nil {
		t.Fatalf("constructing the second city's provider: %v", err)
	}

	firstRoot := boundRootOf(t, firstProvider)
	secondRoot := boundRootOf(t, secondProvider)
	if firstRoot == secondRoot {
		t.Fatalf("two cities sharing config_ref %q resolved the same workspace %s", testConfigRef, firstRoot)
	}
	for _, city := range []struct {
		path string
		root string
	}{{first, firstRoot}, {second, secondRoot}} {
		want, err := WorkspaceRoot(city.path, testConfigRef)
		if err != nil {
			t.Fatalf("resolving the workspace of %s: %v", city.path, err)
		}
		if city.root != want {
			t.Errorf("city %s serves %s, want its own workspace %s", city.path, city.root, want)
		}
	}
}

// boundRootOf reports the workspace a facade is bound to, through the location
// seam a city records rather than through the struct field, so the divergence
// proof reads the same answer the boot gate writes down.
func boundRootOf(t *testing.T, provider storebinding.Provider) string {
	t.Helper()
	locator, ok := provider.(storebinding.BindingLocator)
	if !ok {
		t.Fatal("the provider reports no binding location")
	}
	bound, ok := provider.(*workspaceProvider)
	if !ok {
		t.Fatalf("provider is %T, want the workspace facade", provider)
	}
	location, err := locator.BindingLocation(bound.spec)
	if err != nil {
		t.Fatalf("resolving the binding location: %v", err)
	}
	return location
}

// TestInspectReportsTheWorkspaceAndCreatesNothing is the mutation-free half of
// the contract, proved as a negative from a directory walk rather than from
// the absence of an error.
func TestInspectReportsTheWorkspaceAndCreatesNothing(t *testing.T) {
	city, provider, spec := cityWithProvider(t)
	before := treeContents(t, city)

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
	if after := treeContents(t, city); !sameTree(before, after) {
		t.Errorf("inspecting the binding changed the city directory: %v -> %v", before, after)
	}

	// The identity moves when the workspace appears, and only then: it is what
	// a stat can honestly claim and nothing more.
	absent := inspection.Target.Components[0].PhysicalIdentity
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	provisionWorkspaceConfig(t, root)
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
// did not name — including one that differs only in which city it belongs to.
func TestInspectRefusesASpecificationOtherThanTheBoundOne(t *testing.T) {
	_, provider, spec := cityWithProvider(t)
	elsewhere := spec
	elsewhere.ConfigRef = "elsewhere"
	otherCity := spec
	otherCity.CityRoot = t.TempDir()
	remote := spec
	remote.URL = "https://beads.example/workspaces/infra"
	remote.Auth = storebinding.AuthCredentialProvider

	for _, other := range []storebinding.BindingSpec{elsewhere, otherCity, remote} {
		if _, err := provider.Inspect(context.Background(), other); !errors.Is(err, ErrInvalidWorkspaceBinding) {
			t.Fatalf("inspecting binding %+v = %v, want %v", other, err, ErrInvalidWorkspaceBinding)
		}
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
// precedes the open, and proves none of them left anything behind. An open
// that builds a workspace on the way to refusing it is the failure this
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
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}

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
			if errors.Is(err, ErrWorkspaceUnavailable) && !strings.Contains(err.Error(), root) {
				t.Errorf("the refusal does not name the workspace path it looked at (%s): %v", root, err)
			}
		})
	}
	if entries := treeContents(t, city); len(entries) != 1 {
		t.Errorf("a refused open left %v in the city directory", entries)
	}
}

func TestOpenEngineSelectsCredentialBridgeOnlyForExactHostedBinding(t *testing.T) {
	selectorErr := errors.New("hosted selector reached the running executable")
	tests := []struct {
		name string
		url  string
		auth string
		want bool
	}{
		{name: "credential provider", url: "https://beads.example/workspaces/infra", auth: storebinding.AuthCredentialProvider, want: true},
		{name: "local workspace"},
		{name: "environment credential", url: "https://beads.example/workspaces/infra", auth: "env:BEADS_TOKEN"},
		{name: "remote without auth", url: "https://beads.example/workspaces/infra"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			city := t.TempDir()
			spec := storebinding.BindingSpec{
				Name: "infra", Provider: ProviderID, ConfigRef: testConfigRef, CityRoot: city,
				URL: test.url, Auth: test.auth,
			}
			provider, err := ProviderFactory{}.New(spec)
			if err != nil {
				t.Fatalf("constructing provider: %v", err)
			}
			root, err := WorkspaceRoot(city, testConfigRef)
			if err != nil {
				t.Fatalf("resolving workspace: %v", err)
			}
			provisionWorkspaceConfig(t, root)
			if err := os.WriteFile(workspaceConfigPath(root), []byte(`{"backend":"selector-test"}`), 0o600); err != nil {
				t.Fatalf("writing workspace selector fixture: %v", err)
			}
			classes, err := workspaceClasses()
			if err != nil {
				t.Fatalf("building classes: %v", err)
			}

			originalExecutable := workspaceExecutable
			calls := 0
			workspaceExecutable = func() (string, error) {
				calls++
				return "", selectorErr
			}
			t.Cleanup(func() { workspaceExecutable = originalExecutable })

			store, closer, openErr := engineOpener(t, provider).OpenEngine(spec, classes)
			if store != nil || closer != nil {
				t.Fatal("a refused selector fixture returned a store or closer")
			}
			if openErr == nil {
				t.Fatal("selector fixture unexpectedly opened")
			}
			if test.want {
				if calls != 1 || !errors.Is(openErr, selectorErr) {
					t.Fatalf("hosted selector calls/error = %d/%v, want one executable resolution and %v", calls, openErr, selectorErr)
				}
				return
			}
			if calls != 0 {
				t.Fatalf("non-hosted selector resolved the running executable %d times", calls)
			}
			if errors.Is(openErr, selectorErr) {
				t.Fatalf("non-hosted selector reached the credential bridge: %v", openErr)
			}
		})
	}
}

func TestOpenEngineHostedCredentialCommandIsInvokedAndAmbientEnvIsWithheld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the linked beads command runner uses cmd.exe on Windows; this fixture is a POSIX executable")
	}
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/poison/credential-command")
	t.Setenv("BEADS_DB", "/poison/db")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")

	helperDir := filepath.Join(t.TempDir(), "running gc path with ' quote")
	if err := os.MkdirAll(helperDir, 0o700); err != nil {
		t.Fatalf("creating helper directory: %v", err)
	}
	helperPath := filepath.Join(helperDir, "gc")
	markerPath := filepath.Join(t.TempDir(), "credential-command-invocation")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  printf 'args=%s\\n' \"$*\"\n" +
		"  printf 'db=%s\\n' \"${BEADS_DB-}\"\n" +
		"  printf 'host=%s\\n' \"${BEADS_DOLT_SERVER_HOST-}\"\n" +
		"  printf 'command=%s\\n' \"${BEADS_DOLT_CREDENTIAL_COMMAND-}\"\n" +
		"} > " + shellquote.Quote(markerPath) + "\n" +
		"printf '%s\\n' '{\"token\":\"opaque-test\",\"expirationTimestamp\":\"2099-01-02T03:04:05Z\"}'\n"
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("writing helper executable: %v", err)
	}
	originalExecutable := workspaceExecutable
	workspaceExecutable = func() (string, error) { return helperPath, nil }
	t.Cleanup(func() { workspaceExecutable = originalExecutable })

	city := t.TempDir()
	spec := storebinding.BindingSpec{
		Name: "infra", Provider: ProviderID, ConfigRef: testConfigRef, CityRoot: city,
		URL: "https://beads.example/workspaces/infra", Auth: storebinding.AuthCredentialProvider,
	}
	provider, err := ProviderFactory{}.New(spec)
	if err != nil {
		t.Fatalf("constructing provider: %v", err)
	}
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving workspace: %v", err)
	}
	provisionWorkspaceConfig(t, root)
	metadata := `{"backend":"dolt","database":"dolt","dolt_mode":"server","dolt_server_host":"127.0.0.1","dolt_server_port":1,"dolt_database":"infra","issue_prefix":"gcg"}`
	if err := os.WriteFile(workspaceConfigPath(root), []byte(metadata), 0o600); err != nil {
		t.Fatalf("writing hosted workspace metadata: %v", err)
	}
	classes, err := workspaceClasses()
	if err != nil {
		t.Fatalf("building classes: %v", err)
	}

	store, closer, openErr := engineOpener(t, provider).OpenEngine(spec, classes)
	if openErr == nil {
		if closer != nil {
			_ = closer.Close()
		}
		t.Fatal("hosted workspace unexpectedly connected to the unreachable test endpoint")
	}
	if store != nil || closer != nil {
		t.Fatal("the failed hosted open returned a store or closer")
	}
	if strings.Contains(openErr.Error(), "opaque-test") {
		t.Fatalf("credential appeared in the open error: %v", openErr)
	}

	invocation, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("credential command was not invoked: %v (open error: %v)", err, openErr)
	}
	wantCommand := shellquote.Quote(helperPath) + " internal beads-credential"
	wantInvocation := "args=internal beads-credential\n" +
		"db=\n" +
		"host=\n" +
		"command=" + wantCommand + "\n"
	if got := string(invocation); got != wantInvocation {
		t.Fatalf("credential command invocation = %q, want %q", got, wantInvocation)
	}
	if got := os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND"); got != "/poison/credential-command" {
		t.Errorf("ambient credential command after open = %q, want restored", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
		t.Errorf("ambient server host after open = %q, want restored", got)
	}
}

func TestHostedWorkspaceOpenWiresCredentialRefreshingReopen(t *testing.T) {
	data, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("reading engine.go: %v", err)
	}
	source := string(data)
	for _, want := range []string{
		"beads.WithNativeReopen(reopen)",
		"beads.OpenNativeStorageAtWithoutAmbientEnvWithCredentialCommand(ctx, p.root, command)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("hosted workspace open is missing bounded credential-refresh wiring %q", want)
		}
	}
}

// TestOpenEngineRefusesAHalfProvisionedWorkspaceWithoutBuildingOne is the
// residue proof, and it is why the presence test is the configuration file
// rather than the directory. The linked library treats a directory with no
// configuration as "use the defaults" and builds a complete engine inside it,
// so a weaker check makes a refusing boot create the very thing it rejects.
func TestOpenEngineRefusesAHalfProvisionedWorkspaceWithoutBuildingOne(t *testing.T) {
	city, provider, spec := cityWithProvider(t)
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	if err := os.MkdirAll(workspaceStatePath(root), 0o755); err != nil {
		t.Fatalf("creating the half-provisioned workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceStatePath(root), "leftover.txt"), []byte("not a configuration\n"), 0o644); err != nil {
		t.Fatalf("seeding the half-provisioned workspace: %v", err)
	}
	classes, err := workspaceClasses()
	if err != nil {
		t.Fatalf("building the served class set: %v", err)
	}
	before := treeContents(t, root)

	store, closer, err := engineOpener(t, provider).OpenEngine(spec, classes)
	if !errors.Is(err, ErrWorkspaceUnavailable) {
		if closer != nil {
			_ = closer.Close()
		}
		t.Fatalf("OpenEngine on a workspace with no configuration = %v, want %v", err, ErrWorkspaceUnavailable)
	}
	if store != nil || closer != nil {
		t.Fatal("a refused open returned a store or a closer")
	}
	if !strings.Contains(err.Error(), workspaceConfigPath(root)) {
		t.Errorf("the refusal does not name the configuration file it looked for: %v", err)
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the refusal does not name the workspace it resolved: %v", err)
	}
	if after := treeContents(t, root); !sameTree(before, after) {
		t.Errorf("the refused open changed the workspace:\nbefore %v\nafter  %v", before, after)
	}
}

// TestWorkspaceIsConfiguredAcceptsTheLegacySpelling keeps the presence test
// aligned with what the linked library actually reads: it falls back to the
// older configuration filename, so a workspace carrying only that one is
// provisioned and must not be refused as empty.
func TestWorkspaceIsConfiguredAcceptsTheLegacySpelling(t *testing.T) {
	city, _, _ := cityWithProvider(t)
	root, err := WorkspaceRoot(city, testConfigRef)
	if err != nil {
		t.Fatalf("resolving the workspace root: %v", err)
	}
	if err := os.MkdirAll(workspaceStatePath(root), 0o755); err != nil {
		t.Fatalf("creating the workspace: %v", err)
	}
	if err := os.WriteFile(workspaceLegacyConfigPath(root), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing the legacy configuration: %v", err)
	}
	configured, err := workspaceIsConfigured(root)
	if err != nil {
		t.Fatalf("reading the workspace configuration: %v", err)
	}
	if !configured {
		t.Error("a workspace carrying the legacy configuration spelling reads as unprovisioned")
	}
}

// TestOpenEngineRequiresTheReservedClassPrefixAndClosesWhatItRefuses pins the
// one property that cannot be imposed on a workspace and therefore has to be
// required — an id from this binding is never one the work store could have
// minted — and pins that refusing it releases the handle the open took.
func TestOpenEngineRequiresTheReservedClassPrefixAndClosesWhatItRefuses(t *testing.T) {
	_, provider, _ := cityWithProvider(t)
	workspace, ok := provider.(*workspaceProvider)
	if !ok {
		t.Fatalf("provider is %T, want the workspace facade", provider)
	}
	reserved := reservedGraphPrefix(t)

	admitted := &recordingEngine{Store: beads.NewMemStore(), prefix: reserved}
	store, closer, err := workspace.admit(admitted, reserved)
	if err != nil {
		t.Fatalf("a workspace on the reserved prefix was refused: %v", err)
	}
	if store == nil || closer == nil {
		t.Fatal("an admitted workspace came back without a store or a closer")
	}
	if admitted.closes != 0 {
		t.Errorf("an admitted workspace was closed %d time(s) before its caller asked", admitted.closes)
	}

	for _, observed := range []string{"", "gc", "gr"} {
		refused := &recordingEngine{Store: beads.NewMemStore(), prefix: observed}
		store, closer, err := workspace.admit(refused, reserved)
		if !errors.Is(err, ErrInvalidWorkspaceBinding) {
			t.Errorf("a workspace minting under %q = %v, want %v", observed, err, ErrInvalidWorkspaceBinding)
		}
		if store != nil || closer != nil {
			t.Errorf("a refused workspace (prefix %q) came back with a store or a closer", observed)
		}
		if refused.closes != 1 {
			t.Errorf("a refused workspace (prefix %q) was closed %d time(s), want exactly 1: a refused open must not hold the workspace", observed, refused.closes)
		}
		if !strings.Contains(err.Error(), reserved) {
			t.Errorf("the refusal does not name the required prefix %q: %v", reserved, err)
		}
		if observed != "" && !strings.Contains(err.Error(), observed) {
			t.Errorf("the refusal does not name the prefix the workspace mints under (%q): %v", observed, err)
		}
	}
}
