package runtime

import (
	"context"
	"errors"
)

// ErrRuntimeIncompatible reports that a provider runtime artifact exists but
// was created for a different provider substrate than the current desired one.
// Providers use this instead of ErrSessionExists so lifecycle callers do not
// mistake liveness for compatibility.
var ErrRuntimeIncompatible = errors.New("runtime is incompatible")

// ProviderRuntimeIdentity describes the provider substrate that must match for
// a live runtime to be considered compatible. It is intentionally separate from
// ConfigFingerprint: provider substrate and agent behavior drift for different
// reasons and converge through different evidence.
type ProviderRuntimeIdentity struct {
	Fingerprint string
	Version     string
	Breakdown   string
}

// CompatibilityObservation is the optional provider-native view of a runtime
// artifact's compatibility with the currently desired substrate.
type CompatibilityObservation struct {
	Supported                 bool
	Exists                    bool
	Running                   bool
	Alive                     bool
	Compatible                bool
	Desired                   ProviderRuntimeIdentity
	Current                   ProviderRuntimeIdentity
	Reason                    string
	SafeToReplaceWithoutDrain bool
}

// ProviderRuntimeIdentityReporter is an optional capability for providers that
// can calculate the desired provider-runtime identity without touching a live
// runtime artifact.
type ProviderRuntimeIdentityReporter interface {
	DesiredProviderRuntimeIdentity(ctx context.Context, name string, cfg Config) (ProviderRuntimeIdentity, error)
}

// ProviderRuntimeCompatibilityObserver is an optional capability for providers
// that can distinguish runtime liveness from runtime compatibility.
type ProviderRuntimeCompatibilityObserver interface {
	ObserveRuntimeCompatibility(ctx context.Context, name string, cfg Config) (CompatibilityObservation, error)
}

// DesiredProviderRuntimeIdentity returns the desired identity for providers
// that opt into the capability.
func DesiredProviderRuntimeIdentity(ctx context.Context, sp Provider, name string, cfg Config) (ProviderRuntimeIdentity, bool, error) {
	if sp == nil {
		return ProviderRuntimeIdentity{}, false, nil
	}
	reporter, ok := sp.(ProviderRuntimeIdentityReporter)
	if !ok {
		return ProviderRuntimeIdentity{}, false, nil
	}
	identity, err := reporter.DesiredProviderRuntimeIdentity(ctx, name, cfg)
	return identity, true, err
}

// ObserveProviderRuntimeCompatibility returns a compatibility observation for
// providers that opt into the capability.
func ObserveProviderRuntimeCompatibility(ctx context.Context, sp Provider, name string, cfg Config) (CompatibilityObservation, bool, error) {
	if sp == nil {
		return CompatibilityObservation{}, false, nil
	}
	observer, ok := sp.(ProviderRuntimeCompatibilityObserver)
	if !ok {
		return CompatibilityObservation{}, false, nil
	}
	compat, err := observer.ObserveRuntimeCompatibility(ctx, name, cfg)
	if !compat.Supported {
		compat.Supported = true
	}
	return compat, true, err
}
