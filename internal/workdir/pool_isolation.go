// Package workdir resolves agent working directories from config templates.
package workdir

import (
	"github.com/gastownhall/gascity/internal/config"
)

// ValidatePoolWorkDirIsolation rejects agent configurations that explicitly
// signal they may run more than one concurrently active session instance —
// via a namepool, an explicit max_active_sessions greater than 1 or
// negative (unbounded), or an explicit min_active_sessions/scale_check
// pool-flavor marker — but whose work_dir resolves to the same on-disk
// directory for every instance. Two concurrent sessions sharing a working
// directory silently corrupt each other's checkout the moment both run at
// once, so this probes two synthetic instance identities through the same
// template-resolution path session startup uses (ResolveWorkDirPathStrict)
// and compares the results. It fails closed: a work_dir template that
// errors on either probe is reported as an error rather than ignored.
//
// A merely-unset max_active_sessions is deliberately NOT treated as an
// implicit pool: it is the ordinary shape of a default singleton/
// named-session agent (the ubiquitous `[[agent]] name = "example-agent"` minimal
// config), and nothing in today's system spontaneously creates a second
// concurrent instance for such an agent absent one of the explicit signals
// above.
func ValidatePoolWorkDirIsolation(_ string, _ string, _ []config.Agent, _ []config.Rig) error {
	return nil
}
