package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DoltTagger tags a Dolt-backed store's current commit: an exact snapshot
// that copies nothing (S5 §5.3).
type DoltTagger interface {
	// DoltTag tags HEAD as name with message and returns the commit it
	// tagged and whether the working set held changes HEAD does not cover.
	DoltTag(name, message string) (DoltTagResult, error)
}

// DoltTagResult is what one DoltTag call tagged.
type DoltTagResult struct {
	// Commit is the hash HEAD resolved to when the tag was taken.
	Commit string
	// DirtyWorkingSet reports uncommitted changes the tag does not cover.
	DirtyWorkingSet bool
}

// ErrDoltTagUnsupported is what a tag reports on a storage handle with no SQL
// connection to issue it on (a served or proxied lane).
var ErrDoltTagUnsupported = errors.New("the store exposes no Dolt SQL connection to tag on")

// nativeDoltTagTimeout bounds the tag and its two reads.
const nativeDoltTagTimeout = 30 * time.Second

// NativeDoltTagger returns the tagger behind s, seeing through typed class
// handles: s itself when it is a DoltTagger, else an adapter over the native
// Dolt store under it. It is not a method of NativeDoltStore on purpose: a
// method there is one every store wrapper must forward, and the tag is the
// snapshot's alone, taken on a store the snapshot opens itself.
func NativeDoltTagger(s Store) (DoltTagger, bool) {
	if tagger, ok := As[DoltTagger](s); ok {
		return tagger, true
	}
	if native, ok := As[*NativeDoltStore](s); ok {
		return nativeDoltTagger{native}, true
	}
	return nil, false
}

type nativeDoltTagger struct{ s *NativeDoltStore }

// DoltTag tags HEAD with CALL DOLT_TAG on the storage's own SQL connection, so
// the tag lands in the database the store reads. A tag is a write, so a
// read-only (proxied) handle refuses it.
func (t nativeDoltTagger) DoltTag(name, message string) (DoltTagResult, error) {
	if err := t.s.readOnlyGuard(); err != nil {
		return DoltTagResult{}, err
	}
	var result DoltTagResult
	err := t.withConn(func(ctx context.Context, conn *sql.Conn) error {
		if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&result.Commit); err != nil {
			return fmt.Errorf("reading HEAD: %w", err)
		}
		var dirty int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status").Scan(&dirty); err != nil {
			return fmt.Errorf("reading the working set: %w", err)
		}
		result.DirtyWorkingSet = dirty > 0
		_, err := conn.ExecContext(ctx, "CALL DOLT_TAG(?, ?, '-m', ?)", name, result.Commit, message)
		return err
	})
	if err != nil {
		return DoltTagResult{}, fmt.Errorf("tagging %s: %w", name, err)
	}
	return result, nil
}

// withConn runs fn on one pinned connection of the store's storage.
func (t nativeDoltTagger) withConn(fn func(context.Context, *sql.Conn) error) error {
	storage, release, err := t.s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	raw, ok := storage.(interface{ DB() *sql.DB })
	if !ok || raw.DB() == nil {
		return ErrDoltTagUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeDoltTagTimeout)
	defer cancel()
	conn, err := raw.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // returns the pinned connection to the pool
	return fn(ctx, conn)
}
