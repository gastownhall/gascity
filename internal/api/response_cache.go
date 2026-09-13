package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

var responseCacheTTL = 2 * time.Second

// timeBucketResponseCacheTTL bounds how long a built body is reused by the
// endpoints that key their response-cache entry on a TIME bucket rather than
// the event sequence: /status (gascity#3186), /formulas/feed, and bounded
// all=true /beads reads (#3208). The shared index-keyed cache misses on every
// poll of a busy city — the sequence advances each tick — so O(store-size)
// bodies were rebuilt per request. These are coarse views where a few seconds
// of staleness is acceptable (responses report X-GC-Cache-Age-S where a
// CachingStore backs them); strict-freshness callers (blocking ?index=&wait=
// requests) bypass the time cache at each handler.
var timeBucketResponseCacheTTL = 2 * time.Second

// responseCacheTimeBucket returns a monotonically increasing generation that
// only changes once per timeBucketResponseCacheTTL window. Passing it where
// the shared cache expects an event index makes an entry survive across the
// event-sequence churn of a busy city while still expiring on a wall-clock
// TTL.
func responseCacheTimeBucket(now time.Time) uint64 {
	ttl := timeBucketResponseCacheTTL
	if ttl <= 0 {
		return uint64(now.UnixNano())
	}
	return uint64(now.UnixNano() / int64(ttl))
}

// responseCacheMaxEntries caps the in-memory cache. Query-parameter
// combinations (Rig, Pool, blocking index, etc.) produce a wide but
// bounded key space; a hostile or buggy client could still exhaust
// memory without a ceiling. Eviction is oldest-stored-first, so the most
// recently warmed entries stay hot.
const responseCacheMaxEntries = 256

// responseCacheEntry stores the typed response value directly. No JSON
// serialization happens inside the cache — Huma serializes at the handler
// boundary on every hit. At 2-second TTL on localhost, the re-serialization
// cost is negligible, and we eliminate hand-written JSON (de)serialization
// from the cache-hit path (Phase 3 Fix 3l).
type responseCacheEntry struct {
	index    uint64
	storedAt time.Time
	value    any
}

// expires is the index-keyed lookup deadline. Age-bounded lookups
// (cachedResponseWithinAge) apply their own caller-supplied window
// against storedAt instead.
func (e responseCacheEntry) expires() time.Time {
	return e.storedAt.Add(responseCacheTTL)
}

// cacheKeyFor derives a deterministic cache key for a Huma input struct.
//
// It walks the input's fields and collects any path/query/header parameters
// (identified by struct tags) into a stable string. The key is prefixed with
// name so different endpoints using the same input type don't collide.
//
// This replaces the hand-built string concatenation that handlers used to do:
//
//	cacheKey := "agents"
//	if input.Pool != "" || input.Rig != "" { cacheKey += "?" + input.Pool + ... }
//
// with:
//
//	cacheKey := cacheKeyFor("agents", input)
//
// Adding a new query parameter to an input struct automatically participates
// in the cache key — no handler code needs to change.
func cacheKeyFor(name string, input any) string {
	var parts []string
	collectCacheKeyParts(reflect.ValueOf(input), &parts)
	if len(parts) == 0 {
		return name
	}
	sort.Strings(parts)
	return name + "?" + strings.Join(parts, "&")
}

// collectCacheKeyParts walks a struct value and appends "tag=value" strings
// for each path/query/header parameter it finds. Embedded structs are
// recursed into so mixins (BlockingParam, PaginationParam) contribute their
// fields. The Body field is intentionally ignored — bodies can be large and
// are not part of the request's cacheable identity.
func collectCacheKeyParts(v reflect.Value, parts *[]string) {
	v = reflect.Indirect(v)
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Anonymous {
			// Embedded mixin (BlockingParam, PaginationParam, etc.).
			collectCacheKeyParts(v.Field(i), parts)
			continue
		}
		if field.Name == "Body" {
			// Request bodies are not part of the cache key.
			continue
		}
		var tagName string
		for _, kind := range []string{"path", "query", "header"} {
			if tag := field.Tag.Get(kind); tag != "" {
				tagName = kind + ":" + tag
				break
			}
		}
		if tagName == "" {
			continue
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		// Skip zero values so empty/default fields don't bloat the cache
		// key. reflect.Value.IsZero is Kind-safe, so uint64 / float /
		// time.Duration fields no longer panic the way the previous
		// fv.Int() path did for uint kinds.
		if fv.IsZero() {
			continue
		}
		*parts = append(*parts, fmt.Sprintf("%s=%v", tagName, fv.Interface()))
	}
}

// cachedResponse returns the cached typed value for (key, index) if present
// and unexpired. Callers type-assert the returned any to the concrete type
// they stored.
func (s *Server) cachedResponse(key string, index uint64) (any, bool) {
	if key == "" {
		return nil, false
	}
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseCacheEntries == nil {
		return nil, false
	}
	entry, ok := s.responseCacheEntries[key]
	if !ok || entry.index != index || time.Now().After(entry.expires()) {
		return nil, false
	}
	return entry.value, true
}

// storeResponse caches the typed value under (key, index). No JSON work is
// performed here; Huma serializes on output and cache hits clone through
// cachedResponseAs. The map is capped at responseCacheMaxEntries with
// TTL-based eviction on insert.
func (s *Server) storeResponse(key string, index uint64, v any) {
	if key == "" {
		return
	}
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseCacheEntries == nil {
		s.responseCacheEntries = make(map[string]responseCacheEntry)
	}
	now := time.Now()
	if _, exists := s.responseCacheEntries[key]; !exists && len(s.responseCacheEntries) >= responseCacheMaxEntries {
		s.evictResponseCache(now)
	}
	s.responseCacheEntries[key] = responseCacheEntry{
		index:    index,
		storedAt: now,
		value:    v,
	}
}

// evictResponseCache drops expired entries, and — if the cache is still
// over cap — the single oldest-stored remaining entry. Called under
// the cache mutex.
func (s *Server) evictResponseCache(now time.Time) {
	for k, entry := range s.responseCacheEntries {
		if now.After(entry.expires()) {
			delete(s.responseCacheEntries, k)
		}
	}
	if len(s.responseCacheEntries) < responseCacheMaxEntries {
		return
	}
	var oldestKey string
	var oldestStored time.Time
	for k, entry := range s.responseCacheEntries {
		if oldestKey == "" || entry.storedAt.Before(oldestStored) {
			oldestKey = k
			oldestStored = entry.storedAt
		}
	}
	if oldestKey != "" {
		delete(s.responseCacheEntries, oldestKey)
	}
}

// cachedResponseWithinAge returns the cached value for key when the entry
// was stored within maxAge, regardless of the event index it was built at.
// For endpoints whose rebuild fans out to external stores (status), the
// exact-index lookup never hits on busy cities — every event advances the
// index — so callers opt into a bounded-staleness window instead.
func (s *Server) cachedResponseWithinAge(key string, maxAge time.Duration) (any, bool) {
	if key == "" {
		return nil, false
	}
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseCacheEntries == nil {
		return nil, false
	}
	entry, ok := s.responseCacheEntries[key]
	if !ok || time.Since(entry.storedAt) > maxAge {
		return nil, false
	}
	return entry.value, true
}

// staleResponse returns the cached value for key regardless of its age,
// together with how old it is. Unlike cachedResponse / cachedResponseWithinAge
// this never rejects an entry on staleness — it is the last-resort read for
// callers implementing stale-while-revalidate (ra-4u2eqc): when both the
// index/bucket-keyed and age-bounded lookups miss, this is what lets a
// handler serve the last-known-good body instead of blocking on a rebuild.
// Returns ok=false only when nothing has ever been cached under key (the
// genuine cold-start case, where there is no stale value to serve).
func (s *Server) staleResponse(key string) (value any, age time.Duration, ok bool) {
	if key == "" {
		return nil, 0, false
	}
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseCacheEntries == nil {
		return nil, 0, false
	}
	entry, found := s.responseCacheEntries[key]
	if !found {
		return nil, 0, false
	}
	return entry.value, time.Since(entry.storedAt), true
}

// staleResponseAs is staleResponse for typed callers: deep-copies the cached
// value via a JSON roundtrip the same way cachedResponseAs does.
func staleResponseAs[T any](s *Server, key string) (T, time.Duration, bool) {
	v, age, ok := s.staleResponse(key)
	if !ok {
		var zero T
		return zero, 0, false
	}
	body, ok := cloneCachedValue[T](v)
	if !ok {
		var zero T
		return zero, 0, false
	}
	return body, age, true
}

// beginResponseRefresh reports whether the caller won the right to run a
// background stale-while-revalidate refresh for key. Returns false when a
// refresh for key is already in flight, so concurrent cache-miss requests
// for the same key coalesce onto a single background rebuild instead of
// each launching their own (ra-4u2eqc). Pair with endResponseRefresh, which
// the refresh must call on every exit path (including panics via defer).
func (s *Server) beginResponseRefresh(key string) bool {
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseRefreshing == nil {
		s.responseRefreshing = make(map[string]bool)
	}
	if s.responseRefreshing[key] {
		return false
	}
	s.responseRefreshing[key] = true
	return true
}

// endResponseRefresh releases the in-flight guard beginResponseRefresh took
// for key, so the next cache-miss request may trigger another refresh.
func (s *Server) endResponseRefresh(key string) {
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	delete(s.responseRefreshing, key)
}

// cachedResponseAs is a generic helper: retrieve the cached value and
// deep-copy it via a JSON roundtrip before returning.
func cachedResponseAs[T any](s *Server, key string, index uint64) (T, bool) {
	v, ok := s.cachedResponse(key, index)
	if !ok {
		var zero T
		return zero, false
	}
	return cloneCachedValue[T](v)
}

// cachedResponseWithinAgeAs is cachedResponseAs for age-bounded lookups.
func cachedResponseWithinAgeAs[T any](s *Server, key string, maxAge time.Duration) (T, bool) {
	v, ok := s.cachedResponseWithinAge(key, maxAge)
	if !ok {
		var zero T
		return zero, false
	}
	return cloneCachedValue[T](v)
}

// cloneCachedValue deep-copies a cached value via a JSON roundtrip.
//
// The JSON roundtrip isolates concurrent readers: if a handler mutates
// the returned struct's slices/maps (e.g. appends a partial-error note
// before serialization), other readers of the same cache entry see the
// clean value. The cost is one Marshal + Unmarshal per cache hit, but
// Huma would re-serialize the value on output anyway so the net is ~1
// extra Unmarshal call on the read path.
func cloneCachedValue[T any](v any) (T, bool) {
	var zero T
	data, err := json.Marshal(v)
	if err != nil {
		return zero, false
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return zero, false
	}
	return result, true
}
