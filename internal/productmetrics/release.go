package productmetrics

import "net/url"

// BuildKind classifies the provenance of a Gas City binary.
type BuildKind uint8

const (
	// BuildDevelopment is the fail-closed identity of local, test, CI, and
	// otherwise unversioned builds.
	BuildDevelopment BuildKind = iota
)

// String returns the canonical build-kind name.
func (kind BuildKind) String() string {
	if kind == BuildDevelopment {
		return "development"
	}
	return "unknown"
}

// RolloutMode is the closed product-metrics release rollout domain.
type RolloutMode uint8

const (
	// RolloutDefaultOff disables collection for the compiled artifact.
	RolloutDefaultOff RolloutMode = iota
	// RolloutCanary limits collection to an approved canary artifact.
	RolloutCanary
	// RolloutDefaultOn enables the approved first-run notice flow.
	RolloutDefaultOn
)

// String returns the canonical rollout-mode name.
func (mode RolloutMode) String() string {
	switch mode {
	case RolloutDefaultOff:
		return "default-off"
	case RolloutCanary:
		return "canary"
	case RolloutDefaultOn:
		return "default-on"
	default:
		return "unknown"
	}
}

// ReleaseIdentity is the runtime-unoverrideable product-metrics identity
// compiled into an artifact. Its fields are intentionally private so runtime
// callers cannot construct a promoted identity.
type ReleaseIdentity struct {
	buildKind      BuildKind
	releaseVersion string
	endpoint       string
	privacyURL     string
	metricsEpoch   uint64
	rollout        RolloutMode
}

const (
	compiledBuildKind      = BuildDevelopment
	compiledReleaseVersion = ""
	compiledEndpoint       = ""
	compiledPrivacyURL     = ""
	compiledMetricsEpoch   = uint64(0)
	compiledRollout        = RolloutDefaultOff
)

// CurrentReleaseIdentity returns the immutable identity compiled into this
// artifact. Source builds are always inert.
func CurrentReleaseIdentity() ReleaseIdentity {
	return ReleaseIdentity{
		buildKind:      compiledBuildKind,
		releaseVersion: compiledReleaseVersion,
		endpoint:       compiledEndpoint,
		privacyURL:     compiledPrivacyURL,
		metricsEpoch:   compiledMetricsEpoch,
		rollout:        compiledRollout,
	}
}

// BuildKind returns the artifact's build provenance.
func (identity ReleaseIdentity) BuildKind() BuildKind { return identity.buildKind }

// ReleaseVersion returns the official semver, or empty for a development build.
func (identity ReleaseIdentity) ReleaseVersion() string { return identity.releaseVersion }

// Endpoint returns the compiled ingest endpoint, or empty for an inert build.
func (identity ReleaseIdentity) Endpoint() string { return identity.endpoint }

// PrivacyURL returns the compiled privacy-policy URL, or empty for an inert
// artifact without approved production notice material.
func (identity ReleaseIdentity) PrivacyURL() string { return identity.privacyURL }

// MetricsEpoch returns the compiled privacy-generation epoch.
func (identity ReleaseIdentity) MetricsEpoch() uint64 { return identity.metricsEpoch }

// Rollout returns the compiled rollout mode.
func (identity ReleaseIdentity) Rollout() RolloutMode { return identity.rollout }

func endpointHostnameForPolicy(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		return ""
	}
	return parsed.Hostname()
}
