package main

// Shared pieces of the rig namespace guard (federation = "rig") that every
// enforcement point uses: the guarded store (rig_namespace_store.go), the
// non-store claim and routing paths, gc bd, and gc agent is-foreign with the
// orphan sweep. Keep one copy of each here.

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// errRigNamespace is config.ErrRigNamespace, the sentinel every guard
// refusal wraps.
var errRigNamespace = config.ErrRigNamespace

// cityHasGuardedRig reports whether any rig of cfg sets federation = "rig".
// Every guard step checks it first, so a city without one takes none of the
// guard's paths and does no session lookup.
func cityHasGuardedRig(cfg *config.City) bool {
	if cfg == nil {
		return false
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].FederationGuarded() {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Which guarded rig owns a scope's store.

// guardedRigScopeRoot returns the resolved store root of rig, or "" when the
// rig has no path (and so no store of its own).
func guardedRigScopeRoot(cityPath string, rig config.Rig) string {
	if strings.TrimSpace(rig.Path) == "" {
		return ""
	}
	return normalizePathForCompare(resolveStoreScopeRoot(cityPath, rig.Path))
}

// guardedRigForScope returns the guarded rig whose bead store the scope at
// scopeRoot opens, or nil when that store is no guarded rig's. The city's own
// root is never a guarded rig's. A scope is a guarded rig's when it is the
// rig's root or opens the same store by identity (rigStoreIdentity: the
// .beads directory after a redirect, the project id, the database), which
// covers subdirectories, redirected worktrees and other rigs, guarded or not,
// whose metadata names the rig's store.
//
// It returns an error, and the scope must not be opened, when two guarded
// rigs match, or when the matching rig's store is not its own
// (checkGuardedRigStore): an unguarded rig sharing a guarded rig's store is
// refused here too, whichever of the two is being opened.
func guardedRigForScope(cfg *config.City, cityPath, scopeRoot string) (*config.Rig, error) {
	if !cityHasGuardedRig(cfg) {
		return nil, nil
	}
	scopeRoot = normalizePathForCompare(scopeRoot)
	if scopeRoot == "" || scopeRoot == normalizePathForCompare(resolveStoreScopeRoot(cityPath, cityPath)) {
		return nil, nil
	}
	var scopeID *rigStoreIdentity
	var found *config.Rig
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if !r.FederationGuarded() {
			continue
		}
		root := guardedRigScopeRoot(cityPath, *r)
		if root == "" {
			continue
		}
		match := root == scopeRoot
		if !match {
			if scopeID == nil {
				id := rigStoreIdentityFor(scopeRoot)
				scopeID = &id
			}
			match = scopeID.sharesStoreWith(rigStoreIdentityFor(root))
		}
		if !match {
			continue
		}
		if found != nil && found.Name != r.Name {
			return nil, fmt.Errorf("opening the bead store at %s: rig %q and rig %q (federation = \"rig\") share it; give each its own store: %w", scopeRoot, found.Name, r.Name, errRigNamespace)
		}
		found = r
	}
	if found == nil {
		return nil, nil
	}
	if err := checkGuardedRigStore(cfg, cityPath, *found); err != nil {
		return nil, err
	}
	r := *found
	return &r, nil
}

// rigNamespaceForScope is guardedRigForScope as the namespace that guards the
// scope's store, resolving its sessions through sessions (this city's own
// resolver).
func rigNamespaceForScope(cfg *config.City, cityPath, scopeRoot string, sessions ownSessionResolver) (*rigNamespace, error) {
	rig, err := guardedRigForScope(cfg, cityPath, scopeRoot)
	if err != nil || rig == nil {
		return nil, err
	}
	ns, _ := newRigNamespace(*rig, sessions)
	return ns, nil
}

// checkGuardedRigStore refuses a guarded rig whose bead store is not its own:
// a rig with no store directory of its own, one at the city root, or one
// whose store (by identity) is the city's or any other rig's. Another city
// would then reach this city's other rows through the shared store, this
// rig's rows would be guarded by two namespaces, or an unguarded rig would
// reach them with no guard.
func checkGuardedRigStore(cfg *config.City, cityPath string, rig config.Rig) error {
	root := guardedRigScopeRoot(cityPath, rig)
	if root == "" {
		return guardedRigAliasError(rig.Name, "it has no path, so its bead store would be the city's")
	}
	cityRoot := normalizePathForCompare(resolveStoreScopeRoot(cityPath, cityPath))
	if root == cityRoot {
		return guardedRigAliasError(rig.Name, "it sits at the city root, so its bead store is the city's")
	}
	id, own := guardedRigStoreIdentity(cityPath, root)
	if !own {
		return guardedRigAliasError(rig.Name, "it has no bead store of its own (no .beads directory at "+root+"), so it would open the city's")
	}
	cityID, _ := guardedRigStoreIdentity(cityPath, cityRoot)
	if id.sharesStoreWith(cityID) {
		return guardedRigAliasError(rig.Name, "its bead store is the city's")
	}
	for i := range cfg.Rigs {
		other := cfg.Rigs[i]
		if other.Name == rig.Name {
			continue
		}
		otherRoot := guardedRigScopeRoot(cityPath, other)
		if otherRoot == "" {
			continue
		}
		otherID, _ := guardedRigStoreIdentity(cityPath, otherRoot)
		if otherRoot == root || id.sharesStoreWith(otherID) {
			return guardedRigAliasError(rig.Name, fmt.Sprintf("its bead store is rig %q's", other.Name))
		}
	}
	return nil
}

func guardedRigAliasError(rigName, why string) error {
	return fmt.Errorf("rig %q (federation = \"rig\"): %s; give it its own store: %w", rigName, why, errRigNamespace)
}

// guardedRigStoreIdentity is rigStoreIdentityFor with the file provider's
// layout: a scope-local file store is the scope's own; the legacy shared file
// store is the city's for every scope. own reports whether the scope holds a
// store of its own. An exec provider's store cannot be inspected and counts
// as the scope's own.
func guardedRigStoreIdentity(cityPath, root string) (rigStoreIdentity, bool) {
	provider := strings.TrimSpace(rawBeadsProviderForScope(root, cityPath))
	switch {
	case strings.HasPrefix(provider, "exec:"):
		return rigStoreIdentity{keys: []string{"exec:" + root}}, true
	case provider == "file":
		if scopeUsesFileStoreContract(root) || fileStoreUsesScopedRoots(cityPath) {
			return rigStoreIdentity{keys: []string{"file:" + filepath.Join(root, ".gc", "beads.json")}}, true
		}
		cityRoot := normalizePathForCompare(resolveStoreScopeRoot(cityPath, cityPath))
		return rigStoreIdentity{keys: []string{"file:" + filepath.Join(cityRoot, ".gc", "beads.json")}}, root == cityRoot
	}
	id := rigStoreIdentityFor(root)
	return id, id.own
}

// ---------------------------------------------------------------------------
// This city's session id prefixes and one-shot resolver.

// ownSessionIDPrefixes returns every session id prefix a city's resolver
// (citySessionResolver) must look up: the sessions store's own prefix
// (beads.IDPrefixFor, through the handle and the bead-policy layer) and
// the city's HQ prefix, for records migrated from the city store. Never a
// rig's prefix. sessions may be the zero handle when the store is not open
// yet; the resolver then learns the store's prefix when it opens it.
func ownSessionIDPrefixes(cfg *config.City, sessions beads.SessionStore) []string {
	var hq string
	if cfg != nil {
		hq = config.EffectiveHQPrefix(cfg)
	}
	return normalizeSessionIDPrefixes([]string{beads.IDPrefixFor(sessions), hq})
}

// newCityCLISessionResolver returns a one-shot command's session resolver for
// the city at cityPath: the caller's own session from the environment, then
// the city's sessions store, opened lazily. openSessions must return the
// city's session-class handle over its own bead store
// (cliStores(cityPath, cityStore).Sessions()), never a rig store, which other
// cities share; it is called only when a lookup needs the store (again after
// a failure). Every own session id prefix is looked up: the HQ prefix up
// front and the sessions store's own once it is open. The supervisor and the
// controller never build one.
func newCityCLISessionResolver(cityPath string, cfg *config.City, openSessions func() (beads.SessionStore, error)) *citySessionResolver {
	return newCLISessionResolver(cityPath, func() (beads.SessionStore, error) {
		if openSessions == nil {
			return beads.SessionStore{}, errors.New("no city store")
		}
		return openSessions()
	}, ownSessionIDPrefixes(cfg, beads.SessionStore{})...)
}
