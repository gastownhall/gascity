package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// withheldSessionDoltPortKeys are the env keys a raw bd consults for its Dolt
// port before the scope's .beads/dolt-server.port mirror. BEADS_DOLT_PORT is
// a legacy alias bd still honors when BEADS_DOLT_SERVER_PORT is empty, so it
// is cleared alongside the projected keys.
var withheldSessionDoltPortKeys = [...]string{"GC_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// withholdMirroredManagedDoltPort keeps an agent session's env from pinning
// the Dolt port of a scope gc manages. A session's environment is fixed at
// spawn, and bd ranks an env port above the .beads/dolt-server.port mirror gc
// rewrites on every runtime publication, so a projected port would keep a
// long-lived session dialing the old server after the managed Dolt comes back
// on a new port. Leaving the port keys empty lets bd follow the mirror.
//
// The port is withheld only when the mirror can stand in for it: env carries
// a TCP port with no host or socket override (bd's port-file path dials
// 127.0.0.1), the scope is gc-managed, and the scope's mirror already reads
// that same port. Otherwise env is left as projected; a gc-managed scope whose
// mirror disagrees is logged, since gc keeps that mirror current.
func withholdMirroredManagedDoltPort(env map[string]string, cityPath, scopeRoot string) {
	port := strings.TrimSpace(env["GC_DOLT_PORT"])
	if port == "" || env["GC_DOLT_HOST"] != "" || env["BEADS_DOLT_SERVER_SOCKET"] != "" {
		return
	}
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	if !bdScopeDoltIsGcManaged(cityPath, scopeRoot) {
		return
	}
	mirror, err := readScopeDoltPortMirror(scopeRoot)
	if err != nil {
		log.Printf("gc: session env: keeping Dolt port %s projected for %s: %v", port, scopeRoot, err)
		return
	}
	if mirror != port {
		log.Printf("gc: session env: keeping Dolt port %s projected for %s: .beads/dolt-server.port reads %q", port, scopeRoot, mirror)
		return
	}
	for _, key := range withheldSessionDoltPortKeys {
		env[key] = ""
	}
}

// readScopeDoltPortMirror reads scopeRoot's .beads/dolt-server.port through
// any symlink, the way bd and writeDoltPortFile resolve it.
func readScopeDoltPortMirror(scopeRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "dolt-server.port"))
	if err != nil {
		return "", fmt.Errorf("reading Dolt port mirror: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
