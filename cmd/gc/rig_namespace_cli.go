package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// rigNamespacePathError is one refusal of the rig namespace guard on a
// non-store path (the hook claim cycle, agent-script, gc bd). Msg is the
// complete, user-facing text; it wraps errRigNamespace
// (config.ErrRigNamespace), the sentinel every guard refusal shares.
type rigNamespacePathError struct {
	Msg string
}

func (e *rigNamespacePathError) Error() string { return e.Msg }

// Is makes errors.Is(err, config.ErrRigNamespace) true for every refusal.
func (e *rigNamespacePathError) Is(target error) bool { return target == errRigNamespace }

func newRigNamespacePathError(format string, args ...any) error {
	return &rigNamespacePathError{Msg: fmt.Sprintf(format, args...)}
}

// rigNamespaceLabel names a guarded rig in a refusal; a deny-all namespace
// (an ambiguous guarded scope) has no rig name.
func rigNamespaceLabel(ns *rigNamespace) string {
	if name := ns.rigName(); name != "" {
		return name
	}
	return "(ambiguous guarded scope)"
}

// rigNamespaceShown is the namespace as a refusal prints it.
func rigNamespaceShown(ns *rigNamespace) string {
	if v := ns.namespace(); v != "" {
		return v
	}
	return "(nothing)"
}

// rigNamespaceActorHint is appended to a refused actor: a value that is not
// rig-qualified is typically a city-scoped agent's name or a store-minted
// session id, which a guarded rig never admits.
func rigNamespaceActorHint(ns *rigNamespace, actor string) string {
	if config.RigRouteQualified(actor) || config.IsPoolSessionBeadID(strings.TrimSpace(actor)) {
		return ""
	}
	return fmt.Sprintf("; give the agent a rig alias (named session) or run it as a pool of rig %q", rigNamespaceLabel(ns))
}

// rigNamespacePinAdmitted reports whether v may be written as an assignee
// (a claim actor or a continuation pin) into the guarded rig's store.
func rigNamespacePinAdmitted(ns *rigNamespace, v string) bool {
	return strings.TrimSpace(v) != "" && ns.admits(v)
}

// rigNamespaceRouteAdmitted reports whether v may be written as a routing
// value into the guarded rig's store: it must name one of the rig's own
// agents (lexical admission) and be rig-qualified with no empty segment. A
// pool session id is admitted as an actor, never as a route.
func rigNamespaceRouteAdmitted(ns *rigNamespace, v string) bool {
	return config.RigRouteQualified(v) && ns.admitsLexically(v)
}

// rigNamespaceForLeg returns the namespace a row read from (or written to) a
// leg is judged by. The leg — the store the row actually lives in — decides:
// a row in a guarded rig's store is that rig's to judge whatever its id
// prefix, since imported rows keep their original prefixes and bd mints from
// each directory's own issue prefix. The bead's prefix only adds a deny: a
// prefix mapping to a different guarded rig (or to two) admits nothing. On a
// leg that is not a guarded rig's store the prefix alone decides, as before.
// It returns nil when neither names a guarded rig.
func rigNamespaceForLeg(leg *rigNamespace, cfg *config.City, beadID string, sessions ownSessionResolver) *rigNamespace {
	prefix, prefixGuarded := rigNamespaceForBeadID(cfg, beadID, sessions)
	if leg == nil {
		if prefixGuarded {
			return prefix
		}
		return nil
	}
	if prefixGuarded && prefix.rigName() != leg.rigName() {
		return denyAllRigNamespace(sessions)
	}
	return leg
}

// rigNamespaceForScopeOrDeny is the shared rigNamespaceForScope (by store
// identity, following redirects) for a path that must decide now: a scope
// the shared helper refuses to open (two guarded rigs on one store, or a
// guarded rig whose store is not its own, an unguarded rig sharing it
// included) gives a deny-all namespace, never "unguarded".
func rigNamespaceForScopeOrDeny(cfg *config.City, cityPath, scopeRoot string, sessions ownSessionResolver) *rigNamespace {
	if strings.TrimSpace(scopeRoot) == "" {
		return nil
	}
	ns, err := rigNamespaceForScope(cfg, cityPath, normalizePathForCompare(resolveStoreScopeRoot(cityPath, scopeRoot)), sessions)
	if err != nil {
		return denyAllRigNamespace(sessions)
	}
	return ns
}

// guardedRigNamespaceForDir returns the namespace of the guarded rig whose
// store a bd command run in dir opens, or nil (rigNamespaceForScopeOrDeny:
// subdirectories and redirected worktrees resolve by store identity).
func guardedRigNamespaceForDir(cfg *config.City, cityPath, dir string, sessions ownSessionResolver) *rigNamespace {
	if cfg == nil || strings.TrimSpace(dir) == "" {
		return nil
	}
	return rigNamespaceForScopeOrDeny(cfg, cityPath, dir, sessions)
}

// guardedRigNamespaceForRigName returns the guarded namespace the store of
// the configured rig called name belongs to, by store identity, else nil
// (also for an unknown name). A guarded rig whose store is not its own
// admits nothing.
func guardedRigNamespaceForRigName(cfg *config.City, cityPath, name string, sessions ownSessionResolver) *rigNamespace {
	name = strings.TrimSpace(name)
	ns, guarded, known := rigNamespaceForRigName(cfg, name, sessions)
	if !known {
		return nil
	}
	for i := range cfg.Rigs {
		r := cfg.Rigs[i]
		if r.Name != name {
			continue
		}
		if guarded {
			if err := checkGuardedRigStore(cfg, cityPath, r); err != nil {
				return denyAllRigNamespace(sessions)
			}
			return ns
		}
		return rigNamespaceForScopeOrDeny(cfg, cityPath, r.Path, sessions)
	}
	return ns
}
