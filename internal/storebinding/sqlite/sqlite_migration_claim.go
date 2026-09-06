package sqlite

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/storebinding"
)

// validateSQLiteMigrationGuardClaim confirms that the generic coordinator's
// city-wide guard claim remains live. The coordinator owns city-scope binding;
// SQLite bindings may use a deliberately custom component path outside .gc.
func validateSQLiteMigrationGuardClaim(claim storebinding.MigrationGuardClaim) error {
	_, err := claim.Identity()
	if err != nil {
		return fmt.Errorf("reading SQLite migration guard claim: %w", err)
	}
	return nil
}
