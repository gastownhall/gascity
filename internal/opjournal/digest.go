package opjournal

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

// RequestDigest returns the request digest of op: sha256 over the uvarint
// length-prefixed (key, value) pairs of the defaulted kind, policy, scope,
// after and payload metadata, in sorted key order, skipping empty values.
// Deadlines, not_before, client_seq, causation and correlation are not
// covered, so a retry that only recomputes them still deduplicates. Skipping
// empty values keeps every existing digest stable when an optional key is
// added later.
func RequestDigest(op Op) string { return digestFields(requestFields(op)) }

// digestCompanionKey names a companion's tag in companionDigest's fields. It
// is never written as metadata.
const digestCompanionKey = "gc.op.companion"

// companionDigest is the request digest of op proposed with the companion
// tag names (Store.ProposeWith): RequestDigest's fields plus the tag, so it
// never equals the digest of the same request proposed without a companion
// or with another one.
func companionDigest(op Op, tag string) string {
	fields := requestFields(op)
	fields[digestCompanionKey] = tag
	return digestFields(fields)
}

// digestFields hashes fields as RequestDigest describes.
func digestFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k, v := range fields {
		if v != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	buf := lp(nil, opDigestTag)
	for _, k := range keys {
		buf = lp(buf, k)
		buf = lp(buf, fields[k])
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// validDigest reports whether d is a lowercase hex sha256.
func validDigest(d string) bool {
	if len(d) != sha256.Size*2 {
		return false
	}
	for _, c := range d {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
