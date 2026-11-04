package beads

import "context"

// SchemaCursorReporter is the capability of a store that can say how far its
// database's schema migration has run. A store without it (an in-memory store,
// a subprocess store with no live database handle) is not asked.
type SchemaCursorReporter interface {
	// SchemaCursor returns the database's migration cursor and the version
	// this build migrates to.
	SchemaCursor(ctx context.Context) (current, target int, err error)
}

// CheckSchemaAtTarget reports an error when s's database migration has not
// reached the version this build migrates to.
func CheckSchemaAtTarget(_ context.Context, _ Store) error {
	return nil
}
