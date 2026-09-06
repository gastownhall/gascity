package runtime

import (
	"context"
	"errors"
	"strings"
	"time"
)

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

// ObservationStatus reports whether a liveness observation completed within
// its bound and produced a trustworthy result. It is semantics-free: it says
// nothing about whether a session is running or missing, only whether the
// accompanying Liveness value should be treated as a confirmed answer.
type ObservationStatus int

const (
	// ObservationComplete means the observation finished within its bound and
	// the returned Liveness reflects a real provider answer.
	ObservationComplete ObservationStatus = iota
	// ObservationIncomplete means the observation did not finish
	// trustworthily: the provider reported an error wrapping
	// ErrRuntimeUnavailable, or the bound expired first. The accompanying
	// Liveness must not be treated as confirmed absence.
	ObservationIncomplete
)

// BoundedLivenessObserver is a richer, opt-in alternative to LivenessObserver
// for providers that can report a liveness-observation failure (for example,
// an error wrapping ErrRuntimeUnavailable) alongside the consolidated
// Liveness view. ObserveLivenessBounded prefers it when a provider implements
// it; providers that don't fall back to ObserveLiveness's existing
// plain-boolean behavior.
type BoundedLivenessObserver interface {
	ObserveLivenessWithError(name string, processNames []string) (Liveness, error)
}

// ObserveLivenessBounded is the cancellable, tri-state-aware counterpart to
// ObserveLiveness: it bounds the observation to timeout and reports whether
// the result is trustworthy via the returned ObservationStatus.
//
// It races a goroutine running the observation against context.WithTimeout,
// mirroring OrderFiringCurrentCheck.Run() (internal/doctor/checks_order_firing.go)
// but using a cancellable/composable context bound instead of a bare
// time.After. The goroutine prefers a provider's BoundedLivenessObserver
// implementation if present; otherwise it falls back to exactly today's
// ObserveLiveness behavior.
//
// A provider error wrapping ErrRuntimeUnavailable resolves to
// ObservationIncomplete with the provider's own (possibly partial) Liveness
// value preserved. A context deadline winning the race also resolves to
// ObservationIncomplete, but with a zero Liveness value, since no provider
// answer arrived at all. Providers without a BoundedLivenessObserver
// implementation always resolve to ObservationComplete with a nil error,
// exactly matching today's behavior — additive only, no existing Provider
// call site changes.
func ObserveLivenessBounded(ctx context.Context, sp Provider, name string, processNames []string, timeout time.Duration) (Liveness, ObservationStatus, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type observation struct {
		liveness Liveness
		err      error
	}
	results := make(chan observation, 1)
	go func() {
		if observer, ok := sp.(BoundedLivenessObserver); ok {
			liveness, err := observer.ObserveLivenessWithError(name, processNames)
			results <- observation{liveness: liveness, err: err}
			return
		}
		results <- observation{liveness: ObserveLiveness(sp, name, processNames)}
	}()

	select {
	case obs := <-results:
		if obs.err != nil && errors.Is(obs.err, ErrRuntimeUnavailable) {
			return obs.liveness, ObservationIncomplete, obs.err
		}
		return obs.liveness, ObservationComplete, obs.err
	case <-boundedCtx.Done():
		return Liveness{}, ObservationIncomplete, boundedCtx.Err()
	}
}
