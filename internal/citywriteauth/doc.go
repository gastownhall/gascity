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
//	       (or its configured legacy value, see [Options.LegacyAud])
//	city   the target city; must equal the request's {cityName} path segment
//	cid    tenancy binding: the org-unique city id the grant was minted for;
//	       required to match exactly when the verifier is configured with a
//	       CID (see [Options.CID]); absent on legacy grants
//	epoch  rotation/teardown counter; must be >= the verifier's floor
//	iat    issued-at, unix seconds
//	exp    expiry, unix seconds; exp-iat must be <= the verifier's MaxTTL
//	jti    unique token id; single-use, enforced by a ReplayGuard
//	req    request binding; see [ReqDigest]
//
// The req binding ties a grant to exactly one method+path+query+body, so a
// captured grant cannot be repurposed for a different mutation even by a caller
// able to read it. The query is folded into the digest only when the request
// carries one (see [ReqDigest]), so a narrow ?delete=true or scope-selector
// variant cannot be reached with a grant minted for the query-less request.
//
// The cid binding ties a grant to exactly one tenant: city names are unique
// per org, not globally, so on a multi-tenant deployment the city claim alone
// would let a grant minted for one org's city replay against another org's
// identically named city. A verifier configured with its own cid rejects any
// grant not minted for it — including every legacy grant, which cannot carry
// a cid.
//
// # Integration
//
// The supervisor/API layer wires this verifier into a request path. When a
// verifying key is configured, an mux-level middleware buffers the request
// body, computes [ReqDigest] from the method, path, query, and body, derives
// the expected city from the {cityName} path segment, and calls
// [Verifier.Verify] to fail closed on a missing or invalid X-GC-City-Write
// header. The gate is per-city: it covers every mutating request to an
// already-registered city under /v0/city/{cityName}, while registry creation
// (POST /v0/city) is carved out because a not-yet-created city has no
// path-resident name to bind a grant to. With no key configured the middleware
// is not installed and mutations follow the prior CSRF/read-only guards.
//
// Because this package mints nothing, the in-tree callers carry no grant: the
// bundled gc API client and dashboard SPA send only the CSRF header. Enabling
// the gate therefore turns their direct city mutations away fail-closed; a
// deployment that gates writes fronts mutations through the external trusted
// authority that mints grants, not through those first-party clients.
package citywriteauth
