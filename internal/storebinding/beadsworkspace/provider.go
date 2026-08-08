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

	// workspaceMetadataDir is the directory the linked beads library keeps a
	// workspace's own state and configuration in. Its presence is what makes a
	// directory a workspace rather than an empty path.
	workspaceMetadataDir = ".beads"
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
	if spec.Name != p.spec.Name || spec.ConfigRef != p.spec.ConfigRef || root != p.root {
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
	return WorkspaceRoot(string(spec.ConfigRef))
}

// WorkspaceRoot resolves the workspace directory a configuration reference
// names: <city>/.gc/storage/<config_ref>.
//
// The city is the working directory, because nothing carries a city root to a
// provider. The binding specification a provider is handed has a name, a
// provider ID and this reference, and the engine-opening seam adds only the
// classes to serve — so the base a relative location resolves against is the
// process's own, exactly as it is for the built-in provider's `path`. A city
// is the working directory of the commands that serve it, which is what makes
// that the same directory in practice.
//
// The reference is already restricted to an identifier by both config
// validation and the binding specification, so no separator can arrive here.
// A reference made only of dots is still an identifier and is refused: it
// would name the parent of the directory this layout reserves.
func WorkspaceRoot(configRef string) (string, error) {
	ref := strings.TrimSpace(configRef)
	if ref == "" {
		return "", fmt.Errorf("%w: no config_ref names the workspace", ErrInvalidWorkspaceBinding)
	}
	if strings.Trim(ref, ".") == "" {
		return "", fmt.Errorf("%w: config_ref %q does not name a workspace directory", ErrInvalidWorkspaceBinding, ref)
	}
	root, err := filepath.Abs(filepath.Join(".gc", "storage", ref))
	if err != nil {
		return "", fmt.Errorf("resolving the workspace directory of config_ref %q: %w", ref, err)
	}
	return filepath.Clean(root), nil
}

// workspaceMetadataPath is the workspace's own state directory, the thing whose
// presence makes a path a workspace.
func workspaceMetadataPath(root string) string {
	return filepath.Join(root, workspaceMetadataDir)
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
// where it is, and whether it is there.
//
// It reads no deeper on purpose. Anything more specific — a generation, a
// schema, a byte count — is a fact about the backend serving the workspace,
// and reading it means opening the workspace, which an inspection must not do.
// So a workspace that appears or disappears changes identity, and one whose
// contents change does not: the honest reach of a directory stat.
func workspaceIdentity(root string) storebinding.PhysicalIdentity {
	state := "absent"
	if info, err := os.Stat(workspaceMetadataPath(root)); err == nil && info.IsDir() {
		state = "present"
	}
	sum := sha256.Sum256([]byte("gascity.beads-workspace.component.v1\x00" + state + "\x00" + root))
	return storebinding.PhysicalIdentity("sha256:" + hex.EncodeToString(sum[:]))
}
