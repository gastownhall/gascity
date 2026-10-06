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

// BoundedLivenessObserver is the provider capability ObserveLivenessBounded
// prefers: a richer, opt-in alternative to LivenessObserver for providers that
// can report a liveness-observation failure (for example, an error wrapping
// ErrRuntimeUnavailable) alongside the consolidated Liveness view. It is an
// alias for [LivenessObserverWithError]: one capability, one method set, two
// readable names. Providers that implement neither fall back to
// ObserveLiveness's existing plain-boolean behavior.
type BoundedLivenessObserver = LivenessObserverWithError

// ObserveLivenessBounded is the cancellable, tri-state-aware counterpart to
// ObserveLiveness: it bounds the observation to timeout and reports whether
// the result is trustworthy via the returned ObservationStatus.
//
// It races a goroutine running the observation against context.WithTimeout,
// mirroring OrderFiringCurrentCheck.Run() (internal/doctor/checks_order_firing.go)
// but using a cancellable/composable context bound instead of a bare
// time.After. The goroutine delegates to [ObserveLivenessWithError], so the
// bounded path inherits exactly one copy of the shared behavior: nil-provider
// and blank-name guarding, the BoundedLivenessObserver preference, liveness
// normalization, and the plain-boolean fallback for providers that implement
// neither observer capability.
//
// A provider error wrapping ErrRuntimeUnavailable resolves to
// ObservationIncomplete with the provider's own (possibly partial) Liveness
// value preserved. A context deadline winning the race also resolves to
// ObservationIncomplete, but with a zero Liveness value, since no provider
// answer arrived at all. An already-canceled parent context resolves the same
// way without spawning an observation at all. A provider result that arrives
// is always honored, even if the bound expired concurrently: discarding a real
// answer to report "incomplete" would fail in the wrong direction for a
// primitive whose job is to avoid fabricating confirmed absence. Providers
// without a BoundedLivenessObserver implementation always resolve to
// ObservationComplete with a nil error, exactly matching today's behavior —
// additive only, no existing Provider call site changes.
func ObserveLivenessBounded(ctx context.Context, sp Provider, name string, processNames []string, timeout time.Duration) (Liveness, ObservationStatus, error) {
	if err := ctx.Err(); err != nil {
		return Liveness{}, ObservationIncomplete, err
	}

	boundedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type observation struct {
		liveness Liveness
		err      error
	}
	results := make(chan observation, 1)
	go func() {
		liveness, err := ObserveLivenessWithError(sp, name, processNames)
		results <- observation{liveness: liveness, err: err}
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
