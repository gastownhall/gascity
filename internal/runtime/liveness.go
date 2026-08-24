package runtime

import (
	"strings"
	"time"
)

// Liveness reports both provider-runtime presence and configured agent-process
// presence for a session target.
type Liveness struct {
	Running bool
	Alive   bool
	// Complete reports whether this is a fresh, authoritative absence/presence
	// observation. Callers that may destroy durable state must not infer death
	// from an incomplete observation.
	Complete bool
}

// LivenessTarget identifies the runtime and durable session whose liveness is
// being observed.
type LivenessTarget struct {
	SessionID    string
	SessionName  string
	ProcessNames []string
	// IncarnationStartedAt bounds inaccessible-process uncertainty. A provider
	// may ignore a process only when it can prove the process predates this
	// session incarnation. Zero keeps the unbounded, fail-closed behavior.
	IncarnationStartedAt time.Time
}

// LivenessObserver is implemented by providers that can observe runtime and
// agent-process liveness in one provider-native pass.
type LivenessObserver interface {
	ObserveLiveness(name string, processNames []string) Liveness
}

// FreshLivenessObserver is implemented by providers that can prove a new,
// complete liveness observation for an exact session target.
type FreshLivenessObserver interface {
	ObserveFreshLiveness(LivenessTarget) Liveness
}

// LivenessInvalidator is implemented by providers that cache runtime
// liveness. It discards cached liveness before a caller needs a fresh
// observation for one session.
type LivenessInvalidator interface {
	InvalidateLiveness(name string)
}

// ObserveLiveness returns the consolidated liveness view for a provider
// session. Providers with native support may use additional persisted runtime
// hints; other providers fall back to IsRunning plus ProcessAlive.
func ObserveLiveness(sp Provider, name string, processNames []string) Liveness {
	if sp == nil || strings.TrimSpace(name) == "" {
		return Liveness{}
	}
	if observer, ok := sp.(LivenessObserver); ok {
		return normalizeLiveness(observer.ObserveLiveness(name, processNames))
	}
	running := sp.IsRunning(name)
	if !hasProcessNameHints(processNames) {
		return Liveness{Running: running, Alive: running}
	}
	alive := sp.ProcessAlive(name, processNames)
	if alive && !running {
		running = true
	}
	return normalizeLiveness(Liveness{Running: running, Alive: alive})
}

// ObserveFreshLiveness returns a fresh liveness observation when the provider
// supports one. Providers without that capability retain any positive
// liveness evidence, but their absence is deliberately incomplete.
func ObserveFreshLiveness(sp Provider, target LivenessTarget) Liveness {
	target.SessionName = strings.TrimSpace(target.SessionName)
	if sp == nil || target.SessionName == "" {
		return Liveness{}
	}
	if observer, ok := sp.(FreshLivenessObserver); ok {
		return normalizeLiveness(observer.ObserveFreshLiveness(target))
	}
	observation := normalizeLiveness(ObserveLiveness(sp, target.SessionName, target.ProcessNames))
	observation.Complete = false
	return observation
}

func hasProcessNameHints(processNames []string) bool {
	for _, name := range processNames {
		if strings.TrimSpace(name) != "" {
			return true
		}
	}
	return false
}

func normalizeLiveness(obs Liveness) Liveness {
	if obs.Alive && !obs.Running {
		obs.Running = true
	}
	return obs
}
