package storebinding

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
)

// AuthCredentialProvider is the BindingSpec.Auth reference that delegates
// credential minting to the configured credential-provider command. It is the
// authoring surface's constant, not a second spelling of it: one token, one
// definition, so a plan and the city.toml it came from cannot disagree about
// what the reference is.
const AuthCredentialProvider = config.StorageAuthCredentialProvider

// validateEndpoint validates the remote-endpoint pair on a binding
// specification: where a non-local backing store answers, and the reference to
// the credential that authenticates to it.
//
// This is the last gate before a provider is constructed from a specification,
// and it is COMPLEMENTARY to the authoring gate in internal/config, not a
// mirror of it. The two share the value-shape rules — this calls them, so a
// shape accepted in city.toml is the shape accepted here — but each also
// enforces what only it can:
//
//   - only the authoring gate knows the binding's provider and config_ref, so
//     "url is only supported by beads-workspace" and "url requires config_ref"
//     live there and are not repeated here;
//   - only this gate runs validateSecretFree, the deep credential scan the
//     whole plan envelope shares.
//
// The consequence is worth stating plainly: a URL whose PATH PREFIX carries
// credential-shaped material — `https://beads.example/token=abc123` — passes
// city.toml load, because the shape rule allows a path prefix, and is refused
// here at plan resolution. Config is not the only gate, and neither is this
// one.
func validateEndpoint(endpoint, auth string) error {
	if endpoint == "" {
		if auth != "" {
			return fmt.Errorf("auth requires url")
		}
		return nil
	}
	// Shape first, then the shared secret scan. Both run; ordering them this
	// way means a URL that carries userinfo or a query — the shapes the rule
	// names — is refused for that reason rather than as generic "secret
	// material". The scan then catches what the shape rule permits: it reads
	// the path for credential ASSIGNMENTS (`token=…`, `password=…`), decodes
	// nested URL and JSON encodings, and spots PEM private keys. A bare
	// high-entropy path segment is not credential-shaped to it and stays
	// legal — a path prefix is a legitimate mount point.
	if err := config.ValidateStorageEndpointURL(endpoint); err != nil {
		return err
	}
	if err := validateSecretFree("binding url", endpoint); err != nil {
		return err
	}
	if auth == "" {
		return nil
	}
	if err := config.ValidateStorageAuthReference(auth); err != nil {
		return err
	}
	return validateSecretFree("binding auth", auth)
}
