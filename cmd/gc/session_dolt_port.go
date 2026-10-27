package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// scopeDoltPortMirror reads <scopeRoot>/.beads/dolt-server.port — the raw-bd
// compatibility mirror gc's runtime publication rewrites on every managed
// Dolt state change — through any operator symlink, as bd and writeDoltPortFile
// do. An absent mirror reads as "" with no error.
func scopeDoltPortMirror(scopeRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "dolt-server.port"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// withholdMirroredManagedDoltPort removes the managed Dolt TCP port from a
// freshly built session env when the scope's own .beads/dolt-server.port
// mirror already carries the same port (gastownhall/gascity#1656).
//
// A session's environment is frozen at spawn time, and bd ranks an env port
// above the port-file mirror, so a projected GC_DOLT_PORT keeps every
// long-lived session dialing the port dolt ran on at spawn — reporting
// `database not found` after a restart moves dolt to a new port — while the
// mirror beside it already tracks the live one. Withholding the env port
// sends the session's raw `bd` to the mirror gc keeps current, at the cost of
// one extra file read per bd invocation.
//
// The port is withheld only when the mirror can stand in for it: a TCP port
// was projected (no socket target), no host is projected (bd's port-file path
// dials 127.0.0.1, so it cannot serve a container/remote endpoint), the scope
// is gc-managed (externally-bound, explicit and external targets keep today's
// projection), and the mirror exists and equals the projected port. On any
// mismatch the projection is kept — mirror drift is a gc bug elsewhere, and
// today's behavior is the safe fallback — with one `gc: session env:` log
// line so the drift is observable.
//
// The withheld keys stay present but empty, following setProjectedDoltEnvEmpty,
// so tmux/subprocess providers unset any stale inherited value. BEADS_DOLT_PORT
// is cleared alongside BEADS_DOLT_SERVER_PORT because bd still honors the
// legacy alias when the primary key is empty.
func withholdMirroredManagedDoltPort(env map[string]string, cityPath, scopeRoot string) {
	if env == nil {
		return
	}
	port := strings.TrimSpace(env["GC_DOLT_PORT"])
	if port == "" {
		return
	}
	if strings.TrimSpace(env["GC_DOLT_HOST"]) != "" || strings.TrimSpace(env["BEADS_DOLT_SERVER_SOCKET"]) != "" {
		return
	}
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	if !bdScopeDoltIsGcManaged(cityPath, scopeRoot) {
		return
	}
	mirror, err := scopeDoltPortMirror(scopeRoot)
	if err != nil || mirror != port {
		log.Printf("gc: session env: keeping projected Dolt port %s for %s: .beads/dolt-server.port mirror %q cannot stand in for it (read error: %v)", port, scopeRoot, mirror, err)
		return
	}
	env["GC_DOLT_PORT"] = ""
	env["BEADS_DOLT_SERVER_PORT"] = ""
	env["BEADS_DOLT_PORT"] = ""
}
