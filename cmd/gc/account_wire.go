package main

import (
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/account"
)

// resolveAccountEnv resolves the active account from the registry using the
// priority chain and returns the account's config_dir path. It performs a
// pre-flight check to verify the config_dir exists and is readable.
//
// If no account is resolved (all handles empty and no registry default),
// it returns an empty string and nil error (graceful no-op).
// If the resolved handle is not found in the registry, it returns an error
// naming the unknown handle. If the config_dir does not exist or is not
// readable, it returns an error naming the handle and the path.
func resolveAccountEnv(reg account.Registry, envHandle, flagHandle, configHandle string) (string, error) {
	acct, err := account.Resolve(reg, envHandle, flagHandle, configHandle)
	if err != nil {
		return "", err
	}

	// No account resolved — graceful no-op.
	if acct.Handle == "" {
		return "", nil
	}

	// Pre-flight check: verify config_dir exists and is readable.
	info, err := os.Stat(acct.ConfigDir)
	if err != nil {
		return "", fmt.Errorf("account %q: config_dir %q does not exist: %w", acct.Handle, acct.ConfigDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("account %q: config_dir %q is not a directory", acct.Handle, acct.ConfigDir)
	}
	f, err := os.Open(acct.ConfigDir)
	if err != nil {
		return "", fmt.Errorf("account %q: config_dir %q is not readable: %w", acct.Handle, acct.ConfigDir, err)
	}
	f.Close() //nolint:errcheck // best-effort readability check; Open succeeded

	return acct.ConfigDir, nil
}
