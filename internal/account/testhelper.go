package account

import "testing"

// TestRegistry creates a Registry pre-populated with the given accounts.
// The first account's Handle is set as the default. If no accounts are
// provided, the returned Registry has an empty Default and nil Accounts.
// This is a test helper — it calls t.Helper() so failures report the
// caller's location.
func TestRegistry(t testing.TB, accounts ...Account) Registry {
	t.Helper()
	reg := Registry{
		Accounts: accounts,
	}
	if len(accounts) > 0 {
		reg.Default = accounts[0].Handle
	}
	return reg
}
