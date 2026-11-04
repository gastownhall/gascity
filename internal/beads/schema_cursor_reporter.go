package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	beadsschema "github.com/steveyegge/beads/schema"
)

// ErrSchemaCursorUnavailable is what SchemaCursor returns when the store's
// backend exposes no live database handle to read the cursor from (an embedded
// store). It means "cannot be asked", not "could not be read":
// CheckSchemaAtTarget leaves such a store on its own readiness signal.
var ErrSchemaCursorUnavailable = errors.New("schema cursor unavailable")

// SchemaCursorReporter is the capability of a store that can say how far its
// database's schema migration has run. A store without it (an in-memory store,
// a subprocess store with no live database handle) is not asked.
type SchemaCursorReporter interface {
	// SchemaCursor returns the database's migration cursor and the version
	// this build migrates to.
	SchemaCursor(ctx context.Context) (current, target int, err error)
}

// CheckSchemaAtTarget reports an error when s's database migration has not
// reached the version this build migrates to. A store ahead of the target is
// schema skew, which the library's open already classifies; it passes here.
//
// The error names both cursors and the sanctioned recovery. gc never completes
// the migration itself: promoting a shared database's schema is the operator's
// consent to give (beads#5920).
func CheckSchemaAtTarget(ctx context.Context, s Store) error {
	reporter, ok := As[SchemaCursorReporter](s)
	if !ok {
		return nil
	}
	current, target, err := reporter.SchemaCursor(ctx)
	if errors.Is(err, ErrSchemaCursorUnavailable) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the schema migration cursor: %w", err)
	}
	if current < target {
		return fmt.Errorf("schema migration stalled at v%d of v%d: the store is not ready, and gc does not complete it; "+
			"once every bd client of this server is upgraded, run 'bd migrate schema' once, from one designated migrator",
			current, target)
	}
	return nil
}

// SchemaCursor implements SchemaCursorReporter from the database's own
// schema_migrations table, through the live handle of a server-mode store.
func (s *NativeDoltStore) SchemaCursor(ctx context.Context) (current, target int, err error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return 0, 0, err
	}
	defer release()
	accessor, ok := storage.(interface{ UnderlyingDB() *sql.DB })
	if !ok {
		return 0, 0, fmt.Errorf("%w: the linked library's %T exports no UnderlyingDB", ErrSchemaCursorUnavailable, storage)
	}
	current, err = beadsschema.CurrentVersion(ctx, accessor.UnderlyingDB())
	if err != nil {
		return 0, 0, err
	}
	return current, beadsschema.LatestVersion(), nil
}
