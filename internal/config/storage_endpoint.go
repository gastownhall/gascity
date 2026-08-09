package config

import (
	"errors"
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
//
// No error it returns quotes the value. A rejected `url` is credential-shaped
// by assumption — that is why most of these rules exist — and an error is the
// one thing here that reliably reaches a log.
func ValidateStorageEndpointURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		// *url.Error renders as `parse "<the whole input>": <reason>`. When the
		// parse fails for a reason that precedes the userinfo check below — an
		// invalid host byte, a control character — wrapping it would print the
		// userinfo this function exists to refuse. Keep the reason, drop the
		// value.
		var parseErr *url.Error
		if errors.As(err, &parseErr) && parseErr.Err != nil {
			return fmt.Errorf("url is not a valid URL: %w", parseErr.Err)
		}
		return errors.New("url is not a valid URL")
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
// references a storage binding's `auth` may name. The material checks run
// first so a pasted token is told what it is, rather than being told it is not
// one of two forms it was never trying to be.
//
// No error it returns quotes the value. Every value that reaches a rejection
// here is, by construction, a candidate credential: a token pasted into this
// field passes the length and separator screens only by being short and
// unpunctuated, and the caller is a config loader whose errors reach logs and
// terminals. Naming the field and enumerating the accepted forms tells an
// author everything needed to fix it; echoing the input would publish the one
// thing this field exists to keep out of the city.
func ValidateStorageAuthReference(value string) error {
	if len(value) > storageAuthMaxLength {
		return fmt.Errorf("auth is longer than %d bytes; a credential reference is a short token, not credential material",
			storageAuthMaxLength)
	}
	if strings.Contains(value, "://") || strings.ContainsFunc(value, unicode.IsSpace) {
		return errors.New("auth is a credential reference, not credential material")
	}
	if value == StorageAuthCredentialProvider {
		return nil
	}
	if name, ok := strings.CutPrefix(value, storageAuthEnvPrefix); ok {
		if !envVarName.MatchString(name) {
			return fmt.Errorf("auth %q must be followed by an environment variable name: a letter or underscore, then letters, digits, and underscores",
				storageAuthEnvPrefix)
		}
		return nil
	}
	return fmt.Errorf("auth must be %q or %q", StorageAuthCredentialProvider, storageAuthEnvPrefix+"<VARNAME>")
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
		// A deliberate coupling, and the one place this package names a second
		// built-in provider ID. It keeps `url` from being authorable on a
		// provider that would silently ignore it, at the cost of an
		// out-of-tree provider not being able to accept `url` today. When one
		// needs to, the seam is ValidateStorageBindings: the compiled bundle
		// already validates each binding there and knows which providers it
		// registered, which is the layer that can answer this without config
		// learning any out-of-tree ID.
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
