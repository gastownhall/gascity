package runtime

import "strings"

// Liveness reports both provider-runtime presence and configured agent-process
// presence for a session target.
type Liveness struct {
	Running bool
	Alive   bool
}

// LivenessObserver is implemented by providers that can observe runtime and
// agent-process liveness in one provider-native pass.
type LivenessObserver interface {
	ObserveLiveness(name string, processNames []string) Liveness
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
