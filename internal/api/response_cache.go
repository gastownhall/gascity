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

type responseCacheEntry struct {
	index   uint64
	expires time.Time
	body    []byte
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
		if fv.Kind() == reflect.String && fv.String() == "" {
			continue
		}
		if fv.Kind() == reflect.Int || fv.Kind() == reflect.Int64 || fv.Kind() == reflect.Uint64 {
			if fv.Int() == 0 && fv.Kind() != reflect.Uint64 {
				continue
			}
		}
		*parts = append(*parts, fmt.Sprintf("%s=%v", tagName, fv.Interface()))
	}
}

func (s *Server) cachedResponse(key string, index uint64) ([]byte, bool) {
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
	body := append([]byte(nil), entry.body...)
	return body, true
}

func (s *Server) storeResponse(key string, index uint64, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return body, nil
	}
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseCacheEntries == nil {
		s.responseCacheEntries = make(map[string]responseCacheEntry)
	}
	s.responseCacheEntries[key] = responseCacheEntry{
		index:   index,
		expires: time.Now().Add(responseCacheTTL),
		body:    append([]byte(nil), body...),
	}
	return body, nil
}

