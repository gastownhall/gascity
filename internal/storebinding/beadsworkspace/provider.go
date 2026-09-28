// Package beadsworkspace serves a city's storage classes from a beads
// workspace directory opened through the linked beads library.
//
// The binding names a workspace. The library opens it, and the workspace's own
// configuration names the backend that serves it. That split is the whole
// point: gc chooses the workspace, never the engine behind it. A workspace
// whose backend the linked library learns to serve later needs no change here,
// because nothing in this package asks which backend it got.
//
// It is deliberately the smaller half of what the other built-in provider
// does. That one owns the database it serves — it censuses it, fences it, and
// migrates a city onto it. This one owns none of those things: a workspace's
// writers may not even be on this machine. So every lifecycle arm that would
// have to claim otherwise refuses in the open, and the one arm a booting city
// actually walks is implemented for real. See engine.go.
//
// # Provisioning contract
//
// A city serves from a workspace it did not create, so the workspace has to
// arrive already fit to serve. Three things are required of whoever provisions
// it, and a city refuses rather than repairing any of them:
//
//   - The workspace exists at <city>/.gc/storage/<config_ref>/ and carries the
//     linked library's own configuration file, .beads/metadata.json. That file
//     is what names the backend; without it the library serves the directory
//     with defaults, which for an empty directory means creating an engine.
//   - Its configured issue prefix is the reserved graph-class prefix. gc cannot
//     impose a prefix on a workspace, so a binding whose workspace mints under
//     any other prefix is refused: its ids would be readable as the work
//     store's. See mintsUnderReservedPrefix.
//   - Its custom bead types include the coordination types a city writes —
//     session, convoy, wisp, wait — which the library's own closed type set
//     does not carry. This one is not checked here: the library refuses such a
//     write immediately and by name, and a city has no business editing a
//     workspace's type policy.
//
// The workspace's own configuration is the source of how a local binding is
// served: every ambient BEADS_-prefixed variable is withheld for the duration
// of the open, so nothing a city process inherited can re-point it. A binding
// that explicitly names the hosted credential provider is the one exception:
// the opener projects a command back into this running gc binary while still
// withholding every other ambient BEADS_ variable.
//
// One thing this provider does not exercise: a controller and a one-shot
// command opening the same workspace at the same moment. Whether that is
// contention, a wait, or a refusal is the workspace backend's answer, and
// nothing here has been proved against it.
package beadsworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

const (
	// ProviderID is the built-in provider identifier for one beads workspace
	// projected into every storage class its binding serves.
	ProviderID = storebinding.ProviderID("beads-workspace")

	// ComponentID names the single physical component of such a binding: the
	// workspace directory itself.
	ComponentID = storebinding.ComponentID("workspace")

	// workspaceStateDir is the directory the linked beads library keeps a
	// workspace's own state and configuration in.
	workspaceStateDir = ".beads"

	// workspaceConfigFile is the workspace's own configuration file inside that
	// directory — the one that names the backend serving the workspace. Its
	// presence is what makes a directory a provisioned workspace rather than a
	// path the library would populate with defaults.
	workspaceConfigFile = "metadata.json"

	// workspaceLegacyConfigFile is the older spelling the linked library still
	// reads before falling back to defaults.
	workspaceLegacyConfigFile = "config.json"
)

var (
	// ErrInvalidWorkspaceBinding reports a specification or configuration
	// reference that is not this provider's exact single-workspace scope.
	ErrInvalidWorkspaceBinding = errors.New("invalid beads workspace binding")

	// ErrWorkspaceUnavailable reports a configured workspace that is not there.
	ErrWorkspaceUnavailable = errors.New("beads workspace is not present")

	// ErrWorkspaceLifecycleUnavailable reports a lifecycle operation this
	// provider cannot perform. The workspace's backend is the linked beads
	// library's to choose and the workspace's own to declare, so this build
	// owns neither its writers nor a mutation-free census of its physical
	// state — and a lifecycle arm that answered anyway would be reporting a
	// guarantee nothing here can hold.
	ErrWorkspaceLifecycleUnavailable = errors.New("beads workspace binding has no such lifecycle")

	_ storebinding.ProviderFactory = ProviderFactory{}
	_ storebinding.Provider        = (*workspaceProvider)(nil)
	_ storebinding.BindingLocator  = (*workspaceProvider)(nil)
)

// ProviderFactory constructs the resource-free provider facade for one beads
// workspace binding.
type ProviderFactory struct{}

// ID returns the exact provider identifier this factory registers.
func (ProviderFactory) ID() storebinding.ProviderID { return ProviderID }

// New binds one validated specification. It opens nothing and creates nothing:
// resolving where the workspace lives is a string operation, and the workspace
// itself is only ever touched by the call that needs it.
func (ProviderFactory) New(spec storebinding.BindingSpec) (storebinding.Provider, error) {
	root, err := boundWorkspaceRoot(spec)
	if err != nil {
		return nil, err
	}
	return &workspaceProvider{spec: spec, root: root}, nil
}

// workspaceProvider is one immutable provider facade bound to one binding.
type workspaceProvider struct {
	spec storebinding.BindingSpec
	// root is the resolved workspace directory: the parent of the workspace's
	// own metadata directory, and what the linked beads library is handed.
	root string
}

// Inspect reports where the workspace is and whether it is there, and nothing
// else.
//
// The descriptor is deliberately absent. A complete one would declare the
// binding's physical format, schema and ABI, and the only honest source for
// those is the workspace's own backend — which this provider does not open
// during a mutation-free inspection and could not census from the outside if
// it did. Reporting an incomplete inspection is the contract's way of saying a
// fenced census is required; this provider has no fence, so an inspection of
// this binding never completes, and that is the truthful answer rather than a
// declaration nothing observed.
func (p *workspaceProvider) Inspect(_ context.Context, spec storebinding.BindingSpec) (storebinding.Inspection, error) {
	if err := p.boundTo(spec); err != nil {
		return storebinding.Inspection{}, err
	}
	target, err := p.fenceTarget()
	if err != nil {
		return storebinding.Inspection{}, err
	}
	return storebinding.NewInspection(target, nil)
}

// AcquireFence refuses: this provider cannot exclude a workspace's writers.
//
// A writer fence is a promise that nothing else can write the component while
// it is held. The workspace's writers are whatever its own backend admits —
// other processes on this machine, or clients of a server this build never
// contacts — so no lock this package could take would make that promise true.
// Returning a fence that excludes nothing is worse than refusing: every caller
// of the fenced protocol would then proceed believing it holds one.
func (p *workspaceProvider) AcquireFence(context.Context, storebinding.MigrationGuardClaim, storebinding.FenceRequest) (storebinding.WriterFence, error) {
	return nil, fmt.Errorf("%w: binding %q is a beads workspace whose backend this build does not own, so nothing here can exclude its writers",
		ErrWorkspaceLifecycleUnavailable, p.spec.Name)
}

// InspectFenced refuses: it exists to complete a census under a held fence,
// and this provider has no fence to hold.
func (p *workspaceProvider) InspectFenced(context.Context, storebinding.FencedInspectionRequest) (storebinding.Descriptor, error) {
	return storebinding.Descriptor{}, fmt.Errorf("%w: binding %q cannot be fenced, so no census can be taken under a fence",
		ErrWorkspaceLifecycleUnavailable, p.spec.Name)
}

// Open refuses: the typed front-door lifecycle opens a binding against a
// descriptor this provider never produces.
//
// A city serves this binding through the engine-opening seam instead, which
// asks for the store and nothing else. See OpenEngine.
func (p *workspaceProvider) Open(context.Context, storebinding.OpenRequest) (storebinding.OpenedBinding, error) {
	return nil, fmt.Errorf("%w: binding %q has no censused descriptor to open against; its classes are served through the engine-opening seam",
		ErrWorkspaceLifecycleUnavailable, p.spec.Name)
}

// BindingLocation reports the workspace directory this binding serves from, so
// a city records where it actually served rather than the reference it was
// asked to serve. Nothing is opened or stated to answer.
func (p *workspaceProvider) BindingLocation(spec storebinding.BindingSpec) (string, error) {
	if err := p.boundTo(spec); err != nil {
		return "", err
	}
	return p.root, nil
}

// RetainedGuards reports no retained-guard lifecycle: this provider installs
// and recovers no migration guard.
func (p *workspaceProvider) RetainedGuards() (storebinding.RetainedGuardLifecycle, bool) {
	return nil, false
}

// BindingMigration reports no binding-migration lifecycle: nothing here moves
// a city onto this binding, so no generation is ever activated.
func (p *workspaceProvider) BindingMigration() (storebinding.BindingMigrationLifecycle, bool) {
	return nil, false
}

// WorkMigration reports no Work-migration lifecycle.
func (p *workspaceProvider) WorkMigration() (storebinding.WorkMigrationLifecycle, bool) {
	return nil, false
}

// boundTo refuses a specification other than the one this facade was built
// for. A facade is bound to exactly one binding; answering for a second would
// silently report on a workspace the caller did not name.
func (p *workspaceProvider) boundTo(spec storebinding.BindingSpec) error {
	root, err := boundWorkspaceRoot(spec)
	if err != nil {
		return err
	}
	if spec.Name != p.spec.Name || spec.ConfigRef != p.spec.ConfigRef ||
		spec.URL != p.spec.URL || spec.Auth != p.spec.Auth || root != p.root {
		return fmt.Errorf("%w: specification does not match the bound binding %q", ErrInvalidWorkspaceBinding, p.spec.Name)
	}
	return nil
}

// boundWorkspaceRoot validates a specification as this provider's and resolves
// the workspace directory it names.
func boundWorkspaceRoot(spec storebinding.BindingSpec) (string, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	if spec.Provider != ProviderID {
		return "", fmt.Errorf("%w: provider %q", ErrInvalidWorkspaceBinding, spec.Provider)
	}
	if spec.Path != "" {
		return "", fmt.Errorf("%w: binding %q names a path; this provider's configuration is a config_ref naming a workspace", ErrInvalidWorkspaceBinding, spec.Name)
	}
	root, err := WorkspaceRoot(spec.CityRoot, string(spec.ConfigRef))
	if err != nil {
		return "", fmt.Errorf("%w (binding %q)", err, spec.Name)
	}
	return root, nil
}

// WorkspaceRoot resolves the workspace directory a city's configuration
// reference names: <city>/.gc/storage/<config_ref>.
//
// It is exported because the boot gate resolves the same directory when it
// records which location served a city, and two spellings of one layout drift.
//
// An absent city root is refused rather than defaulted. The tempting default
// is the process working directory, and it is wrong in the deployment that
// matters most: one supervisor process hosts every registered city and is
// started from wherever its launcher happened to be, so that default would
// point every city at one directory — or, with the same configuration
// reference in two cities, at one shared workspace. There is no base to guess.
//
// The reference is already restricted to an identifier by both config
// validation and the binding specification, so no separator can arrive here.
// A reference made only of dots is still an identifier and is refused: it
// would name the parent of the directory this layout reserves.
func WorkspaceRoot(cityRoot, configRef string) (string, error) {
	ref := strings.TrimSpace(configRef)
	if ref == "" {
		return "", fmt.Errorf("%w: no config_ref names the workspace", ErrInvalidWorkspaceBinding)
	}
	if strings.Trim(ref, ".") == "" {
		return "", fmt.Errorf("%w: config_ref %q does not name a workspace directory", ErrInvalidWorkspaceBinding, ref)
	}
	root := strings.TrimSpace(cityRoot)
	if root == "" {
		return "", fmt.Errorf("%w: config_ref %q names a workspace under a city, and the binding carries no CityRoot to resolve it against", ErrInvalidWorkspaceBinding, ref)
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: city root %q is not absolute", ErrInvalidWorkspaceBinding, root)
	}
	canonical, err := canonicalCityRoot(root)
	if err != nil {
		return "", fmt.Errorf("resolving the city root %q: %w", root, err)
	}
	return filepath.Join(canonical, ".gc", "storage", ref), nil
}

// canonicalCityRoot resolves a city's own path through its symlinks, the way
// the built-in provider canonicalizes a binding root.
//
// Two spellings of one city — the path a command was given and the path a
// symlink points at — must produce one location string, because that string is
// what a city records as the binding it served. Recorded under one spelling
// and recomputed under the other, it reads as a re-point and holds the boot.
//
// A city root that does not exist cannot be resolved and is cleaned instead:
// this runs on paths that have deliberately created nothing, and a missing
// city is the caller's problem to report, not a resolution failure.
func canonicalCityRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Clean(root), nil
	}
	return "", err
}

// workspaceStatePath is the directory the linked beads library keeps a
// workspace's state and configuration in.
func workspaceStatePath(root string) string {
	return filepath.Join(root, workspaceStateDir)
}

// workspaceConfigPath is the workspace's own configuration file: the one the
// linked library reads to learn which backend serves this workspace.
func workspaceConfigPath(root string) string {
	return filepath.Join(workspaceStatePath(root), workspaceConfigFile)
}

// workspaceLegacyConfigPath is the older spelling of the same file. The linked
// library still reads it (and migrates it), so a workspace carrying only this
// one is provisioned and must not be mistaken for an empty directory.
func workspaceLegacyConfigPath(root string) string {
	return filepath.Join(workspaceStatePath(root), workspaceLegacyConfigFile)
}

// fenceTarget describes the one component this binding has.
func (p *workspaceProvider) fenceTarget() (storebinding.FenceTarget, error) {
	classes, err := workspaceClasses()
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	return storebinding.NewFenceTarget(ProviderID, classes, []storebinding.FenceComponentTarget{{
		ID:               ComponentID,
		Locator:          workspaceLocator(p.root),
		PhysicalIdentity: workspaceIdentity(p.root),
		Classes:          classes,
	}})
}

// workspaceClasses is the complete set of classes one workspace can serve. The
// classes a given binding actually serves are the ones assigned to it, which
// the engine-opening seam checks against this set.
func workspaceClasses() (storebinding.ClassSet, error) {
	return storebinding.NewClassSet(
		coordclass.ClassWork,
		coordclass.ClassGraph,
		coordclass.ClassSessions,
		coordclass.ClassMessaging,
		coordclass.ClassOrders,
		coordclass.ClassNudges,
	)
}

// workspaceLocator spells where the workspace is, in the same file-URL form
// every other locator in this tree uses.
func workspaceLocator(root string) storebinding.ComponentLocator {
	return storebinding.ComponentLocator((&url.URL{Scheme: "file", Path: root}).String())
}

// workspaceIdentity is everything a stat of the workspace can honestly claim:
// where it is, and whether a provisioned workspace is there.
//
// It reads no deeper on purpose. Anything more specific — a generation, a
// schema, a byte count — is a fact about the backend serving the workspace,
// and reading it means opening the workspace, which an inspection must not do.
// So a workspace that appears or disappears changes identity, and one whose
// contents change does not: the honest reach of a directory stat.
func workspaceIdentity(root string) storebinding.PhysicalIdentity {
	state := "absent"
	if configured, err := workspaceIsConfigured(root); err == nil && configured {
		state = "present"
	}
	sum := sha256.Sum256([]byte("gascity.beads-workspace.component.v1\x00" + state + "\x00" + root))
	return storebinding.PhysicalIdentity("sha256:" + hex.EncodeToString(sum[:]))
}
