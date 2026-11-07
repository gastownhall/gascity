package beads

import (
	"errors"
	"fmt"
	"strings"
)

// MigrateSchemaMinBDVersion is the first bd release that ships
// `bd migrate schema`. An older bd's `migrate` has no subcommands and would
// run its unrelated bare migration instead, so callers skip the step on a bd
// that reports an older version.
const MigrateSchemaMinBDVersion = "1.0.5"

// ErrRemoteBackedMigrateRefused marks a `bd migrate schema` that bd's
// remote-migrate gate refused because the store is remote-backed. Applying
// those migrations is an operator decision bd describes in its message, not
// a broken store. A refusal to migrate a shared-server database without
// consent, or any other migration failure, does not match it.
var ErrRemoteBackedMigrateRefused = errors.New("bd refused to migrate a remote-backed database")

// MigrateSchema runs `bd migrate schema --quiet`, bd's client-side application
// of the schema migrations its own release knows and the store has not yet
// applied. It returns bd's output, which reports what the step did, and on
// failure wraps the runner's error with bd's diagnostic intact, additionally
// matching ErrRemoteBackedMigrateRefused when the remote-migrate gate refused.
func (s *BdStore) MigrateSchema() ([]byte, error) {
	out, err := s.runner(s.dir, "bd", "migrate", "schema", "--quiet")
	if err == nil {
		return out, nil
	}
	if isRemoteBackedMigrateRefusal(err.Error()) || isRemoteBackedMigrateRefusal(string(out)) {
		return out, fmt.Errorf("bd migrate schema: %w: %w", ErrRemoteBackedMigrateRefused, err)
	}
	return out, fmt.Errorf("bd migrate schema: %w", err)
}

// isRemoteBackedMigrateRefusal matches the two messages of bd's remote-migrate
// gate (RemoteMigrateGateError) that gc-beads-bd.sh's
// finish_bd_schema_migrations treats as a refusal rather than a failure: the
// auto-apply refusal, and the refusal for a remote that is already migrated
// or whose schema has forked.
func isRemoteBackedMigrateRefusal(msg string) bool {
	if strings.Contains(msg, "refusing to migrate a remote-backed database") {
		return true
	}
	_, rest, ok := strings.Cut(msg, "refusing to auto-apply ")
	return ok && strings.Contains(rest, " to a remote-backed database")
}
