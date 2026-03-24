package config

import (
	"strings"
	"time"
)

// SessionSleepClass identifies the policy bucket used for idle session sleep.
type SessionSleepClass string

const (
	SessionSleepInteractiveResume SessionSleepClass = "interactive_resume"
	SessionSleepInteractiveFresh  SessionSleepClass = "interactive_fresh"
	SessionSleepNonInteractive    SessionSleepClass = "noninteractive"

	// SessionSleepOff disables idle sleep while preserving inheritance
	// semantics for empty/unset values.
	SessionSleepOff = "off"

	// SessionSleepSourceAgent means the agent explicitly set sleep_after_idle.
	SessionSleepSourceAgent = "agent"
	// SessionSleepSourceRigOverride means a rig override stamped the value.
	SessionSleepSourceRigOverride = "rig_override"
	// SessionSleepSourceAgentPatch means a post-merge agent patch stamped it.
	SessionSleepSourceAgentPatch = "agent_patch"
	// SessionSleepSourceRigDefault means the value was inherited from the rig.
	SessionSleepSourceRigDefault = "rig_default"
	// SessionSleepSourceWorkspaceDefault means the value came from workspace defaults.
	SessionSleepSourceWorkspaceDefault = "workspace_default"
	// SessionSleepSourceLegacyOff means no policy was configured, so legacy behavior applies.
	SessionSleepSourceLegacyOff = "legacy_off"
)

// ResolvedSessionSleepPolicy is the class-resolved raw policy before runtime
// capability filtering.
type ResolvedSessionSleepPolicy struct {
	Class  SessionSleepClass
	Value  string
	Source string
}

// ValueForClass returns the configured value for a session class.
func (c SessionSleepConfig) ValueForClass(class SessionSleepClass) string {
	switch class {
	case SessionSleepInteractiveResume:
		return c.InteractiveResume
	case SessionSleepInteractiveFresh:
		return c.InteractiveFresh
	case SessionSleepNonInteractive:
		return c.NonInteractive
	default:
		return ""
	}
}

// NormalizeSleepAfterIdle trims whitespace and canonicalizes "off".
func NormalizeSleepAfterIdle(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.EqualFold(trimmed, SessionSleepOff) {
		return SessionSleepOff
	}
	return trimmed
}

// SleepAfterIdleDisabled reports whether the raw config disables idle sleep.
func SleepAfterIdleDisabled(raw string) bool {
	return NormalizeSleepAfterIdle(raw) == SessionSleepOff
}

// ParseSleepAfterIdle parses a duration-or-"off" config value.
// Empty values are treated as unset and return (0, false, nil).
func ParseSleepAfterIdle(raw string) (time.Duration, bool, error) {
	normalized := NormalizeSleepAfterIdle(raw)
	switch normalized {
	case "":
		return 0, false, nil
	case SessionSleepOff:
		return 0, true, nil
	default:
		d, err := time.ParseDuration(normalized)
		if err != nil {
			return 0, false, err
		}
		return d, false, nil
	}
}

// NormalizeSessionSleepFields canonicalizes parsed duration-or-"off" values and
// records explicit agent-level provenance when not already set.
func NormalizeSessionSleepFields(cfg *City) {
	if cfg == nil {
		return
	}
	cfg.SessionSleep.InteractiveResume = NormalizeSleepAfterIdle(cfg.SessionSleep.InteractiveResume)
	cfg.SessionSleep.InteractiveFresh = NormalizeSleepAfterIdle(cfg.SessionSleep.InteractiveFresh)
	cfg.SessionSleep.NonInteractive = NormalizeSleepAfterIdle(cfg.SessionSleep.NonInteractive)
	for i := range cfg.Rigs {
		cfg.Rigs[i].SessionSleep.InteractiveResume = NormalizeSleepAfterIdle(cfg.Rigs[i].SessionSleep.InteractiveResume)
		cfg.Rigs[i].SessionSleep.InteractiveFresh = NormalizeSleepAfterIdle(cfg.Rigs[i].SessionSleep.InteractiveFresh)
		cfg.Rigs[i].SessionSleep.NonInteractive = NormalizeSleepAfterIdle(cfg.Rigs[i].SessionSleep.NonInteractive)
	}
	for i := range cfg.Agents {
		cfg.Agents[i].SleepAfterIdle = NormalizeSleepAfterIdle(cfg.Agents[i].SleepAfterIdle)
		if cfg.Agents[i].SleepAfterIdle != "" && cfg.Agents[i].SleepAfterIdleSource == "" {
			cfg.Agents[i].SleepAfterIdleSource = SessionSleepSourceAgent
		}
	}
}

// ResolveSessionSleepPolicy returns the raw idle-sleep policy selected for the
// agent after class-based inheritance. Runtime capability filtering happens
// later in the reconciler.
func ResolveSessionSleepPolicy(cfg *City, agent *Agent) ResolvedSessionSleepPolicy {
	class := ClassifySessionSleepAgent(agent)
	if agent == nil {
		return ResolvedSessionSleepPolicy{
			Class:  class,
			Value:  SessionSleepOff,
			Source: SessionSleepSourceLegacyOff,
		}
	}
	if agent.SleepAfterIdle != "" {
		source := agent.SleepAfterIdleSource
		if source == "" {
			source = SessionSleepSourceAgent
		}
		return ResolvedSessionSleepPolicy{
			Class:  class,
			Value:  agent.SleepAfterIdle,
			Source: source,
		}
	}
	if rig := findSessionSleepRig(cfg, agent); rig != nil {
		if value := rig.SessionSleep.ValueForClass(class); value != "" {
			return ResolvedSessionSleepPolicy{
				Class:  class,
				Value:  value,
				Source: SessionSleepSourceRigDefault,
			}
		}
	}
	if cfg != nil {
		if value := cfg.SessionSleep.ValueForClass(class); value != "" {
			return ResolvedSessionSleepPolicy{
				Class:  class,
				Value:  value,
				Source: SessionSleepSourceWorkspaceDefault,
			}
		}
	}
	return ResolvedSessionSleepPolicy{
		Class:  class,
		Value:  SessionSleepOff,
		Source: SessionSleepSourceLegacyOff,
	}
}

// ClassifySessionSleepAgent determines the session-sleep policy class for the
// configured agent.
func ClassifySessionSleepAgent(agent *Agent) SessionSleepClass {
	if agent == nil || !agent.AttachEnabled() {
		return SessionSleepNonInteractive
	}
	if agent.EffectiveWakeMode() == "fresh" {
		return SessionSleepInteractiveFresh
	}
	return SessionSleepInteractiveResume
}

func findSessionSleepRig(cfg *City, agent *Agent) *Rig {
	if cfg == nil || agent == nil || agent.Dir == "" {
		return nil
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == agent.Dir {
			return &cfg.Rigs[i]
		}
	}
	return nil
}
