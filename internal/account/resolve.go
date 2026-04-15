package account

import "fmt"

// Resolve selects the active account from the registry using a priority chain:
// envHandle (GC_ACCOUNT env) → flagHandle (--account flag) → configHandle
// (agent.Account config field) → registry default. The first non-empty handle
// is looked up in the registry.
//
// If all handles are empty and the registry has no default, Resolve returns
// a zero Account and nil error (graceful no-op — no account configured).
// If a non-empty handle is not found in the registry, Resolve returns an
// error naming the unknown handle.
func Resolve(reg Registry, envHandle, flagHandle, configHandle string) (Account, error) {
	// Determine the effective handle using priority order.
	handle := envHandle
	if handle == "" {
		handle = flagHandle
	}
	if handle == "" {
		handle = configHandle
	}
	if handle == "" {
		handle = reg.Default
	}

	// No handle resolved — graceful no-op.
	if handle == "" {
		return Account{}, nil
	}

	// Look up the handle in the registry.
	for _, acct := range reg.Accounts {
		if acct.Handle == handle {
			return acct, nil
		}
	}

	return Account{}, fmt.Errorf("account %q not found in registry", handle)
}
