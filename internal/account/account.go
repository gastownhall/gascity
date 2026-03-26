// Package account manages a local registry of provider accounts for
// multi-account quota rotation. Each account maps a short handle to
// a CLAUDE_CONFIG_DIR path containing provider credentials.
package account

import "fmt"

// Account is a single registered provider account.
type Account struct {
	// Handle is a short unique name for this account (e.g., "work", "personal").
	Handle string `json:"handle"`
	// Description is a human-readable note about this account.
	Description string `json:"description,omitempty"`
	// ConfigDir is the absolute path to the provider config directory
	// (set as CLAUDE_CONFIG_DIR at session startup).
	ConfigDir string `json:"config_dir"`
	// Provider is the provider name this account is associated with
	// (e.g., "claude", "gemini"). Empty means any provider.
	Provider string `json:"provider,omitempty"`
}

// Registry holds the set of registered accounts and a default selection.
type Registry struct {
	// Accounts is the list of registered accounts.
	Accounts []Account `json:"accounts"`
	// Default is the handle of the default account. Empty means no default.
	Default string `json:"default,omitempty"`
}

// Resolve looks up an account by handle. Returns an error if not found.
func Resolve(reg Registry, handle string) (Account, error) {
	for _, a := range reg.Accounts {
		if a.Handle == handle {
			return a, nil
		}
	}
	return Account{}, fmt.Errorf("account %q not found in registry", handle)
}

// DefaultAccount returns the default account from the registry.
// Returns an error if no default is set or the default handle is not found.
func DefaultAccount(reg Registry) (Account, error) {
	if reg.Default == "" {
		return Account{}, fmt.Errorf("no default account set")
	}
	return Resolve(reg, reg.Default)
}

// Add appends a new account to the registry. Returns an error if the handle
// already exists.
func Add(reg *Registry, acct Account) error {
	for _, a := range reg.Accounts {
		if a.Handle == acct.Handle {
			return fmt.Errorf("account %q already exists", acct.Handle)
		}
	}
	reg.Accounts = append(reg.Accounts, acct)
	return nil
}

// Remove deletes an account by handle. Returns an error if not found.
// If the removed account was the default, the default is cleared.
func Remove(reg *Registry, handle string) error {
	for i, a := range reg.Accounts {
		if a.Handle == handle {
			reg.Accounts = append(reg.Accounts[:i], reg.Accounts[i+1:]...)
			if reg.Default == handle {
				reg.Default = ""
			}
			return nil
		}
	}
	return fmt.Errorf("account %q not found in registry", handle)
}

// SetDefault sets the default account handle. Returns an error if the handle
// is not found in the registry.
func SetDefault(reg *Registry, handle string) error {
	for _, a := range reg.Accounts {
		if a.Handle == handle {
			reg.Default = handle
			return nil
		}
	}
	return fmt.Errorf("account %q not found in registry", handle)
}
