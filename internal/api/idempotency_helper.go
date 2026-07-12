package api

import "github.com/gastownhall/gascity/internal/api/apierr"

// withIdempotency runs create() at most once per (scoped key, request body),
// giving create endpoints safe retries via the Idempotency-Key header.
//
// The scoped key is "POST:<path>:<key>" — namespaced by path so the same
// Idempotency-Key value on two different endpoints within one city can't
// collide. Every caller passes a static endpoint path (e.g. "/v0/agents"), so
// this scoping is intra-city only; cross-city isolation does NOT come from the
// key. It comes from each city owning a separate *Server, and therefore a
// separate idem cache, built per city by getCityServer via New(state). A future
// refactor that hoisted idem to a process-wide scope would silently reintroduce
// cross-city key collisions despite this scoping. On a repeat with a completed
// reservation it replays the cached typed body value; an in-flight repeat
// returns apierr.IdempotencyInFlight (409); a same-key/different-body repeat
// returns apierr.IdempotencyMismatch (422).
//
// It ALWAYS releases the pending reservation when create() returns an error OR
// panics (via defer), so no caller can leak a reservation — the defect the
// hand-rolled per-handler unreserve boilerplate was prone to. When key == "" it
// is a passthrough: create() runs exactly once and nothing is cached.
//
// create() should perform all fallible work (validation, the store write) and
// return the domain body value to cache. Callers wrap that value in their
// response envelope after withIdempotency returns, so envelope fields derived
// from live state (e.g. the X-GC-Index event sequence) stay fresh on replay.
func withIdempotency[T any](s *Server, path, key string, body any, create func() (T, error)) (T, error) {
	var zero T
	if key == "" {
		return create()
	}
	scopedKey := "POST:" + path + ":" + key
	bodyHash := hashBody(body)

	existing, found := s.idem.reserve(scopedKey, bodyHash)
	if found {
		if existing.bodyHash != bodyHash {
			return zero, apierr.IdempotencyMismatch.Msg("idempotency_mismatch: Idempotency-Key reused with different request body")
		}
		if existing.pending {
			return zero, apierr.IdempotencyInFlight.Msg("in_flight: request with this Idempotency-Key is already in progress")
		}
		if v, ok := replayAs[T](existing); ok {
			return v, nil
		}
		// Completed entry of an unexpected type (should be impossible for a
		// given endpoint's fixed T). Fall through and recreate rather than
		// serve a wrong-typed replay.
	}

	// Release the reservation on any non-success exit — an error return OR a
	// panic in create(). unreserve only drops a *pending* entry, so on the
	// wrong-typed fall-through above (where this caller holds no reservation) it
	// is a harmless no-op against the completed entry.
	settled := false
	defer func() {
		if !settled {
			s.idem.unreserve(scopedKey)
		}
	}()

	v, err := create()
	if err != nil {
		return zero, err
	}
	settled = true
	s.idem.storeResponse(scopedKey, bodyHash, v)
	return v, nil
}
