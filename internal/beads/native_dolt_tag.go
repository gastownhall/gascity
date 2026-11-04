package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DoltTagger is the optional surface a Dolt-backed store implements when it
// can tag its current commit: an exact snapshot that copies nothing (S5 §5.3).
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

// ErrDoltTagUnsupported is what DoltTag reports on a storage handle with no
// SQL connection to issue the tag on (a served or proxied lane).
var ErrDoltTagUnsupported = errors.New("the store exposes no Dolt SQL connection to tag on")

// nativeDoltTagTimeout bounds the tag and its two reads.
const nativeDoltTagTimeout = 30 * time.Second

// DoltTag tags the store's HEAD with CALL DOLT_TAG on the storage's own SQL
// connection, so the tag lands in the database this store reads. A tag is a
// write, so a read-only (proxied) handle refuses it.
func (s *NativeDoltStore) DoltTag(name, message string) (DoltTagResult, error) {
	if err := s.readOnlyGuard(); err != nil {
		return DoltTagResult{}, err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return DoltTagResult{}, err
	}
	defer release()
	raw, ok := storage.(interface{ DB() *sql.DB })
	if !ok || raw.DB() == nil {
		return DoltTagResult{}, ErrDoltTagUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeDoltTagTimeout)
	defer cancel()
	conn, err := raw.DB().Conn(ctx)
	if err != nil {
		return DoltTagResult{}, fmt.Errorf("tagging %s: %w", name, err)
	}
	defer conn.Close() //nolint:errcheck // returns the pinned connection to the pool
	var result DoltTagResult
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('HEAD')").Scan(&result.Commit); err != nil {
		return DoltTagResult{}, fmt.Errorf("tagging %s: reading HEAD: %w", name, err)
	}
	var dirty int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status").Scan(&dirty); err != nil {
		return DoltTagResult{}, fmt.Errorf("tagging %s: reading the working set: %w", name, err)
	}
	result.DirtyWorkingSet = dirty > 0
	if _, err := conn.ExecContext(ctx, "CALL DOLT_TAG(?, ?, '-m', ?)", name, result.Commit, message); err != nil {
		return DoltTagResult{}, fmt.Errorf("tagging %s: %w", name, err)
	}
	return result, nil
}

// DoltTagCommit returns the commit the tag name points at, for a snapshot's
// verify, or ok=false when no such tag exists.
func (s *NativeDoltStore) DoltTagCommit(name string) (commit string, ok bool, err error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return "", false, err
	}
	defer release()
	raw, isSQL := storage.(interface{ DB() *sql.DB })
	if !isSQL || raw.DB() == nil {
		return "", false, ErrDoltTagUnsupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeDoltTagTimeout)
	defer cancel()
	err = raw.DB().QueryRowContext(ctx, "SELECT tag_hash FROM dolt_tags WHERE tag_name = ?", name).Scan(&commit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading tag %s: %w", name, err)
	}
	return commit, true, nil
}
