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
// so it re-applies the authoring surface's rules rather than trusting that
// every specification came from a parsed city.toml. It applies the rules
// themselves, from the package that owns them, so the two gates cannot drift.
func validateEndpoint(endpoint, auth string) error {
	if endpoint == "" {
		if auth != "" {
			return fmt.Errorf("auth requires url")
		}
		return nil
	}
	// Shape first, then the shared secret scan. Both run; ordering them this
	// way means a URL that carries userinfo or a query — the shapes the rule
	// names — is refused with the reason it was refused, and the scan stays
	// the backstop for a credential hiding somewhere the shape rule allows,
	// such as a token pasted into the path prefix.
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
