// Package account defines the core types for the account registry.
// An Account represents a provider account (e.g. an API key configuration
// directory) and a Registry holds the collection of known accounts plus
// a default selection.
package account

// Account represents a single provider account registration.
type Account struct {
	Handle      string `json:"handle"`
	Email       string `json:"email"`
	Description string `json:"description"`
	ConfigDir   string `json:"config_dir"`
}

// Registry holds the set of registered accounts and the handle of the
// default account.
type Registry struct {
	Default  string    `json:"default"`
	Accounts []Account `json:"accounts"`
}
