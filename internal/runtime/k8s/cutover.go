package k8s

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing SleepCapability
// through to the underlying *Provider. The early cut-over for the k8s provider.
//
// ExecProvider is not passed through: k8s's Exec (execInPod) is the connection
// the carrier drives over internally; no production caller type-asserts it.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                  = (*seamBackedProvider)(nil)
	_ runtime.LivenessObserverWithError = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider   = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider          = (*seamBackedProvider)(nil)
	_ runtime.ListingAttestation        = (*seamBackedProvider)(nil)
)

// NewSeamBacked constructs a k8s provider served through the seams.
func NewSeamBacked() (runtime.Provider, error) {
	return NewSeamBackedWithConfig(nil, nil, PodSecurityConfig{})
}

// NewSeamBackedWithSecretProjection constructs a seam-backed K8s provider with
// the city's configured Secret projections.
func NewSeamBackedWithSecretProjection(secretEnv []runtime.K8sSecretEnv, secretMounts []runtime.K8sSecretMount) (runtime.Provider, error) {
	return NewSeamBackedWithConfig(secretEnv, secretMounts, PodSecurityConfig{})
}

// NewSeamBackedWithSecurityConfig constructs a K8s provider with optional
// pod-level UID, GID, and fsGroup values from the city session config.
func NewSeamBackedWithSecurityConfig(config PodSecurityConfig) (runtime.Provider, error) {
	return NewSeamBackedWithConfig(nil, nil, config)
}

// NewSeamBackedWithConfig constructs a seam-backed K8s provider with the city's
// configured Secret projections and optional pod identity values.
func NewSeamBackedWithConfig(secretEnv []runtime.K8sSecretEnv, secretMounts []runtime.K8sSecretMount, config PodSecurityConfig) (runtime.Provider, error) {
	raw, err := NewProviderWithConfig(secretEnv, secretMounts, config)
	if err != nil {
		return nil, err
	}
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}, nil
}

// ObserveLivenessWithError preserves Kubernetes probe uncertainty across the
// production seam wrapper. The generic seam adapter only has boolean
// IsRunning/ProcessAlive methods, which would turn a transient exec failure
// into a false zombie observation.
func (s *seamBackedProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return s.raw.ObserveLivenessWithError(name, processNames)
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// Relaunch passes through to the underlying provider's warm-pod relaunch
// (respawn-pane via execInPod; B2, RelaunchProvider).
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}

// ListRunningComplete passes the underlying provider's listing attestation
// through; the seams route ListRunning to the same raw listing.
func (s *seamBackedProvider) ListRunningComplete() bool {
	return s.raw.ListRunningComplete()
}
