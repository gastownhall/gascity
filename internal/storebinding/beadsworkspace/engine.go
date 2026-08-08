package beadsworkspace

// The serving half of the beads workspace provider: how a planned binding
// becomes the store the class front doors read and write through.
//
// The engine is not chosen here, and that is the design rather than an
// omission. This opens a workspace directory through the linked beads library;
// the library reads the workspace's own configuration and serves it with
// whichever backend that configuration names. Nothing in this file branches on
// the answer, so a workspace served by a backend the linked library gains
// later needs no edit here.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storebinding"
)

var _ storebinding.EngineOpener = (*workspaceProvider)(nil)

// OpenEngine opens this binding's workspace for the classes it serves.
//
// Everything it proves, it proves before or immediately after the open and
// never later: that the caller resolved this binding and not another, that the
// classes are ones a single workspace can serve, that the workspace exists,
// and that it mints ids under the reserved class prefix. The last one is the
// only check with a cost — it is answered from what the open already read —
// and it is the difference between a binding whose ids cannot collide with the
// work store's and one that merely probably does not.
func (p *workspaceProvider) OpenEngine(spec storebinding.BindingSpec, classes storebinding.ClassSet) (beads.Store, io.Closer, error) {
	if err := p.boundTo(spec); err != nil {
		return nil, nil, err
	}
	served, err := workspaceClasses()
	if err != nil {
		return nil, nil, err
	}
	if classes.Empty() {
		return nil, nil, fmt.Errorf("%w: binding %q opens for no class", ErrInvalidWorkspaceBinding, p.spec.Name)
	}
	if !served.Contains(classes) {
		return nil, nil, fmt.Errorf("%w: binding %q is assigned classes this provider does not serve", ErrInvalidWorkspaceBinding, p.spec.Name)
	}
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		return nil, nil, fmt.Errorf("%w: no reserved id prefix is registered for the %q class", ErrInvalidWorkspaceBinding, config.BeadClassGraph)
	}
	if err := p.workspacePresent(); err != nil {
		return nil, nil, err
	}

	store, err := beads.OpenNativeDoltStoreAt(context.Background(), p.root, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the workspace of binding %q at %s through the linked beads library: %w", p.spec.Name, p.root, err)
	}
	if err := p.mintsUnderReservedPrefix(store.IDPrefix(), prefix); err != nil {
		if closeErr := store.CloseStore(); closeErr != nil {
			return nil, nil, errors.Join(err, fmt.Errorf("closing the refused workspace of binding %q: %w", p.spec.Name, closeErr))
		}
		return nil, nil, err
	}
	return store, storeCloser{store}, nil
}

// workspacePresent refuses a binding whose workspace is not there.
//
// Opening it anyway would create one, and a workspace created this way has no
// configured id prefix — so the boot would succeed and the first write would
// fail somewhere inside the linked library, naming a directory the operator
// never chose to create. Refusing here says which directory is missing while
// that is still the whole problem.
func (p *workspaceProvider) workspacePresent() error {
	metadata := workspaceMetadataPath(p.root)
	info, err := os.Stat(metadata)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: binding %q names the workspace at %s, and %s is not there; create the workspace with the linked beads library's own tooling before a city serves from it",
			ErrWorkspaceUnavailable, p.spec.Name, p.root, metadata)
	}
	if err != nil {
		return fmt.Errorf("stating the workspace of binding %q at %s: %w", p.spec.Name, p.root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: binding %q names the workspace at %s, where %s is not a directory",
			ErrWorkspaceUnavailable, p.spec.Name, p.root, metadata)
	}
	return nil
}

// mintsUnderReservedPrefix proves the workspace mints bead ids under the
// reserved class prefix.
//
// A store that mints its own ids can be told which prefix to use. This one
// cannot: the prefix is the workspace's own configuration, read by the linked
// beads library at open, and imposing a different one from here would make
// this binding report a namespace it does not actually write. So the invariant
// — an id minted in this binding can never be mistaken for one from the work
// store — is held by requiring the prefix rather than by setting it. A
// workspace with no prefix configured is refused for the same reason it would
// fail its first write: it cannot mint an id at all.
func (p *workspaceProvider) mintsUnderReservedPrefix(observed, reserved string) error {
	if observed == reserved {
		return nil
	}
	if observed == "" {
		return fmt.Errorf("%w: the workspace of binding %q at %s has no id prefix configured, so it can mint no bead id; configure it with the reserved prefix %q",
			ErrInvalidWorkspaceBinding, p.spec.Name, p.root, reserved)
	}
	return fmt.Errorf("%w: the workspace of binding %q at %s mints ids under prefix %q, and a binding serving this city's classes must mint under the reserved prefix %q so its ids cannot be read as the work store's",
		ErrInvalidWorkspaceBinding, p.spec.Name, p.root, observed, reserved)
}

// storeCloser adapts the store's own close to io.Closer.
//
// beads.Store already has a Close method with a different meaning — closing
// one bead, not the store — so an engine handle cannot satisfy io.Closer
// directly.
type storeCloser struct {
	store interface{ CloseStore() error }
}

// Close releases the workspace handle this binding opened.
func (c storeCloser) Close() error { return c.store.CloseStore() }
