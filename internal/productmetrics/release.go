package productmetrics

import (
	"net/url"
	"runtime/debug"

	"github.com/Masterminds/semver/v3"
)

// BuildKind classifies the provenance of a Gas City binary.
type BuildKind uint8

const (
	// BuildDevelopment is the provenance of local, test, CI, and otherwise
	// untagged builds. It emits, tagged development@0.0.0-dev, so development
	// usage can be filtered out of release reporting.
	BuildDevelopment BuildKind = iota
	// BuildCanary is a pre-release artifact (release-candidate tag or rolling
	// edge / goreleaser snapshot) carrying a prerelease semver.
	BuildCanary
	// BuildRelease is a stable, clean-semver tagged release artifact.
	BuildRelease
)

// String returns the canonical build-kind name.
func (kind BuildKind) String() string {
	switch kind {
	case BuildDevelopment:
		return "development"
	case BuildCanary:
		return "canary"
	case BuildRelease:
		return "release"
	default:
		return "unknown"
	}
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

// The compiled identity core decides whether and where telemetry is sent and
// whether collection is enabled. These are const so no ordinary -ldflags -X
// can promote a build. This is the activation point: the endpoint is the live
// gc command-usage ingest, collection is default-on behind the first-run
// notice, and the metrics epoch is the first privacy generation.
const (
	compiledEndpoint     = "https://gastownhall-eventsapi.com/v1/gascity/command-events"
	compiledPrivacyURL   = ""
	compiledMetricsEpoch = uint64(1)
	compiledRollout      = RolloutDefaultOn
)

// compiledReleaseTag is the ONLY linker-injectable identity input, set solely
// by the release build (.goreleaser.yml -X). It is a semver-shaped label used
// to classify the artifact (development / canary / release) for reporting. It
// cannot redirect telemetry, force-enable collection, or bypass consent — all
// of that is governed by the const core above and the compiled notice. Empty
// (plain `go build`, `make`, `go test`, `go install`) is a development build.
var compiledReleaseTag string

// developmentReleaseVersion is the fixed canonical semver reported by every
// non-release build. A single fixed value keeps one signed pause able to cover
// all development builds and passes strict-semver validation on both the
// client and the ingest tee.
const developmentReleaseVersion = "0.0.0-dev"

// CurrentReleaseIdentity returns the identity compiled into this artifact. The
// build kind and reported version are derived from the injected release tag;
// the rest of the identity is compiled and runtime-unoverrideable.
func CurrentReleaseIdentity() ReleaseIdentity {
	kind, version := classifyBuild(compiledReleaseTag, buildIsDirty())
	return ReleaseIdentity{
		buildKind:      kind,
		releaseVersion: version,
		endpoint:       compiledEndpoint,
		privacyURL:     compiledPrivacyURL,
		metricsEpoch:   compiledMetricsEpoch,
		rollout:        compiledRollout,
	}
}

// classifyBuild derives the build kind and reported semver from the injected
// release tag. A clean, canonical semver tag with no prerelease is a stable
// release; a canonical semver tag with a prerelease segment (release-candidate
// or goreleaser snapshot) is a canary; anything else — including an empty tag
// or a dirty working tree — is a development build.
func classifyBuild(tag string, dirty bool) (BuildKind, string) {
	if tag != "" && !dirty {
		if version, err := semver.StrictNewVersion(tag); err == nil && version.String() == tag {
			if version.Prerelease() == "" {
				return BuildRelease, tag
			}
			return BuildCanary, tag
		}
	}
	return BuildDevelopment, developmentReleaseVersion
}

// buildIsDirty reports whether the artifact was built from a modified working
// tree. Only an explicit vcs.modified=true stamp counts as dirty; absent VCS
// metadata (e.g. -buildvcs=false) is treated as clean so release artifacts
// classify correctly. A dirty tree can never classify as release or canary.
func buildIsDirty() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.modified" {
			return setting.Value == "true"
		}
	}
	return false
}

// BuildKind returns the artifact's build provenance.
func (identity ReleaseIdentity) BuildKind() BuildKind { return identity.buildKind }

// ReleaseVersion returns the reported semver: the release/canary tag for tagged
// builds, or the fixed development version for untagged builds.
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
