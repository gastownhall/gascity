package api

import (
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// Generic keyset pagination over non-bead collections (mail messages,
// sessions, convoys). Same contract as the bead list (see beadListSeek /
// resolveBeadListPage): opaque v1 "cb" tokens carrying the (created_at, id)
// boundary of the last row served, a typed 400 on anything else, pages cut as
// the contiguous suffix strictly after the boundary in the collection's
// (created_at DESC, id DESC) total order, and Total keeping its full-set
// meaning across a walk.

// keysetKey is the (created_at, id) sort key of a paginated row.
type keysetKey struct {
	CreatedAt time.Time
	ID        string
}

// keysetAfterDesc reports whether k sorts strictly after boundary b in
// (created_at DESC, id DESC) order. The comparison mirrors
// beads.SeekBoundary.After exactly — tie-break included — so a page boundary
// can never skip or duplicate a row.
func keysetAfterDesc(k, b keysetKey) bool {
	if k.CreatedAt.Before(b.CreatedAt) {
		return true
	}
	return k.CreatedAt.Equal(b.CreatedAt) && k.ID < b.ID
}

// sortKeysetDesc sorts items into the (created_at DESC, id DESC) total order —
// the precondition for keyset paging. Collections whose sources return
// store-default (nondeterministic) or CreatedAt-only orders get one canonical
// order here.
func sortKeysetDesc[T any](items []T, key func(T) keysetKey) {
	sort.Slice(items, func(i, j int) bool {
		ki, kj := key(items[i]), key(items[j])
		if !ki.CreatedAt.Equal(kj.CreatedAt) {
			return ki.CreatedAt.After(kj.CreatedAt)
		}
		return ki.ID > kj.ID
	})
}

// keysetSeek parses a request cursor into a page boundary. An empty cursor is
// first-page paging (nil boundary, no error). Any other non-empty value —
// garbage, a legacy offset cursor, or a wrong-kind token — is a typed 400
// rather than a silent restart at page 1, which duplicated rows under the old
// integer-offset scheme.
func keysetSeek(cursor string) (*keysetKey, error) {
	if cursor == "" {
		return nil, nil
	}
	c, err := decodeKeysetCursor(cursor)
	if err != nil || c.Kind != cursorKindCreatedID {
		return nil, apierr.InvalidCursor.Msg("cursor is not a valid pagination token; re-fetch the first page")
	}
	return &keysetKey{CreatedAt: c.CreatedAt, ID: c.ID}, nil
}

// resolveKeysetPage cuts the response page, Total, and has-more flag from the
// complete result set, which must already be in (created_at DESC, id DESC)
// order. Total is the full set's length — constant across a walk — and the
// page is the contiguous suffix strictly after the boundary. It performs no
// I/O.
func resolveKeysetPage[T any](items []T, key func(T) keysetKey, seek *keysetKey, limit int) (page []T, total int, hasMore bool) {
	total = len(items)
	start := 0
	if seek != nil {
		for start < len(items) && !keysetAfterDesc(key(items[start]), *seek) {
			start++
		}
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], total, end < len(items)
}

// mintKeysetNextCursor returns the continuation cursor for a truncated page:
// the (created_at, id) boundary of the last row served. An exhausted or empty
// page mints nothing, which the client reads as walk-complete.
func mintKeysetNextCursor[T any](page []T, key func(T) keysetKey, hasMore bool) string {
	if !hasMore || len(page) == 0 {
		return ""
	}
	k := key(page[len(page)-1])
	return encodeKeysetCursor(keysetCursor{
		Kind:      cursorKindCreatedID,
		CreatedAt: k.CreatedAt,
		ID:        k.ID,
	})
}
