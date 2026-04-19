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

// responseCacheMaxEntries caps the in-memory cache. Query-parameter
// combinations (Rig, Pool, blocking index, etc.) produce a wide but
// bounded key space; a hostile or buggy client could still exhaust
// memory without a ceiling. Eviction is oldest-by-expiry, so the most
// recently warmed entries stay hot.
const responseCacheMaxEntries = 256

// responseCacheEntry stores the typed response value directly. No JSON
// serialization happens inside the cache — Huma serializes at the handler
// boundary on every hit. At 2-second TTL on localhost, the re-serialization
// cost is negligible, and we eliminate hand-written JSON (de)serialization
// from the cache-hit path (Phase 3 Fix 3l).
type responseCacheEntry struct {
	index   uint64
	expires time.Time
	value   any
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
		if fv.Kind() == reflect.Ptr {
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
	if !ok || entry.index != index || time.Now().After(entry.expires) {
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
		index:   index,
		expires: now.Add(responseCacheTTL),
		value:   v,
	}
}

// evictResponseCache drops expired entries, and — if the cache is still
// over cap — the single oldest-by-expiry remaining entry. Called under
// the cache mutex.
func (s *Server) evictResponseCache(now time.Time) {
	for k, entry := range s.responseCacheEntries {
		if now.After(entry.expires) {
			delete(s.responseCacheEntries, k)
		}
	}
	if len(s.responseCacheEntries) < responseCacheMaxEntries {
		return
	}
	var oldestKey string
	var oldestExp time.Time
	for k, entry := range s.responseCacheEntries {
		if oldestKey == "" || entry.expires.Before(oldestExp) {
			oldestKey = k
			oldestExp = entry.expires
		}
	}
	if oldestKey != "" {
		delete(s.responseCacheEntries, oldestKey)
	}
}

// cachedResponseAs is a generic helper: retrieve the cached value and
// deep-copy it via a JSON roundtrip before returning.
//
// The JSON roundtrip isolates concurrent readers: if a handler mutates
// the returned struct's slices/maps (e.g. appends a partial-error note
// before serialization), other readers of the same cache entry see the
// clean value. The cost is one Marshal + Unmarshal per cache hit, but
// Huma would re-serialize the value on output anyway so the net is ~1
// extra Unmarshal call on the read path.
func cachedResponseAs[T any](s *Server, key string, index uint64) (T, bool) {
	var zero T
	v, ok := s.cachedResponse(key, index)
	if !ok {
		return zero, false
	}
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
