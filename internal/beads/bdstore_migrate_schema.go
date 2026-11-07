package beads

import (
	"errors"
	"fmt"
	"strings"
)

// MigrateSchemaMinBDVersion is the first bd release that ships
// `bd migrate schema`. An older bd's `migrate` has no subcommands: it would
// take `schema` as an ignored argument and run the bare metadata and repo-id
// migration instead, which is not this operation. Callers gate on it and leave
// pending schema migrations to bd's next store open, exactly as
// examples/bd/assets/scripts/gc-beads-bd.sh's finish_bd_schema_migrations
// does.
const MigrateSchemaMinBDVersion = "1.0.5"

// ErrRemoteBackedMigrateRefused reports that bd's remote-migrate gate
// (RemoteMigrateGateError) refused `bd migrate schema` because the store is
// remote-backed: migrating a remote-backed database is an operator decision
// bd deliberately withholds from unattended callers. The underlying bd error
// is preserved, so bd's refusal text and hint stay visible.
var ErrRemoteBackedMigrateRefused = errors.New("bd refused to migrate a remote-backed database")

// MigrateSchema runs `bd migrate schema --quiet`, bd's idempotent entry point
// for applying pending schema migrations to an existing store. Successful
// output is returned as-is for the caller to surface (from bd 1.3.0 a
// migration can lock out older co-resident clients, so what bd reports the
// step did is not dropped). Every error is the runner's original error —
// bd's diagnostics included — except a remote-backed refusal, which is
// wrapped in [ErrRemoteBackedMigrateRefused] with the original preserved.
func (s *BdStore) MigrateSchema() ([]byte, error) {
	out, err := s.runner(s.dir, "bd", "migrate", "schema", "--quiet")
	if err != nil {
		if isRemoteBackedMigrateRefusal(err.Error()) {
			return out, fmt.Errorf("%w: %w", ErrRemoteBackedMigrateRefused, err)
		}
		return out, err
	}
	return out, nil
}

// isRemoteBackedMigrateRefusal reports whether msg carries one of bd's two
// remote-migrate-gate refusal shapes: the auto-apply refusal ("refusing to
// auto-apply <what> to a remote-backed database") and the already-migrated or
// forked-schema one ("refusing to migrate a remote-backed database"). It
// matches the gate's own words, exactly as gc-beads-bd.sh's
// finish_bd_schema_migrations does, so an unrelated failure that merely
// mentions a remote and a migration is not read as this refusal.
func isRemoteBackedMigrateRefusal(msg string) bool {
	if strings.Contains(msg, "refusing to migrate a remote-backed database") {
		return true
	}
	const autoApply = "refusing to auto-apply "
	if i := strings.Index(msg, autoApply); i >= 0 {
		return strings.Contains(msg[i+len(autoApply):], " to a remote-backed database")
	}
	return false
}
