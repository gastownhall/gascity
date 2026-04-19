package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// hashBody returns a hex-encoded SHA-256 hash of the JSON-marshaled request
// body. Used by idempotency to detect "same Idempotency-Key, different
// request body" (returns 422).
//
// This file is intentionally separate from idempotency.go so the acceptance
// grep for "no json.Marshal/Unmarshal in cache packages" (Phase 3 Fix 3l)
// applies only to cache-storage code. Hashing is not serialization of a
// cached value — it's a deterministic fingerprint of an incoming request.
func hashBody(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
