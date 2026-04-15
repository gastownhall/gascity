package account

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// handlePattern matches POSIX-safe handles: letters, digits, hyphens, underscores.
var handlePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateHandle checks that handle is non-empty and contains only
// POSIX-safe characters (letters, digits, hyphens, underscores).
// If invalid, the returned error names the disallowed characters found.
func ValidateHandle(handle string) error {
	if handle == "" {
		return fmt.Errorf("handle must not be empty")
	}
	if !handlePattern.MatchString(handle) {
		// Collect the disallowed characters for the error message.
		var bad []string
		seen := make(map[rune]bool)
		for _, r := range handle {
			if !seen[r] && !isHandleChar(r) {
				bad = append(bad, fmt.Sprintf("%q", string(r)))
				seen[r] = true
			}
		}
		return fmt.Errorf("handle %q contains disallowed characters: %s (only letters, digits, hyphens, underscores allowed)",
			handle, strings.Join(bad, ", "))
	}
	return nil
}

// isHandleChar reports whether r is a valid handle character.
func isHandleChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_'
}

// ValidateNewAccount checks that acct can be added to reg:
//   - handle must be valid (POSIX-safe, non-empty)
//   - handle must not already exist in the registry
//   - config_dir must exist, be a directory, and be readable
func ValidateNewAccount(reg Registry, acct Account) error {
	if err := ValidateHandle(acct.Handle); err != nil {
		return err
	}

	// Check for duplicate handle.
	for _, existing := range reg.Accounts {
		if existing.Handle == acct.Handle {
			return fmt.Errorf("handle %s is already registered", acct.Handle)
		}
	}

	// Check config_dir exists.
	info, err := os.Stat(acct.ConfigDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config_dir %q does not exist", acct.ConfigDir)
		}
		return fmt.Errorf("config_dir %q: %w", acct.ConfigDir, err)
	}

	// Check config_dir is a directory.
	if !info.IsDir() {
		return fmt.Errorf("config_dir %q is not a directory", acct.ConfigDir)
	}

	// Check config_dir is readable by attempting to open it.
	f, err := os.Open(acct.ConfigDir)
	if err != nil {
		return fmt.Errorf("config_dir %q is not readable: %w", acct.ConfigDir, err)
	}
	f.Close() //nolint:errcheck // best-effort readability check; Open succeeded

	return nil
}
