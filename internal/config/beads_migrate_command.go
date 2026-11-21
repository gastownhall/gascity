package config

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// BeadsMigrateCommandData is what a [beads] migrate_command template sees for
// one bd database whose schema is behind the beads library linked into gc.
// Every value is raw; the template's shq function quotes one for a POSIX
// shell.
type BeadsMigrateCommandData struct {
	// City is the city directory.
	City string
	// Rig is the rig's name, empty for the city's own database.
	Rig string
	// ScopeDir is the directory of the scope (the city or the rig) whose
	// database this is.
	ScopeDir string
	// BeadsDir is the scope's .beads directory.
	BeadsDir string
	// Database is the database's name on its Dolt server.
	Database string
	// Host and Port are the Dolt server's TCP endpoint, empty when it is
	// served on a Unix socket.
	Host string
	Port string
	// Socket is the Dolt server's Unix socket, empty when it is served over
	// TCP.
	Socket string
	// Bd is the bd binary the city pins for this scope, or "bd" when gc
	// could not resolve one.
	Bd string
}

// beadsMigrateCommandFuncs are the functions a migrate_command template may
// call besides text/template's builtins.
var beadsMigrateCommandFuncs = template.FuncMap{
	"shq": shellQuotePOSIX,
}

// shellQuotePOSIX quotes s as one POSIX shell word, leaving it bare when it
// holds only characters no shell treats specially.
func shellQuotePOSIX(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') &&
			r != '/' && r != '.' && r != '_' && r != '-'
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RenderBeadsMigrateCommand renders a [beads] migrate_command template for one
// database, trimmed of surrounding space. It fails on a template that does not
// parse or execute, and on one that renders blank or on more than one line.
func RenderBeadsMigrateCommand(tmpl string, data BeadsMigrateCommandData) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return "", errors.New("is blank; remove it to use gc's default command")
	}
	if strings.ContainsAny(strings.TrimSpace(tmpl), "\r\n") {
		return "", errors.New("spans more than one line; it must be a single shell command line")
	}
	t, err := template.New("migrate_command").Funcs(beadsMigrateCommandFuncs).Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	out := strings.TrimSpace(b.String())
	switch {
	case out == "":
		return "", errors.New("renders blank")
	case strings.ContainsAny(out, "\r\n"):
		return "", errors.New("renders more than one line")
	}
	return out, nil
}

// validateBeadsMigrateCommand rejects at load time a [beads] migrate_command
// that does not parse or that fails to render a one-line command for sample
// databases: a rig's and the city's own (no rig), each on a TCP and on a
// socket endpoint. That catches unknown fields and functions on every path
// the samples take; a branch only some real database takes is checked when a
// notice renders it, which falls back to gc's default command and says why.
func validateBeadsMigrateCommand(raw string) error {
	if raw == "" {
		return nil
	}
	rig := BeadsMigrateCommandData{
		City:     "/city",
		Rig:      "rig",
		ScopeDir: "/city/rigs/rig",
		BeadsDir: "/city/rigs/rig/.beads",
		Database: "db",
		Host:     "127.0.0.1",
		Port:     "3307",
		Bd:       "/usr/local/bin/bd",
	}
	city := rig
	city.Rig, city.ScopeDir, city.BeadsDir = "", rig.City, rig.City+"/.beads"
	for _, data := range []BeadsMigrateCommandData{rig, city} {
		socket := data
		socket.Host, socket.Port, socket.Socket = "", "", "/run/dolt.sock"
		for _, d := range []BeadsMigrateCommandData{data, socket} {
			if _, err := RenderBeadsMigrateCommand(raw, d); err != nil {
				return fmt.Errorf("beads.migrate_command: %w", err)
			}
		}
	}
	return nil
}
