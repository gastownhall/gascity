// session_resolve.go provides CLI-level session resolution.
// The core resolution logic lives in internal/session.ResolveSessionID.
package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// resolveSessionID delegates to session.ResolveSessionID.
func resolveSessionID(store beads.Store, identifier string) (string, error) {
	return session.ResolveSessionID(store, identifier)
}

func resolveSessionIDAllowClosed(store beads.Store, identifier string) (string, error) {
	return session.ResolveSessionIDAllowClosed(store, identifier)
}
