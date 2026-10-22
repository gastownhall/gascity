package agentutil

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// ErrRigNamespaceRoute marks a route refused because it would write an
// identity outside a guarded rig's namespace (federation = "rig") into that
// rig's store. It wraps config.ErrRigNamespace, the sentinel every guard
// refusal shares; errors.Is reports both for every *RigNamespaceRouteError.
var ErrRigNamespaceRoute = fmt.Errorf("route refused: %w", config.ErrRigNamespace)

// RigNamespaceRouteError is a refused route into a guarded rig. --force
// never overrides it.
type RigNamespaceRouteError struct {
	// Rig is the guarded rig's name; empty for an ambiguous guarded scope
	// (a bead prefix two guarded rigs share), which admits nothing.
	Rig string
	// Target is the refused route or assignee value.
	Target string
	// Namespace is the rig's identity namespace ("<rig>/").
	Namespace string
	// Key names the field the value was headed for (gc.routed_to,
	// assignee, ...); empty for a sling target.
	Key string
}

func (e *RigNamespaceRouteError) Error() string {
	rig := e.Rig
	if rig == "" {
		rig = "(ambiguous guarded scope)"
	}
	ns := e.Namespace
	if ns == "" {
		ns = "(nothing)"
	}
	field := ""
	if e.Key != "" {
		field = " (" + e.Key + ")"
	}
	return fmt.Sprintf("refusing route into rig %q: target %q%s is outside its namespace %q (federation = \"rig\"); route to one of the rig's own agents. --force does not override this.", rig, e.Target, field, ns)
}

// Is reports ErrRigNamespaceRoute and config.ErrRigNamespace.
func (e *RigNamespaceRouteError) Is(target error) bool {
	return target == ErrRigNamespaceRoute || target == config.ErrRigNamespace
}

// GuardedRigForStoreRef returns the guarded rig a workflow store ref names
// ("rig:<name>", exact name), or nil when the ref names no rig or an
// unguarded one.
func GuardedRigForStoreRef(cfg *config.City, storeRef string) *config.Rig {
	if cfg == nil {
		return nil
	}
	name, scoped := storeref.ScopeRigContext(storeRef)
	if !scoped || name == "" {
		return nil
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == name && cfg.Rigs[i].FederationGuarded() {
			r := cfg.Rigs[i]
			return &r
		}
	}
	return nil
}

// GuardedRigForPrefix returns the guarded rig whose effective bead prefix is
// prefix (case-insensitive), or nil. Two guarded rigs sharing the prefix
// give a nameless guarded rig, which admits nothing (fail closed).
func GuardedRigForPrefix(cfg *config.City, prefix string) *config.Rig {
	prefix = strings.TrimSpace(prefix)
	if cfg == nil || prefix == "" {
		return nil
	}
	var found *config.Rig
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if !r.FederationGuarded() || !strings.EqualFold(r.EffectivePrefix(), prefix) {
			continue
		}
		if found != nil && found.Name != r.Name {
			return &config.Rig{Federation: config.RigFederationRig}
		}
		found = r
	}
	if found == nil {
		return nil
	}
	r := *found
	return &r
}

// RigRouteAdmitted reports whether v may be written as a routing value into
// guarded rig r's store: its fold is inside the rig's namespace, it is
// rig-qualified with no empty segment, and it does not name a pool session.
// A bare (city-scoped) route in a shared store is served by every city that
// defines it, so it is never admitted.
func RigRouteAdmitted(r *config.Rig, v string) bool {
	if r == nil {
		return true
	}
	if !config.RigRouteQualified(v) || config.EmbedsPoolSessionID(v) {
		return false
	}
	return r.AdmitsLexically(v)
}

// RigRouteRefusal returns the refusal of writing v under key into guarded
// rig r's store, or nil when r is nil or admits v. Empty values are not
// writes and are never refused.
func RigRouteRefusal(r *config.Rig, key, v string) error {
	if r == nil || strings.TrimSpace(v) == "" || RigRouteAdmitted(r, v) {
		return nil
	}
	return &RigNamespaceRouteError{Rig: r.Name, Target: strings.TrimSpace(v), Namespace: r.IdentityNamespace(), Key: key}
}
