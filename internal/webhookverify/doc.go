// Package webhookverify authenticates inbound webhook deliveries for the
// supervisor webhook receiver.
//
// It is pure verification and secret-resolution logic: no HTTP server, no
// config composition, no rule matching, and no dispatch. The receiver (E3)
// owns the mux and calls into this package with the raw request body, the
// request headers, and the operator-resolved secret material; this package
// answers a single question per scheme — "is this delivery authentic?" — and
// surfaces the few fields (event type, delivery id, verified identity) the
// receiver needs downstream.
//
// # Schemes
//
// A [Verifier] is selected by a scheme string via [New]. Five schemes ship:
//
//   - github-hmac-sha256 — X-Hub-Signature-256 HMAC over the raw body.
//   - hmac-sha256        — generic HMAC over the raw body, configurable header.
//   - slack-v0           — Slack v0 HMAC over "v0:{ts}:{body}" with a replay window.
//   - discord-ed25519    — Ed25519 over "{ts}{body}" against the app public key.
//   - jwt-jwks           — a bearer JWT validated against an operator-pinned JWKS.
//
// # Failure model
//
// Verify distinguishes two failure kinds so the receiver can map them to
// different HTTP responses:
//
//   - A returned error means the check could not be performed — an operator
//     wiring or configuration fault (missing/short secret, unreachable JWKS,
//     malformed public key). The receiver should treat this as 503 and refuse.
//   - A VerifyResult with OK==false and a Reason means the check ran and the
//     delivery is not authentic (bad or missing signature, replayed timestamp,
//     wrong issuer/audience, expired token). The receiver should treat this as
//     401 and reject.
//
// # Operator-owned secrets (security review R1)
//
// Secret material is never read from arbitrary process environment. A
// [SecretResolver] enforces that every secret_env lives in the operator
// namespace ([OperatorSecretEnvPrefix]), is actually set, and clears a minimum
// entropy bar — so a pack cannot name an ambient variable like HOME or
// AWS_SECRET_ACCESS_KEY, and a misconfigured empty secret fails closed instead
// of authenticating every caller. For jwt-jwks the trust anchor (issuer,
// audience, JWKS URL) is carried in a separate [JWTVerifierPolicy] that only
// the operator's city.toml populates — the pack-authored WebhookVerify cannot
// supply it, by construction of this package's API.
package webhookverify
