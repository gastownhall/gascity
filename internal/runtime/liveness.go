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

// LivenessObserverWithError is the optional provider capability for liveness
// observations that can distinguish confirmed absence from an observation
// failure. Legacy providers keep using [LivenessObserver] or the
// [Provider.IsRunning] and [Provider.ProcessAlive] fallback.
type LivenessObserverWithError interface {
	ObserveLivenessWithError(name string, processNames []string) (Liveness, error)
}

// ObserveLivenessWithError returns an error-bearing consolidated liveness view.
// Providers that do not expose the optional error-bearing capability retain the
// legacy observation behavior and return a nil error.
func ObserveLivenessWithError(sp Provider, name string, processNames []string) (Liveness, error) {
	if sp == nil || strings.TrimSpace(name) == "" {
		return Liveness{}, nil
	}
	if observer, ok := sp.(LivenessObserverWithError); ok {
		obs, err := observer.ObserveLivenessWithError(name, processNames)
		return normalizeLiveness(obs), err
	}
	return ObserveLiveness(sp, name, processNames), nil
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
