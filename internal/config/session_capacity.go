package config

import (
	"strings"
	"time"
)

// DrainTimeoutDuration returns the drain timeout as a time.Duration.
// Defaults to 5m if empty or unparseable.
func (a *Agent) DrainTimeoutDuration() time.Duration {
	if a.DrainTimeout == "" {
		return 5 * time.Minute
	}
	dur, err := time.ParseDuration(a.DrainTimeout)
	if err != nil {
		return 5 * time.Minute
	}
	return dur
}

// EffectiveMaxActiveSessions returns the agent's max active sessions.
// Priority: agent.MaxActiveSessions > pool.Max > nil (unlimited).
func (a *Agent) EffectiveMaxActiveSessions() *int {
	return a.MaxActiveSessions // nil = unlimited (default)
}

// EffectiveMinActiveSessions returns the agent's min active sessions.
func (a *Agent) EffectiveMinActiveSessions() int {
	if a.MinActiveSessions != nil && *a.MinActiveSessions > 0 {
		return *a.MinActiveSessions
	}
	return 0
}

// SupportsGenericEphemeralSessions reports whether the template may satisfy
// generic controller demand with ephemeral sessions.
func (a *Agent) SupportsGenericEphemeralSessions() bool {
	if a == nil {
		return false
	}
	if m := a.EffectiveMaxActiveSessions(); m != nil && *m == 0 {
		return false
	}
	return true
}

// SupportsMultipleSessions reports whether the template may materialize more
// than one distinct concrete session identity. Unlike
// SupportsGenericEphemeralSessions, max_active_sessions = 0 still represents a
// multi-session template shape even though generic ephemeral session creation
// is disabled.
func (a *Agent) SupportsMultipleSessions() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	maxSessions := a.EffectiveMaxActiveSessions()
	return maxSessions == nil || *maxSessions != 1
}

// UsesCanonicalSingletonPoolIdentity reports whether singleton pool-shaped
// surfaces should use the configured agent identity instead of synthesizing a
// slot identity such as "{name}-1".
func (a *Agent) UsesCanonicalSingletonPoolIdentity() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return false
	}
	maxSessions := a.EffectiveMaxActiveSessions()
	return maxSessions != nil && *maxSessions == 1
}

// SupportsExpandedSessionIdentities reports whether callers should expose or
// discover concrete member identities instead of only the configured identity.
func (a *Agent) SupportsExpandedSessionIdentities() bool {
	if a == nil {
		return false
	}
	if m := a.EffectiveMaxActiveSessions(); m != nil && *m == 0 {
		return false
	}
	return a.SupportsInstanceExpansion() && !a.UsesCanonicalSingletonPoolIdentity()
}

// SupportsInstanceExpansion reports whether the template may have multiple
// simultaneously addressable concrete instances and therefore needs instance
// discovery / synthetic member naming.
//
// max_active_sessions=1 has two distinct flavors:
//
//   - Pool agents (MinActiveSessions or ScaleCheck set) keep pool controller
//     semantics. Non-namepool singleton pools still use the canonical
//     configured identity; see UsesCanonicalSingletonPoolIdentity.
//   - Named-session agents (MaxActiveSessions=1 with a [[named_session]]
//     entry, no Min/ScaleCheck) addressed as just "{name}" — they have a
//     stable canonical identity and a phantom "-1" suffix breaks tools that
//     resolve by qualified name.
//
// We keep instance expansion on for the pool flavor so controller paths still
// run pool reconciliation, and turn it off for the named-session flavor so the
// bare name resolves correctly.
func (a *Agent) SupportsInstanceExpansion() bool {
	if a == nil {
		return false
	}
	if strings.TrimSpace(a.Namepool) != "" || len(a.NamepoolNames) > 0 {
		return true
	}
	m := a.EffectiveMaxActiveSessions()
	if m == nil {
		return true
	}
	if *m < 0 || *m > 1 {
		return true
	}
	// *m == 1: distinguish pool agents (keep numbered instances) from
	// named-session agents (collapse to base identity). Pool agents are
	// identified by an explicit MinActiveSessions or a ScaleCheck override.
	if a.MinActiveSessions != nil || strings.TrimSpace(a.ScaleCheck) != "" {
		return true
	}
	return false
}

// HasUnlimitedSessionCapacity reports whether max_active_sessions is unbounded.
func (a *Agent) HasUnlimitedSessionCapacity() bool {
	if a == nil {
		return false
	}
	m := a.EffectiveMaxActiveSessions()
	return m == nil || *m < 0
}

// ResolvedMaxActiveSessions returns the effective max for this agent,
// inheriting from rig then workspace if not set on the agent directly.
func (a *Agent) ResolvedMaxActiveSessions(cfg *City) *int {
	if m := a.EffectiveMaxActiveSessions(); m != nil {
		return m
	}
	// Inherit from rig.
	if a.Dir != "" && cfg != nil {
		for _, rig := range cfg.Rigs {
			if rig.Name == a.Dir && rig.MaxActiveSessions != nil {
				return rig.MaxActiveSessions
			}
		}
	}
	// Inherit from workspace.
	if cfg != nil && cfg.Workspace.MaxActiveSessions != nil {
		return cfg.Workspace.MaxActiveSessions
	}
	return nil // unlimited
}
