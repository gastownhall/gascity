// Package config — quota types for per-account quota state tracking.
package config

// QuotaAccountStatus is a distinct named type for quota-specific account states.
// It is NOT the existing Status type used elsewhere in the codebase.
type QuotaAccountStatus string

const (
	// QuotaStatusAvailable indicates the account has quota remaining.
	QuotaStatusAvailable QuotaAccountStatus = "available"
	// QuotaStatusLimited indicates the account has hit a quota limit.
	QuotaStatusLimited QuotaAccountStatus = "limited"
	// QuotaStatusCooldown indicates the account is in a cooldown period.
	QuotaStatusCooldown QuotaAccountStatus = "cooldown"
)

// QuotaAccountState is the per-account quota state.
type QuotaAccountState struct {
	Status    QuotaAccountStatus `json:"status"`
	LimitedAt string             `json:"limited_at,omitempty"` // RFC3339
	ResetsAt  string             `json:"resets_at,omitempty"`  // RFC3339
	LastUsed  string             `json:"last_used,omitempty"`  // RFC3339
}

// QuotaState is the top-level quota.json structure.
type QuotaState struct {
	Accounts map[string]QuotaAccountState `json:"accounts"`
}
