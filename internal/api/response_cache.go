package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

var responseCacheTTL = 5 * time.Second

// responseCacheStableTTL is used for endpoints whose data changes
// infrequently (config, formulas, providers). These use time-based
// expiry only, ignoring the event index.
var responseCacheStableTTL = 30 * time.Second //nolint:unused // wired in follow-up PR

type responseCacheEntry struct {
	index   uint64
	expires time.Time
	body    []byte
}

func responseCacheKey(name string, r *http.Request) string {
	if r == nil || r.URL == nil || r.URL.RawQuery == "" {
		return name
	}
	return name + "?" + r.URL.RawQuery
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

// storeResponseStable caches a response with a long TTL that is NOT
// invalidated by event index changes. Use for slow-changing data
// (config, formulas, providers).
func (s *Server) storeResponseStable(key string, v any) ([]byte, error) { //nolint:unused // wired in follow-up PR
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
		index:   0, // stable entries ignore index
		expires: time.Now().Add(responseCacheStableTTL),
		body:    append([]byte(nil), body...),
	}
	return body, nil
}

// cachedResponseStable checks for a cached response that uses time-based
// expiry only (no event index). Used with storeResponseStable.
func (s *Server) cachedResponseStable(key string) ([]byte, bool) { //nolint:unused // wired in follow-up PR
	if key == "" {
		return nil, false
	}
	s.responseCacheMu.Lock()
	defer s.responseCacheMu.Unlock()
	if s.responseCacheEntries == nil {
		return nil, false
	}
	entry, ok := s.responseCacheEntries[key]
	if !ok || entry.index != 0 || time.Now().After(entry.expires) {
		return nil, false
	}
	return append([]byte(nil), entry.body...), true
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

func writeCachedJSON(w http.ResponseWriter, index uint64, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GC-Index", strconv.FormatUint(index, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
