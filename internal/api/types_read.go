package api

import (
	"net/http"
	"strconv"
)

// This file hosts stable, CLI-facing read-path envelope types that are
// shared across per-domain migrations.
//
// Domain-specific CLI types (SessionSummary, MailHeader, RigView, ...)
// live alongside their decode_<domain>.go translator in this package so
// cmd/gc/ never imports internal/api/genclient directly. A new per-file
// migration typically adds:
//
//   - internal/api/decode_<domain>.go  — translators from genclient response
//                                        types to CLI-facing types, plus
//                                        small, focused unit tests.
//   - internal/api/client.go           — one typed read wrapper per endpoint
//                                        returning (<CLIType>, float64 age, error).
//   - cmd/gc/cmd_<domain>.go           — routes via apiClient(cityPath);
//                                        logs route=... on every exit path.
//
// The CacheAge float64 returned from each wrapper is the supervisor's
// CachingStore age in seconds at the time of the read, sourced from the
// X-GC-Cache-Age-S response header (populated by handlers via the
// cacheAgeSeconds helper). CLI callers surface this as _cache_age_s on
// the --json envelope and as a stale-read banner on human output when
// the age crosses 30 s.

// CachedRead is a convenience wrapper returned by read wrappers so the
// two cache-age-bearing return values stay co-located with the payload.
// Per-domain wrappers may return CachedRead[[]SessionSummary],
// CachedRead[MailHeader], and so on. A zero AgeSeconds means the server
// did not surface a cache age (non-caching store or fallback path).
type CachedRead[T any] struct {
	Body       T
	AgeSeconds float64
}

// cacheAgeHeader is the wire name of the X-GC-Cache-Age-S response header,
// set by read handlers via the cacheAgeSeconds helper and consumed by CLI
// wrappers through cacheAgeFromResponse.
const cacheAgeHeader = "X-GC-Cache-Age-S"

// cacheAgeFromResponse extracts the CachingStore age from the response's
// X-GC-Cache-Age-S header. Returns 0 when the response is nil, the header
// is absent, or the value fails to parse. The header value is a float64
// second count; fallback paths omit the header and naturally yield 0.
func cacheAgeFromResponse(r *http.Response) float64 {
	if r == nil {
		return 0
	}
	v := r.Header.Get(cacheAgeHeader)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}
