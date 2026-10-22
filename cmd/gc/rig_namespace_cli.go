package main

import (
	"fmt"
	"strings"
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
	if rigRouteQualified(actor) || isPoolSessionBeadID(strings.TrimSpace(actor)) {
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
	return rigRouteQualified(v) && ns.admitsLexically(v)
}
