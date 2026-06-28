// Package citywriteauth verifies single-use, request-bound authorization grants
// for city configuration mutations.
//
// It is verify-only. A configured trusted authority mints grants out of band
// with an ed25519 private key; the supervisor verifies them here against the
// corresponding public key(s). This lets any operator require that every config
// mutation carry a credential only their authority can produce. The package
// names no particular authority and ships no minter — an operator self-hosting
// can sign grants with their own key using the wire format below.
//
// # Wire format
//
// A grant token is two base64url (no padding) segments joined by ".":
//
//	base64url(payload) "." base64url(signature)
//
// payload is the UTF-8 JSON encoding of a [Grant]. signature is the ed25519
// signature over the exact payload bytes (before base64url encoding); a minter
// MUST sign over the same bytes it transmits. Fields:
//
//	kid    key id selecting the verifying public key
//	aud    audience discriminator; must equal the verifier's expected value
//	city   the target city; must equal the request's {cityName} path segment
//	epoch  rotation/teardown counter; must be >= the verifier's floor
//	iat    issued-at, unix seconds
//	exp    expiry, unix seconds; exp-iat must be <= the verifier's MaxTTL
//	jti    unique token id; single-use, enforced by a ReplayGuard
//	req    request binding; see [ReqDigest]
//
// The req binding ties a grant to exactly one method+path+body, so a captured
// grant cannot be repurposed for a different mutation even by a caller able to
// read it.
//
// # Integration status
//
// This package is verify-only and is not yet wired into a request path. No
// middleware currently buffers the request body, computes [ReqDigest], derives
// the expected city from the {cityName} path segment, sources the verifying
// keys, and calls [Verifier.Verify] to fail closed on a missing or invalid
// X-GC-City-Write header. Until that integration lands, importing this package
// grants no protection on its own. The supervisor/API integration and a
// request-level integration test that drives a real request through [Expect]
// and [Verifier.Verify] are tracked as follow-up work in ga-wojrnk.
package citywriteauth
