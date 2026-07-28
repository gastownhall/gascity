package workdir

import (
	"fmt"
	"strings"

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
// named-session agent (the ubiquitous `[[agent]] name = "mayor"` minimal
// config), and nothing in today's system spontaneously creates a second
// concurrent instance for such an agent absent one of the explicit signals
// above.
func ValidatePoolWorkDirIsolation(cityPath, cityName string, agents []config.Agent, rigs []config.Rig) error {
	for i := range agents {
		a := agents[i]
		if !requiresPoolWorkDirIsolationCheck(a) {
			continue
		}

		firstName := a.QualifiedInstanceName(a.Name + "-1")
		secondName := a.QualifiedInstanceName(a.Name + "-2")

		firstPath, err := ResolveWorkDirPathStrict(cityPath, cityName, firstName, a, rigs)
		if err != nil {
			return fmt.Errorf("agent %q: resolving work_dir for pool instance %q: %w", a.QualifiedName(), firstName, err)
		}
		secondPath, err := ResolveWorkDirPathStrict(cityPath, cityName, secondName, a, rigs)
		if err != nil {
			return fmt.Errorf("agent %q: resolving work_dir for pool instance %q: %w", a.QualifiedName(), secondName, err)
		}

		if firstPath == secondPath {
			return fmt.Errorf(
				"agent %q supports multiple concurrent sessions but work_dir does not vary per instance (instances %q and %q both resolve to %q); "+
					"set work_dir to a template that includes {{.AgentBase}} (or another value that differs per instance) so concurrent sessions get isolated working directories",
				a.QualifiedName(), firstName, secondName, firstPath,
			)
		}
	}
	return nil
}

// requiresPoolWorkDirIsolationCheck reports whether the agent carries an
// explicit configuration signal that it may run more than one concurrently
// active session — as opposed to merely defaulting to an unset
// max_active_sessions, which SupportsExpandedSessionIdentities treats as
// "unlimited" for identity-discovery purposes but which is also the
// ordinary shape of a default singleton/named-session agent. Isolation
// enforcement needs the narrower, explicit-only reading so it does not hard
// fail every minimally-configured agent that never opted into pooling.
func requiresPoolWorkDirIsolationCheck(a config.Agent) bool {
	if !a.SupportsExpandedSessionIdentities() {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	if a.MinActiveSessions != nil || strings.TrimSpace(a.ScaleCheck) != "" {
		return true
	}
	return a.MaxActiveSessions != nil
}
