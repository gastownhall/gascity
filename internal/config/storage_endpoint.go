package config

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

const (
	// StorageAuthCredentialProvider is the `auth` reference that delegates
	// credential minting to the configured credential-provider command (the
	// `gasworks credential-provider` default, or whatever GC_CREDENTIAL_PROVIDER
	// names). It is spelled after that command, not after any hosted service:
	// the reference selects the mechanism, and the mechanism resolves whatever
	// endpoint the operator configured.
	StorageAuthCredentialProvider = "gasworks"

	// storageAuthEnvPrefix introduces the environment-variable form of `auth`.
	storageAuthEnvPrefix = "env:"

	// storageAuthMaxLength bounds a credential *reference*. Every legal
	// reference is a short token, so a longer value is a credential wearing the
	// field's name — and keeping credentials out of city.toml is the entire
	// reason this field is a reference.
	storageAuthMaxLength = 64
)

// ValidateStorageEndpointURL enforces the shape of a storage binding's `url`:
// a location and nothing else. A path prefix is allowed because an edge may
// mount the service below the root; userinfo, a query, and a fragment are
// refused because those are the parts of a URL a credential rides in on.
//
// It is deliberately not the identifier rule the other storage fields use, and
// it is exported so the plan envelope in internal/storebinding applies exactly
// this rule rather than a second copy that can drift from it.
func ValidateStorageEndpointURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("url is not a valid URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("url scheme must be http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("url has no host")
	}
	if parsed.User != nil {
		return fmt.Errorf("url must not embed credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("url must not carry a query")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("url must not carry a fragment")
	}
	return nil
}

// ValidateStorageAuthReference enforces the closed set of credential
// references a storage binding's `auth` may name. The material check runs
// first so a pasted token is told what it is, rather than being told it is not
// one of two forms it was never trying to be.
func ValidateStorageAuthReference(value string) error {
	if len(value) > storageAuthMaxLength ||
		strings.Contains(value, "://") ||
		strings.ContainsFunc(value, unicode.IsSpace) {
		return fmt.Errorf("auth is a credential reference, not credential material")
	}
	if value == StorageAuthCredentialProvider {
		return nil
	}
	if name, ok := strings.CutPrefix(value, storageAuthEnvPrefix); ok {
		if !envVarName.MatchString(name) {
			return fmt.Errorf("auth %q does not name an environment variable", value)
		}
		return nil
	}
	return fmt.Errorf("auth must be %q or \"env:<VARNAME>\", got %q", StorageAuthCredentialProvider, value)
}

// validateStorageBindingEndpoint validates the remote-endpoint pair on one
// binding: `url` and the reference to the credential that authenticates to it.
//
// Both fields are inert in this build — nothing here opens the endpoint. They
// are typed and validated anyway because the authoring surface rejects every
// undecoded [storage] key, so an untyped field could not be authored at all,
// and because a malformed or secret-bearing value is worth refusing at load
// rather than at first use.
func validateStorageBindingEndpoint(prefix string, binding StorageBindingConfig) error {
	if binding.URL == "" {
		if binding.Auth != "" {
			// A credential with nothing to authenticate to is either a
			// half-finished edit or a credential parked in config.
			return fmt.Errorf("%s: auth requires url", prefix)
		}
		return nil
	}
	if binding.Provider != StorageProviderBeadsWorkspace {
		return fmt.Errorf("%s: url is only supported by provider %q", prefix, StorageProviderBeadsWorkspace)
	}
	if err := ValidateStorageEndpointURL(binding.URL); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if binding.ConfigRef == "" {
		// The workspace reference stays the on-disk anchor even when the
		// backend is remote: url says where the backend answers, config_ref
		// says which workspace in this city is asking.
		return fmt.Errorf("%s: url requires config_ref", prefix)
	}
	if binding.Auth == "" {
		return nil
	}
	if err := ValidateStorageAuthReference(binding.Auth); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}
