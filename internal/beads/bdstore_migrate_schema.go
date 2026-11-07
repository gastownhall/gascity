package beads

import "fmt"

// MigrateSchemaMinBDVersion is the first bd release that ships
// `bd migrate schema`. An older bd's `migrate` has no subcommands and would
// run its unrelated bare migration instead, so callers skip the step on a bd
// that reports an older version.
const MigrateSchemaMinBDVersion = "1.0.5"

// MigrateSchema runs `bd migrate schema --quiet`, bd's client-side application
// of the schema migrations its own release knows and the store has not yet
// applied. It returns bd's output, which reports what the step did, and on
// failure wraps the runner's error with bd's diagnostic intact.
func (s *BdStore) MigrateSchema() ([]byte, error) {
	out, err := s.runner(s.dir, "bd", "migrate", "schema", "--quiet")
	if err != nil {
		return out, fmt.Errorf("bd migrate schema: %w", err)
	}
	return out, nil
}
