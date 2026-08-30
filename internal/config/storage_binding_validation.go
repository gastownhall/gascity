package config

import "fmt"

// validateStorageBindingEndpoint validates the remote-endpoint pair on one
// binding: `url` and the reference to the credential that authenticates to it.
// This coupling stays with the full storage configuration surface; the
// hermetic endpoint validators can therefore be tested without loading the
// storage schema or provider graph.
func validateStorageBindingEndpoint(prefix string, binding StorageBindingConfig) error {
	if binding.URL == "" {
		if binding.Auth != "" {
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
