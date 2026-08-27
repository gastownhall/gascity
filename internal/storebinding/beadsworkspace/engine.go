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
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/storebinding"
)

var _ storebinding.EngineOpener = (*workspaceProvider)(nil)

var workspaceExecutable = os.Executable

// workspaceEngine is what an opened workspace has to be for a binding to serve
// from it: a bead store that reports the prefix it mints under and owns a
// handle it can release. It is an interface so the admission decision below —
// including the close that has to follow a refusal — is provable without a
// live workspace behind it.
type workspaceEngine interface {
	beads.Store
	IDPrefix() string
	CloseStore() error
}

// OpenEngine opens this binding's workspace for the classes it serves.
//
// Everything it proves, it proves before the open or immediately after, never
// later: that the caller resolved this binding and not another, that the
// classes are ones a single workspace can serve, that the directory really is
// a provisioned workspace, and that it mints ids under the reserved class
// prefix. Only the last needs the workspace open, and it is answered from what
// the open already read.
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
	if err := p.workspaceProvisioned(); err != nil {
		return nil, nil, err
	}

	store, err := p.openWorkspace(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the workspace of binding %q at %s through the linked beads library: %w", p.spec.Name, p.root, err)
	}
	return p.admit(store, prefix)
}

// openWorkspace withholds the ambient BEADS_ namespace from every open. The
// exact remote credential-provider selector adds only a command that calls
// back into this running gc binary; local workspaces and other auth forms stay
// fully hermetic.
func (p *workspaceProvider) openWorkspace(spec storebinding.BindingSpec) (workspaceEngine, error) {
	if spec.Provider != ProviderID || strings.TrimSpace(spec.URL) == "" || spec.Auth != storebinding.AuthCredentialProvider {
		return beads.OpenNativeDoltStoreAtWithoutAmbientEnv(context.Background(), p.root)
	}
	executable, err := workspaceExecutable()
	if err != nil {
		return nil, fmt.Errorf("resolving the running gc executable for the credential provider: %w", err)
	}
	if strings.TrimSpace(executable) == "" {
		return nil, errors.New("resolving the running gc executable for the credential provider: empty path")
	}
	if !filepath.IsAbs(executable) {
		executable, err = filepath.Abs(executable)
		if err != nil {
			return nil, fmt.Errorf("resolving the running gc executable to an absolute path: %w", err)
		}
	}
	command := shellquote.Quote(executable) + " internal beads-credential"
	reopen := func(ctx context.Context) (beads.NativeStorage, error) {
		return beads.OpenNativeStorageAtWithoutAmbientEnvWithCredentialCommand(ctx, p.root, command)
	}
	return beads.OpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommand(
		context.Background(), p.root, command, beads.WithNativeReopen(reopen))
}

// admit hands back an opened workspace this binding may serve from, and closes
// one it may not. A refused open that leaves its handle behind holds the
// workspace for the life of a process that is about to refuse to start, which
// is the failure such a boot is least able to explain.
func (p *workspaceProvider) admit(engine workspaceEngine, reserved string) (beads.Store, io.Closer, error) {
	if err := p.mintsUnderReservedPrefix(engine.IDPrefix(), reserved); err != nil {
		if closeErr := engine.CloseStore(); closeErr != nil {
			return nil, nil, errors.Join(err, fmt.Errorf("closing the refused workspace of binding %q: %w", p.spec.Name, closeErr))
		}
		return nil, nil, err
	}
	return engine, storeCloser{engine}, nil
}

// workspaceProvisioned refuses a binding whose directory is not a workspace
// somebody provisioned.
//
// The test is the presence of the workspace's own configuration file, and the
// reason is that the linked library treats its absence as "no configuration"
// and falls back to defaults — which for a fresh directory means creating a
// complete engine on the spot. Any weaker directory test lets a boot that is
// about to refuse build a database first: an empty directory passes, the
// library populates it, and the prefix check then rejects the workspace it
// just caused to exist. Requiring the file that NAMES the backend also makes
// this provider's own premise true, because a workspace with no configuration
// has not named one.
func (p *workspaceProvider) workspaceProvisioned() error {
	configured, err := workspaceIsConfigured(p.root)
	if err != nil {
		return fmt.Errorf("reading the workspace configuration of binding %q at %s: %w", p.spec.Name, p.root, err)
	}
	if configured {
		return nil
	}
	return fmt.Errorf("%w: binding %q names the workspace at %s, and its configuration %s is not there; provision the workspace with the linked beads library's own tooling before a city serves from it",
		ErrWorkspaceUnavailable, p.spec.Name, p.root, workspaceConfigPath(p.root))
}

// workspaceIsConfigured reports whether the workspace holds the configuration
// file the linked library reads, in either the current or the legacy spelling
// that library still accepts. A read failure other than absence is returned
// rather than swallowed: a directory this build cannot look at decides nothing.
func workspaceIsConfigured(root string) (bool, error) {
	for _, path := range []string{workspaceConfigPath(root), workspaceLegacyConfigPath(root)} {
		info, err := os.Stat(path)
		if err == nil {
			return info.Mode().IsRegular(), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
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
