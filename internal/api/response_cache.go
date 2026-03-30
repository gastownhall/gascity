package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

var responseCacheTTL = 2 * time.Second

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
