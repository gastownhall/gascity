package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// providerOwnedFanOutBudget bounds how long one provider-owned health call
// keeps starting scope-level work. Every probe and recover is a provider
// process with its own timeout, so a city with K scopes and a hung provider
// would otherwise spend K times a scope's worth of timeouts in one call. The
// budget gates the start of a probe or a recover, never an op that has begun:
// a started recover always gets its confirming probe.
const providerOwnedFanOutBudget = 120 * time.Second

// lastProviderScopeRecover records, per provider-owned scope root, when the
// most recent recover started. A scope recovered within providerRecoverCooldown
// is not recovered again, so one wedged rig cannot be cycled on every tick.
var lastProviderScopeRecover sync.Map // normalized scope root → time.Time

// providerOwnedFanOutCursor records, per city, the first scope a budget-limited
// health sweep did not reach. The next sweep starts there, so a scope late in
// the order is not starved by the ones before it. A sweep that reaches every
// scope clears it.
var providerOwnedFanOutCursor sync.Map // normalized city path → scope root

// providerFanOut is the budget and bookkeeping of one health call. It travels
// in the context so the probe sweep and the recover pass share one clock. The
// clock starts when the first provider-owned sweep begins, so a call that never
// reaches one never reads it.
// A nil *providerFanOut means "unbudgeted" and every method tolerates it.
type providerFanOut struct {
	cityKey    string
	start      time.Time
	incomplete bool
}

type providerFanOutKey struct{}

// withProviderFanOut starts the budget for one health call over cityPath.
func withProviderFanOut(ctx context.Context, cityPath string) context.Context {
	return context.WithValue(ctx, providerFanOutKey{}, &providerFanOut{cityKey: normalizePathForCompare(cityPath)})
}

// providerFanOutFor returns the budget for a health op, or nil for any other op
// or a call that has no budget.
func providerFanOutFor(ctx context.Context, op string) *providerFanOut {
	if op != "health" {
		return nil
	}
	fan, _ := ctx.Value(providerFanOutKey{}).(*providerFanOut)
	return fan
}

// exhausted reports whether the call has used its budget.
func (f *providerFanOut) exhausted() bool {
	return f != nil && !f.start.IsZero() && providerRecoverNow().Sub(f.start) >= providerOwnedFanOutBudget
}

// rotate starts the budget clock on the first sweep of the call and returns
// scopes starting at the cursor's scope, or unchanged when the city has no
// cursor or the cursor's scope is gone.
func (f *providerFanOut) rotate(scopes []string) []string {
	if f == nil {
		return scopes
	}
	if f.start.IsZero() {
		f.start = providerRecoverNow()
	}
	v, ok := providerOwnedFanOutCursor.Load(f.cityKey)
	if !ok {
		return scopes
	}
	at, _ := v.(string)
	for i, scopeRoot := range scopes {
		if normalizePathForCompare(scopeRoot) == at {
			return append(append([]string{}, scopes[i:]...), scopes[:i]...)
		}
	}
	return scopes
}

// recordSweep stores where the next sweep resumes and returns the error naming
// the scopes this sweep did not probe. A sweep that probed everything clears
// the cursor and returns nil.
func (f *providerFanOut) recordSweep(notProbed []string) error {
	if f == nil {
		return nil
	}
	if len(notProbed) == 0 {
		providerOwnedFanOutCursor.Delete(f.cityKey)
		return nil
	}
	f.incomplete = true
	providerOwnedFanOutCursor.Store(f.cityKey, normalizePathForCompare(notProbed[0]))
	return fmt.Errorf("provider-owned health sweep stopped at its %s budget; not probed: %s",
		providerOwnedFanOutBudget, strings.Join(notProbed, ", "))
}

// incompleteErr is non-nil when the sweep left scopes unprobed, so a caller
// that recovered every scope it did probe still does not report the city
// healthy.
func (f *providerFanOut) incompleteErr() error {
	if f == nil || !f.incomplete {
		return nil
	}
	return errors.New("provider-owned health sweep incomplete: some scopes were not probed")
}

// scopeVisit is what one scope contributed to a provider-owned fan-out.
type scopeVisit int

const (
	scopeVisited  scopeVisit = iota // the op ran (or failed to start)
	scopeSkipped                    // not provider-owned, or idle-retired for health
	scopeDeferred                   // the budget ran out before the op could start
)

// visitProviderOwnedScope runs op against one scope of a fan-out.
func visitProviderOwnedScope(parent context.Context, cityPath, scopeRoot, op string, fan *providerFanOut) (scopeVisit, error) {
	owned, err := scopeProviderOwned(cityPath, scopeRoot)
	if err != nil {
		return scopeVisited, err
	}
	if !owned {
		return scopeSkipped, nil
	}
	if op == "health" && beadsScopeIdleRetired(scopeRoot) {
		// The proxy retired on its idle timeout. That is healthy, and a
		// ping would only restart it for another idle period.
		return scopeSkipped, nil
	}
	if fan.exhausted() {
		return scopeDeferred, nil
	}
	return scopeVisited, runProviderOwnedScopeLifecycleOpContext(parent, cityPath, scopeRoot, op)
}

// providerScopeRecoverSkipReason says why scopeRoot must not be recovered now,
// or "" when it may be. started counts the recovers this call already began:
// the first one is exempt from the budget so a call that finds an unhealthy
// scope always makes progress on it.
func providerScopeRecoverSkipReason(scopeRoot string, started int, fan *providerFanOut) string {
	if v, ok := lastProviderScopeRecover.Load(normalizePathForCompare(scopeRoot)); ok {
		if last, _ := v.(time.Time); providerRecoverNow().Sub(last) < providerRecoverCooldown() {
			return "cooldown"
		}
	}
	if started > 0 && fan.exhausted() {
		return "fan-out budget"
	}
	return ""
}

// startProviderOwnedRecovers runs recover on each unhealthy scope that may be
// recovered now. It returns the scopes it started and an error naming every
// scope it skipped or that failed to recover.
func startProviderOwnedRecovers(ctx context.Context, cityPath string, unhealthy []string, fan *providerFanOut) (started []string, skips, failures []error) {
	for _, scopeRoot := range unhealthy {
		if reason := providerScopeRecoverSkipReason(scopeRoot, len(started), fan); reason != "" {
			skips = append(skips, fmt.Errorf("provider-owned scope %q recovery skipped (%s)", scopeRoot, reason))
			continue
		}
		lastProviderScopeRecover.Store(normalizePathForCompare(scopeRoot), providerRecoverNow())
		started = append(started, scopeRoot)
		if err := runProviderOwnedScopeLifecycleOpContext(ctx, cityPath, scopeRoot, "recover"); err != nil {
			failures = append(failures, fmt.Errorf("provider-owned scope %q recover: %w", scopeRoot, err))
		}
	}
	return started, skips, failures
}
