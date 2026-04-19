package worker

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/events"
)

// NewSessionHandle constructs a session-backed worker handle.
func NewSessionHandle(cfg SessionHandleConfig) (*SessionHandle, error) {
	if cfg.Manager == nil {
		return nil, fmt.Errorf("%w: manager is required", ErrHandleConfig)
	}

	spec := cloneSessionSpec(cfg.Session)
	if spec.Provider == "" {
		spec.Provider = profileFamily(spec.Profile)
	}
	if spec.Command == "" {
		spec.Command = spec.Provider
	}
	if spec.Title == "" {
		spec.Title = spec.Template
	}
	if spec.Metadata == nil {
		spec.Metadata = map[string]string{}
	} else {
		spec.Metadata = cloneStringMap(spec.Metadata)
	}
	if strings.TrimSpace(spec.Metadata["session_origin"]) == "" {
		spec.Metadata["session_origin"] = "worker"
	}
	if spec.Profile != "" && strings.TrimSpace(spec.Metadata["worker_profile"]) == "" {
		spec.Metadata["worker_profile"] = string(spec.Profile)
	}
	applyCanonicalProfileIdentityMetadata(spec.Profile, spec.Metadata)
	if spec.ID == "" {
		switch {
		case strings.TrimSpace(spec.Template) == "":
			return nil, fmt.Errorf("%w: template is required", ErrHandleConfig)
		case strings.TrimSpace(spec.WorkDir) == "":
			return nil, fmt.Errorf("%w: work_dir is required", ErrHandleConfig)
		case strings.TrimSpace(spec.Provider) == "":
			return nil, fmt.Errorf("%w: provider is required", ErrHandleConfig)
		}
	}

	adapter := cfg.Adapter
	searchPaths := append([]string(nil), cfg.SearchPaths...)
	if len(adapter.SearchPaths) == 0 {
		adapter.SearchPaths = append([]string(nil), searchPaths...)
	}
	recorder := cfg.Recorder
	if recorder == nil {
		recorder = events.Discard
	}

	return &SessionHandle{
		manager:     cfg.Manager,
		adapter:     adapter,
		recorder:    recorder,
		searchPaths: searchPaths,
		session:     spec,
		sessionID:   strings.TrimSpace(spec.ID),
	}, nil
}

func applyCanonicalProfileIdentityMetadata(profile Profile, metadata map[string]string) {
	if metadata == nil {
		return
	}
	identity, ok := CanonicalProfileIdentity(profile)
	if !ok {
		return
	}
	setIfEmpty(metadata, "worker_profile_provider_family", identity.ProviderFamily)
	setIfEmpty(metadata, "worker_profile_transport_class", identity.TransportClass)
	setIfEmpty(metadata, "worker_profile_behavior_version", identity.BehaviorClaimsVersion)
	setIfEmpty(metadata, "worker_profile_transcript_adapter_version", identity.TranscriptAdapterVersion)
	setIfEmpty(metadata, "worker_profile_compatibility_version", identity.CompatibilityVersion)
	setIfEmpty(metadata, "worker_profile_certification_fingerprint", identity.CertificationFingerprint)
}

func setIfEmpty(metadata map[string]string, key, value string) {
	if strings.TrimSpace(metadata[key]) == "" && strings.TrimSpace(value) != "" {
		metadata[key] = value
	}
}
