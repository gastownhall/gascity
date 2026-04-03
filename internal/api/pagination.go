package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
)

// pageParams holds parsed cursor-based pagination parameters.
type pageParams struct {
	Offset   int
	Limit    int
	IsPaging bool // true when the client explicitly supplied cursor or limit
}

// maxPaginationLimit caps the maximum page size to prevent oversized responses.
const maxPaginationLimit = 1000

// parsePagination extracts cursor and limit from query parameters.
// The cursor is an opaque string that encodes an offset into the result set.
// Limit is capped at maxPaginationLimit regardless of the requested value.
func parsePagination(r *http.Request, defaultLimit int) pageParams {
	q := r.URL.Query()
	isPaging := q.Has("cursor")
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n == 0 {
				limit = maxPaginationLimit // 0 means "no limit"
			} else if n > 0 {
				limit = n
			}
		}
	}
	if limit > maxPaginationLimit {
		limit = maxPaginationLimit
	}
	var offset int
	if c := q.Get("cursor"); c != "" {
		offset = decodeCursor(c)
	}
	return pageParams{Offset: offset, Limit: limit, IsPaging: isPaging}
}

// decodeCursor decodes an opaque cursor string to an integer offset.
// Returns 0 for invalid or empty cursors.
func decodeCursor(cursor string) int {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(data))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// encodeCursor encodes an integer offset as an opaque cursor string.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// paginate applies cursor-based pagination to a slice. Returns the page,
// the total count (pre-pagination), and an opaque cursor for the next page
// (empty string if this is the last page).
func paginate[T any](items []T, pp pageParams) (page []T, total int, nextCursor string) {
	total = len(items)
	if pp.Offset >= total {
		return nil, total, ""
	}
	end := pp.Offset + pp.Limit
	if end > total {
		end = total
	}
	page = items[pp.Offset:end]
	if end < total {
		nextCursor = encodeCursor(end)
	}
	return page, total, nextCursor
}
