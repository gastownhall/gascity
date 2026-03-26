// Package quota tracks per-account rate-limit state and rotation logic
// for multi-account provider sessions.
package quota

import "time"

// Status represents the current quota state of an account.
type Status string

const (
	// StatusAvailable means the account is usable.
	StatusAvailable Status = "available"
	// StatusLimited means the account has hit a rate limit.
	StatusLimited Status = "limited"
	// StatusCooldown means the account was limited and is waiting to retry.
	StatusCooldown Status = "cooldown"
)

// AccountQuota holds the quota state for a single account.
type AccountQuota struct {
	// Handle is the account registry handle.
	Handle string `json:"handle"`
	// Status is the current quota status.
	Status Status `json:"status"`
	// LimitedAt is when the rate limit was detected.
	LimitedAt time.Time `json:"limited_at,omitempty"`
	// ResetsAt is the estimated time when the limit resets.
	ResetsAt time.Time `json:"resets_at,omitempty"`
	// LastUsed is when this account was last assigned to a session.
	LastUsed time.Time `json:"last_used,omitempty"`
	// UseCount is how many times this account has been assigned.
	UseCount int `json:"use_count,omitempty"`
}

// QuotaState holds the full quota state across all accounts.
type QuotaState struct {
	// Accounts is the per-account quota state.
	Accounts []AccountQuota `json:"accounts"`
	// Updated is when this state was last modified.
	Updated time.Time `json:"updated"`
}

// Get returns a pointer to the AccountQuota for the given handle, or nil.
func (s *QuotaState) Get(handle string) *AccountQuota {
	for i := range s.Accounts {
		if s.Accounts[i].Handle == handle {
			return &s.Accounts[i]
		}
	}
	return nil
}

// GetOrCreate returns the AccountQuota for handle, creating it if missing.
func (s *QuotaState) GetOrCreate(handle string) *AccountQuota {
	if aq := s.Get(handle); aq != nil {
		return aq
	}
	s.Accounts = append(s.Accounts, AccountQuota{
		Handle: handle,
		Status: StatusAvailable,
	})
	return &s.Accounts[len(s.Accounts)-1]
}
