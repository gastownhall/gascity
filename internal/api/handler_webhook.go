package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/webhookmatch"
	"github.com/gastownhall/gascity/internal/webhooksink"
	"github.com/gastownhall/gascity/internal/webhookverify"
)

// defaultMaxWebhookBodyBytes caps the raw body the receiver buffers to compute
// the signature and parse the payload, so an unauthenticated caller cannot
// exhaust memory by streaming an unbounded body before verification. The raw
// bytes are load-bearing: HMAC/ed25519 sign the exact body, so it must be read
// whole rather than streamed. 5 MiB comfortably covers GitHub's large PR/issue
// payloads while staying well under GitHub's own 25 MiB delivery ceiling.
const defaultMaxWebhookBodyBytes int64 = 5 << 20

// webhookRequest carries the resolved receiver context for one /hook/ delivery,
// threaded through the receiver's stages so each stage stays a small, focused
// function (keeping handleHookProxy's complexity low).
type webhookRequest struct {
	hook   config.Webhook
	cfg    *config.City
	scheme string
}

// handleHookProxy is the raw /hook/{name} receiver — the fourth sanctioned
// non-Huma surface (alongside /svc/*), mounted on the per-city Server.mux so the
// HMAC/ed25519 verifiers see the exact raw body. It deliberately sits OUTSIDE the
// typed Huma control plane and its CSRF/read-only middleware; the R2 perimeter
// (webhookRequestAllowed) and E4 signature verification are the gates here.
//
// The route is NOT exempt from the mux-level write-auth grant (cityScopedObjectMutation
// keeps /hook/ gated — the deliberate H2 reversal): signature verification is an
// ADDITIONAL gate for public webhooks, never a replacement for the operator's grant
// when write-auth is configured.
//
// Flow (split into stages): resolve webhook (404 if unknown) → admit (R2
// perimeter → POST-only → allowed_cidrs → bearer_env → E8 rate-limit) → read raw
// body (capped) → verify (R1 build → E4 verify → Discord PING→PONG) → dispatch
// (parse + match → dedup → E6 sink). The pre-verification reject paths that an
// unauthenticated caller fully controls (unknown name, perimeter, method,
// source/bearer denial, rate-limit) are NON-evented so a flood cannot amplify
// into per-request event/log writes; so are the pre-limiter access-gate
// operator-fault 503s (allowed_cidrs/bearer_env misconfig), which are logged
// one-shot instead. The verify/dispatch decisions past the limiter — including the
// verifier operator-fault 503 the limiter throttles — stay evented. The access
// gates sit BEFORE the limiter so a disallowed caller cannot drain the shared
// delivery bucket.
func (s *Server) handleHookProxy(w http.ResponseWriter, r *http.Request) {
	req, ok := s.resolveWebhookRequest(w, r)
	if !ok {
		return
	}
	if !s.admitWebhookRequest(w, r, req) {
		return
	}
	body, ok := s.readWebhookBody(w, r, req)
	if !ok {
		return
	}
	vres, ok := s.verifyWebhook(w, r, req, body)
	if !ok {
		return
	}
	s.dispatchWebhook(w, r, req, body, vres)
}

// resolveWebhookRequest resolves the {name} segment to a configured webhook.
// An empty or unknown name → 404, deliberately NOT evented: the route segment is
// attacker-chosen and unauthenticated, so emitting would be an event-log-flood
// amplifier and a name-existence oracle.
func (s *Server) resolveWebhookRequest(w http.ResponseWriter, r *http.Request) (webhookRequest, bool) {
	name := webhookNameFromPath(r.URL.Path)
	if name == "" {
		problemWebhookRouteNotFound.writeTo(w)
		return webhookRequest{}, false
	}
	cfg := s.state.Config()
	hook, ok := findWebhook(cfg, name)
	if !ok {
		problemWebhookRouteNotFound.writeTo(w)
		return webhookRequest{}, false
	}
	return webhookRequest{hook: hook, cfg: cfg, scheme: strings.TrimSpace(hook.Verify.Scheme)}, true
}

// admitWebhookRequest runs the cheap pre-verification gates in the order that
// closes the amplification/existence-leak findings AND keeps a disallowed caller
// off the shared per-hook delivery bucket: the R2 perimeter FIRST (so a
// private/tenant probe gets the same 404 as an unknown route, never a 405 that
// confirms existence), then POST-only, then the operator-owned source and bearer
// gates, and ONLY THEN the E8 rate limiter. Running the access gates before the
// limiter is load-bearing: an off-network or unauthenticated flood is rejected
// without consuming a delivery token, so it cannot drain the bucket that
// legitimate provider deliveries draw from and force them into 429s. Every gate
// here is non-evented — each is a cheap, unauthenticated, attacker-fully-controlled
// reject, so eventing it would be the per-request amplification the limiter exists
// to stop (an operator misconfiguration surfaced by the access gates is the lone
// evented exception, a 503). It returns false when it has already written the
// response.
func (s *Server) admitWebhookRequest(w http.ResponseWriter, r *http.Request, req webhookRequest) bool {
	// R2 perimeter on the EFFECTIVE (post pack-guard) visibility. Non-evented: the
	// private/tenant 404 must be as quiet as an unknown-route 404.
	visibility := strings.ToLower(strings.TrimSpace(req.hook.Publication.Visibility))
	if !webhookRequestAllowed(w, visibility, r, s.readOnly) {
		return false
	}
	// POST-only, right after the perimeter so a non-POST probe of a private/tenant
	// hook already got the existence-hiding 404. Cheap, non-evented.
	if r.Method != http.MethodPost {
		problemWebhookMethodNotAllowed.writeTo(w)
		return false
	}
	// Operator-owned access controls, enforced fail-closed BEFORE the limiter so a
	// disallowed source/bearer neither consumes a delivery token nor reaches the
	// body read and signature verify. Their attacker-controlled denials are
	// non-evented; only an operator misconfiguration (503) events.
	if !s.webhookSourceAllowed(w, r, req) {
		return false
	}
	if !s.webhookBearerAllowed(w, r, req) {
		return false
	}
	// E8 rate-limit on the RESOLVED name, LAST in admit so only access-passing
	// requests consume the operator-owned per-hook delivery bucket, and still
	// upstream of the expensive body read + signature verify it exists to throttle.
	// Non-evented: eventing here would be the per-request amplification the limiter
	// stops. A pack can only LOWER its own ceiling (EffectiveRateLimit), never raise it.
	perMinute, burst := req.cfg.WebhookPolicy.EffectiveRateLimit(req.hook)
	if ok, retryAfter := s.webhookLimiter.allow(req.hook.Name, perMinute, burst); !ok {
		setRetryAfter(w, retryAfter)
		problemWebhookRateLimited.writeTo(w)
		return false
	}
	return true
}

// readWebhookBody reads the raw body under a hard cap (the signature is computed
// over it, so it must be buffered whole). A too-large/unreadable body is evented
// (it is past the limiter, so bounded, and diagnostically useful).
func (s *Server) readWebhookBody(w http.ResponseWriter, r *http.Request, req webhookRequest) ([]byte, bool) {
	body, err := readCappedBody(w, r, s.maxWebhookBodyBytes())
	if err == nil {
		return body, true
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		problemWebhookBodyTooLarge.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme,
			Reason: reasonBodyTooLarge, Status: http.StatusRequestEntityTooLarge,
		})
		return nil, false
	}
	problemWebhookBadBody.writeTo(w)
	s.emitWebhookRejected(WebhookRejectedPayload{
		Webhook: req.hook.Name, Scheme: req.scheme,
		Reason: reasonBadBody, Status: http.StatusBadRequest,
	})
	return nil, false
}

// verifyWebhook builds the R1 verifier and runs the E4 signature check, then
// short-circuits a verified Discord PING to a PONG. It returns ok=false (response
// already written) on an operator fault (503), a failed verification (401), or a
// handled PING; otherwise it returns the verified result.
func (s *Server) verifyWebhook(w http.ResponseWriter, r *http.Request, req webhookRequest, body []byte) (webhookverify.VerifyResult, bool) {
	verifier, secret, verr := s.buildWebhookVerifier(req.cfg, req.hook)
	if verr != nil {
		// Operator fault (secret_env outside GC_WEBHOOK_*, unset, too weak; or a
		// jwt-jwks webhook with no operator [webhooks].jwt_policy; or a scheme
		// construction error) → 503, never 401: the delivery may be perfectly
		// authentic, we simply cannot check it. This is the R1 fail-closed contract.
		log.Printf("api: webhook %q verifier unavailable: %v", req.hook.Name, verr)
		s.rejectWebhookOperatorFault(w, req, len(body))
		return webhookverify.VerifyResult{}, false
	}
	vres, verifyErr := verifier.Verify(r.Context(), webhookverify.VerifyRequest{
		Body:   body,
		Header: r.Header,
		Secret: secret,
	})
	if verifyErr != nil {
		// The check could not be performed (operator fault, e.g. malformed key).
		log.Printf("api: webhook %q verify error: %v", req.hook.Name, verifyErr)
		s.rejectWebhookOperatorFault(w, req, len(body))
		return webhookverify.VerifyResult{}, false
	}
	if !vres.OK {
		problemWebhookUnauthorized.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme,
			Reason: reasonVerifyFailed, Status: http.StatusUnauthorized,
			EventType: vres.EventType, BodySize: len(body),
		})
		return webhookverify.VerifyResult{}, false
	}
	// Discord PING (interaction type 1) on a VERIFIED payload → PONG, no dispatch.
	// Ordered after verification so a forged type=1 body cannot elicit a PONG. A
	// protocol handshake, not a delivery, so it is neither deduped nor evented.
	if strings.EqualFold(req.scheme, "discord-ed25519") && isDiscordPing(body) {
		writeJSONBytes(w, http.StatusOK, discordPongBody)
		return webhookverify.VerifyResult{}, false
	}
	return vres, true
}

// rejectWebhookOperatorFault writes the shared 503 verifier-unavailable response
// and emits the operator_fault rejection event. It is the POST-limiter fault path
// (verifier unavailable), so the delivery limiter already throttles a flood; the
// per-request event is bounded and diagnostically useful. The pre-limiter access
// gates use rejectWebhookAccessOperatorFault instead, which must not amplify.
func (s *Server) rejectWebhookOperatorFault(w http.ResponseWriter, req webhookRequest, bodySize int) {
	problemWebhookVerifierUnavailable.writeTo(w)
	s.emitWebhookRejected(WebhookRejectedPayload{
		Webhook: req.hook.Name, Scheme: req.scheme,
		Reason: reasonOperatorFault, Status: http.StatusServiceUnavailable, BodySize: bodySize,
	})
}

// rejectWebhookAccessOperatorFault writes the shared 503 operator-fault response
// for a PRE-LIMITER access gate — a misconfigured allowed_cidrs, or an unset/empty
// bearer_env on a hook that still passed config load. Unlike the post-limiter
// verifier fault above, these gates run BEFORE the delivery limiter, so an
// attacker flooding a misconfigured public hook could amplify the fault into
// unbounded per-request event/log writes (CWE-400). This path is therefore
// deliberately NON-EVENTED and its diagnostic log is one-shot per (hook, fault):
// the 503 status — still returned per request, as cheap as the other pre-limiter
// rejects — plus ingress metrics are the flood-proof operator signal, and the
// latched log names the broken hook once. faultDetail identifies the specific
// misconfiguration so a later, different fault reports again.
func (s *Server) rejectWebhookAccessOperatorFault(w http.ResponseWriter, hookName, faultDetail string) {
	problemWebhookVerifierUnavailable.writeTo(w)
	if s.webhookAccessFaultFirstSeen(hookName, faultDetail) {
		log.Printf("api: webhook %q %s", hookName, faultDetail)
	}
}

// webhookAccessFaultFirstSeen reports whether the (hook, fault) pair has not been
// reported yet, latching it so a flood reports the fault once instead of once per
// request. The key derives from the webhook name and the operator-owned
// misconfiguration, never attacker input, so the latch set stays bounded by config.
func (s *Server) webhookAccessFaultFirstSeen(hookName, faultDetail string) bool {
	key := hookName + "\x00" + faultDetail
	s.webhookAccessFaultMu.Lock()
	defer s.webhookAccessFaultMu.Unlock()
	if s.webhookAccessFaultLogged == nil {
		s.webhookAccessFaultLogged = make(map[string]struct{})
	}
	if _, seen := s.webhookAccessFaultLogged[key]; seen {
		return false
	}
	s.webhookAccessFaultLogged[key] = struct{}{}
	return true
}

// dispatchWebhook parses + matches the verified delivery, claims dedup, and
// routes a matched rule to the E6 sink. It owns every post-verification response.
func (s *Server) dispatchWebhook(w http.ResponseWriter, r *http.Request, req webhookRequest, body []byte, vres webhookverify.VerifyResult) {
	parsed, perr := webhookmatch.ParseBody(body)
	if perr != nil {
		// Authentic sender, malformed payload → 400.
		problemWebhookBadPayload.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme,
			Reason: reasonBadPayload, Status: http.StatusBadRequest,
			EventType: vres.EventType, BodySize: len(body),
		})
		return
	}

	match, matched, merr := webhookmatch.Match(webhookmatch.MatchInput{
		EventType: vres.EventType,
		DedupID:   vres.DedupID,
		Identity:  vres.Identity,
		Body:      parsed,
	}, req.hook.Rules)
	if merr != nil {
		// Structural arg-extraction failure on a matched rule (misconfiguration).
		log.Printf("api: webhook %q match error: %v", req.hook.Name, merr)
		problemInternalServerError.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme,
			Reason: reasonMatchError, Status: http.StatusInternalServerError,
			EventType: vres.EventType, BodySize: len(body),
		})
		return
	}
	if !matched {
		// A valid, authentic delivery that no rule wanted. Webhook senders retry on
		// non-2xx, so a valid-but-unmatched delivery is a 2xx no-op — never a 4xx —
		// but it IS an accepted delivery, so it is evented as webhook.received.
		s.emitWebhookReceived(WebhookReceivedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme, EventType: vres.EventType,
			DedupID: vres.DedupID, Matched: false, Dispatched: false,
			RuleIndex: -1, BodySize: len(body),
		})
		writeJSONBytes(w, http.StatusOK, webhookNoMatchBody)
		return
	}

	// E8 dedup: claim (webhook, delivery) so a provider retry (GitHub/Slack/
	// Discord all re-deliver) cannot fire the order twice. The KEY is derived from
	// content the signature COVERS (a signed jti, else the body hash) — never from
	// the unsigned/coarse provider delivery id, which an attacker could mutate to
	// mint a fresh key (github) or which could silently collide two distinct
	// deliveries in one second (slack). See webhookDedupKeyFor.
	//
	// eventDedupID keeps the provider's surfaced id on the emitted event for
	// observability (the github delivery id / slack ts is still useful to log),
	// falling back to the body hash only when the scheme surfaces none.
	eventDedupID := strings.TrimSpace(vres.DedupID)
	if eventDedupID == "" {
		eventDedupID = webhookBodyHash(body)
	}
	dedupKey := webhookDedupKeyFor(req.hook.Name, vres, body)
	if s.webhookDedup.seen(dedupKey) {
		// Duplicate: ack 2xx so the sender stops retrying, but do NOT dispatch.
		s.emitWebhookReceived(WebhookReceivedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme, EventType: vres.EventType,
			DedupID: eventDedupID, Deduped: true, Matched: true, Dispatched: false,
			RuleIndex: match.RuleIndex, Order: match.Order, Rig: match.Rig, BodySize: len(body),
		})
		writeJSONBytes(w, http.StatusOK, webhookDuplicateBody)
		return
	}

	// Dispatch through the E6 sink + the live E0.5 dispatcher seam.
	dispatcher := webhookDispatcherFor(s.state)
	if dispatcher == nil {
		s.webhookDedup.forget(dedupKey) // never acted on: let the sender retry
		problemWebhookDispatchUnavailable.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme,
			Reason: reasonDispatchUnavailable, Status: http.StatusServiceUnavailable,
			EventType: vres.EventType, DedupID: eventDedupID, BodySize: len(body),
		})
		return
	}
	// Detach cancellation from the request: the design's fast-ACK contract runs
	// the order async after the HTTP response, and dispatchOne bounds itself with
	// the order's own timeout. WithoutCancel keeps request values (trace) but not
	// the request's Done channel, so writing the response cannot kill the order.
	result, rerr := webhooksink.Route(context.WithoutCancel(r.Context()), webhooksink.Deps{
		Dispatcher:   dispatcher,
		ResolveOrder: orderResolverFor(s.state),
	}, webhookScopeFor(req.hook), match)
	if rerr != nil {
		s.webhookDedup.forget(dedupKey) // genuine failure: allow the sender's retry
		log.Printf("api: webhook %q dispatch failed: %v", req.hook.Name, rerr)
		problemWebhookDispatchUnavailable.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme,
			Reason: reasonDispatchError, Status: http.StatusServiceUnavailable,
			EventType: vres.EventType, DedupID: eventDedupID, BodySize: len(body),
		})
		return
	}

	if result.Dispatched {
		s.emitWebhookReceived(WebhookReceivedPayload{
			Webhook: req.hook.Name, Scheme: req.scheme, EventType: vres.EventType,
			DedupID: eventDedupID, Deduped: false, Matched: true, Dispatched: true,
			RuleIndex: match.RuleIndex, Order: match.Order, Rig: match.Rig,
			ScopedName: result.Dispatch.ScopedName, TrackingID: result.Dispatch.TrackingID,
			BodySize: len(body),
		})
		writeJSONBytes(w, http.StatusAccepted, webhookAcceptedBody)
		return
	}
	// Refused by a sink guard (rig scope, public-hook exec order, trigger!=webhook,
	// missing required param, conversation sink not yet wired). Deterministic, so
	// release the dedup claim: the sender's non-2xx retry should get an honest 422,
	// not a masked 2xx dedup. The detailed reason names an order/rig/param — safe to
	// log, but the wire body AND the event stay generic (reason=dispatch_refused) so
	// the public edge learns nothing about the city's order catalog.
	s.webhookDedup.forget(dedupKey)
	log.Printf("api: webhook %q refused: %s", req.hook.Name, result.Reason)
	s.emitWebhookRejected(WebhookRejectedPayload{
		Webhook: req.hook.Name, Scheme: req.scheme,
		Reason: reasonDispatchRefused, Status: http.StatusUnprocessableEntity,
		EventType: vres.EventType, DedupID: eventDedupID, BodySize: len(body),
	})
	writeJSONBytes(w, http.StatusUnprocessableEntity, webhookRejectedBody)
}

// maxWebhookBodyBytes returns the configured body cap, defaulting when unset.
func (s *Server) maxWebhookBodyBytes() int64 {
	if s.webhookMaxBody > 0 {
		return s.webhookMaxBody
	}
	return defaultMaxWebhookBodyBytes
}

// webhookNameFromPath extracts the {name} segment from a /hook/{name}/... path.
// Mirrors serviceNameFromPath.
func webhookNameFromPath(path string) string {
	path = strings.TrimPrefix(path, "/hook/")
	if path == "" {
		return ""
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// findWebhook returns the configured webhook with the given name.
func findWebhook(cfg *config.City, name string) (config.Webhook, bool) {
	if cfg == nil {
		return config.Webhook{}, false
	}
	for _, w := range cfg.Webhooks {
		if w.Name == name {
			return w, true
		}
	}
	return config.Webhook{}, false
}

// webhookRequestAllowed is the R2 mux-enforced, visibility-aware perimeter — the
// webhook analog of serviceRequestAllowed. It runs on every /hook/ delivery
// before the body is read or the signature is checked.
//
//   - private / tenant webhook: require the /svc-style loopback-or-X-GC-Request
//     internal-origin check. A non-loopback external POST → 404, so an external
//     caller never learns a private/tenant hook exists. (Unlike serviceRequestAllowed,
//     a loopback/internal POST is NOT additionally gated on the CSRF header: the
//     E4 signature verify is the anti-forgery gate for webhooks, and a loopback
//     delivery must be able to proceed to that verification.)
//   - public webhook (survived E2's allow_public grant): MAY accept edge
//     (non-loopback) ingress; the E4 signature verify is the ingress auth.
//
// Dispatch is a mutation, so read-only mode refuses every visibility. The
// perimeter/loopback check runs FIRST for private/tenant so an external probe
// gets a 404 (not a read-only 403 that would confirm the route exists); a public
// route's existence is already known, so a read-only 403 there leaks nothing.
//
// It returns true to proceed; on false it has already written the rejection.
// These denials are DELIBERATELY NOT evented (the caller does not emit): they are
// cheap, unauthenticated, attacker-fully-controlled reject paths, so eventing
// them would be the same event-log-flood amplifier and existence oracle that
// keeps an unknown-name 404 non-evented.
func webhookRequestAllowed(w http.ResponseWriter, visibility string, r *http.Request, apiReadOnly bool) bool {
	public := visibility == "public"
	if !public {
		internalProxyRequest := r.Header.Get("X-GC-Request") != ""
		if !isLoopbackRemoteAddr(r.RemoteAddr) && !internalProxyRequest {
			problemWebhookRouteNotFound.writeTo(w)
			return false
		}
	}
	if apiReadOnly {
		problemWebhookReadOnly.writeTo(w)
		return false
	}
	return true
}

// buildWebhookVerifier constructs the E4 verifier for a hook with an
// operator-owned secret (R1). For the HMAC family and discord-ed25519 the secret
// is resolved through webhookverify.SecretResolver, which enforces the
// GC_WEBHOOK_* operator namespace, present-not-empty, and an entropy floor. For
// jwt-jwks the trust anchor comes from the operator-owned [webhooks].jwt_policy in
// city.toml — never from the pack-authored [webhook.verify] — so a pack cannot
// point trust at an attacker issuer/JWKS. Any failure is an operator fault the
// caller maps to 503, never 401.
func (s *Server) buildWebhookVerifier(cfg *config.City, hook config.Webhook) (webhookverify.Verifier, []byte, error) {
	scheme := strings.TrimSpace(hook.Verify.Scheme)
	var opts webhookverify.Options
	var secret []byte
	if scheme == "jwt-jwks" {
		if cfg == nil {
			return nil, nil, fmt.Errorf("no operator [webhooks].jwt_policy for jwt-jwks webhook %q", hook.Name)
		}
		policy, ok := cfg.WebhookPolicy.OperatorJWTPolicy(hook.Name)
		if !ok {
			return nil, nil, fmt.Errorf("no operator [webhooks].jwt_policy for jwt-jwks webhook %q", hook.Name)
		}
		opts.JWTPolicy = &webhookverify.JWTVerifierPolicy{
			Issuer:   policy.Issuer,
			Audience: policy.Audience,
			JWKSURL:  policy.JWKSURL,
		}
	} else {
		sec, err := webhookverify.NewSecretResolver().Resolve(hook.Verify)
		if err != nil {
			return nil, nil, fmt.Errorf("webhook %q secret: %w", hook.Name, err)
		}
		secret = sec
	}
	// Reuse a memoized verifier when the config fingerprint is unchanged, so the
	// jwt-jwks JWKS cache (the only stateful part) persists across deliveries
	// instead of being rebuilt — and its JWKS refetched, blocking, pre-signature —
	// on every request. Secret resolution above stays per-request (cheap env read);
	// only the verifier is cached. Non-jwt verifiers are stateless, so memoizing
	// them is harmless.
	v, err := s.cachedWebhookVerifier(scheme, hook, opts)
	if err != nil {
		return nil, nil, err
	}
	return v, secret, nil
}

// cachedWebhookVerifier returns the memoized verifier for hook, rebuilding it
// when no entry exists or the config fingerprint changed (a hot-reload). The
// build runs under the mutex; for jwt-jwks that is a cheap allocation (the JWKS
// fetch is lazy, on first Verify), so no network call happens while locked.
func (s *Server) cachedWebhookVerifier(scheme string, hook config.Webhook, opts webhookverify.Options) (webhookverify.Verifier, error) {
	fp := webhookVerifierFingerprint(hook, opts)
	s.webhookVerifiersMu.Lock()
	defer s.webhookVerifiersMu.Unlock()
	if s.webhookVerifiers == nil {
		s.webhookVerifiers = make(map[string]cachedWebhookVerifier)
	}
	if c, ok := s.webhookVerifiers[hook.Name]; ok && c.fingerprint == fp {
		return c.verifier, nil
	}
	v, err := webhookverify.New(scheme, hook.Verify, opts)
	if err != nil {
		return nil, err
	}
	s.webhookVerifiers[hook.Name] = cachedWebhookVerifier{verifier: v, fingerprint: fp}
	return v, nil
}

// webhookVerifierFingerprint is a cheap identity for the security-relevant
// verifier inputs, so a config hot-reload that changes any of them rebuilds the
// verifier (and drops the stale JWKS cache). It covers the verify scheme, secret
// env/slot, the scheme header overrides, the replay window, and the operator jwt
// trust anchor (issuer/audience/jwks_url) carried in opts.
func webhookVerifierFingerprint(hook config.Webhook, opts webhookverify.Options) string {
	v := hook.Verify
	var iss, aud, jwks string
	if opts.JWTPolicy != nil {
		iss, aud, jwks = opts.JWTPolicy.Issuer, opts.JWTPolicy.Audience, opts.JWTPolicy.JWKSURL
	}
	return strings.Join([]string{
		v.Scheme, v.SecretEnv, v.SecretKey,
		v.SignatureHeader, v.EventHeader, v.DedupHeader, v.TimestampHeader,
		v.ReplayWindow, iss, aud, jwks,
	}, "\x00")
}

// webhookScopeFor builds the E6 dispatch scope from a matched webhook. It carries
// the webhook's authoritative rig binding (so a rig-scoped webhook dispatches to
// its own rig and refuses foreign rigs, R4) and its EFFECTIVE (post pack-guard)
// publication visibility (so the sink refuses to let a public hook reach the exec
// sink, R4). A city-scoped webhook lets the rule's own rig stand.
func webhookScopeFor(w config.Webhook) webhooksink.WebhookScope {
	return webhooksink.WebhookScope{
		Name:       w.Name,
		Scope:      w.ScopeOrDefault(),
		Rig:        strings.TrimSpace(w.Rig),
		Visibility: strings.ToLower(strings.TrimSpace(w.Publication.Visibility)),
		SourceDir:  w.SourceDir,
	}
}

// webhookDispatcherFor returns the live order dispatcher when the State exposes
// one (the H1/E0.5 seam), or nil when webhook dispatch is unavailable.
func webhookDispatcherFor(state State) orderdispatch.Dispatcher {
	if p, ok := state.(WebhookDispatchProvider); ok {
		return p.WebhookDispatcher()
	}
	return nil
}

// orderResolverFor backs webhooksink.OrderResolver with the city's active
// (enabled) scanned order set, matching by name and effective rig. It is the E6
// resolver the sink uses to turn a matched rule's {order, rig} into a resolved
// order, which the sink then re-checks (trigger, required params) before firing.
func orderResolverFor(state State) webhooksink.OrderResolver {
	return func(name, rig string) (orders.Order, bool) {
		rig = strings.TrimSpace(rig)
		for _, o := range state.Orders() {
			if o.Name == name && strings.TrimSpace(o.Rig) == rig {
				return o, true
			}
		}
		return orders.Order{}, false
	}
}

// isDiscordPing reports whether a Discord interaction body is a PING (type 1).
// json.Number tolerates the numeric type field without a float round-trip.
func isDiscordPing(body []byte) bool {
	var probe struct {
		Type json.Number `json:"type"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Type.String() == "1"
}

// --- Typed operator run route: POST /v0/city/{cityName}/order/{name}/run ---

// OrderRunInput is the Huma input for POST /v0/city/{cityName}/order/{name}/run.
type OrderRunInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name of a trigger=\"webhook\" order."`
	Body struct {
		Vars map[string]string `json:"vars,omitempty" doc:"Declared [order.params] as key/value dispatch args (parity with 'gc order run --var'). Namespaced into the exec env under GC_WEBHOOK_ARG_ before overlay (R4)."`
	}
}

// OrderRunOutput is the response for POST /v0/city/{cityName}/order/{name}/run.
type OrderRunOutput struct {
	Status   int    `json:"-"`
	Location string `header:"Location" doc:"Runs-list URL. An order dispatches asynchronously, so no single run root is known at response time; the dispatched run appears in the list once it materializes."`
	Body     struct {
		Status     string `json:"status" doc:"\"dispatched\" when the order fired."`
		ScopedName string `json:"scoped_name,omitempty" doc:"Rig-qualified name of the fired order."`
		TrackingID string `json:"tracking_id,omitempty" doc:"Tracking bead id for the dispatch."`
	}
}

// humaHandleOrderRun is the internal/operator path to fire a trigger="webhook"
// order directly with typed params — no signature, because this route lives on
// the typed Huma surface where write-auth/CSRF/read-only IS the auth. It reuses
// the E6 sink + E0.5 dispatcher so every guard (trigger opt-in, required-param
// validation, R4 exec-env namespacing) is the same code the raw /hook/ path runs.
func (s *Server) humaHandleOrderRun(ctx context.Context, input *OrderRunInput) (*OrderRunOutput, error) {
	order, ok := resolveWebhookOrder(s.state, input.Name)
	if !ok {
		return nil, apierr.OrderNotFound.Msg("not_found: order not found: " + input.Name)
	}
	// Refuse non-webhook-trigger orders up front for a clear 422 (the sink also
	// enforces this — defense in depth).
	if strings.TrimSpace(order.Trigger) != "webhook" {
		return nil, apierr.WebhookRejected.Msg(fmt.Sprintf(
			"order %q has trigger %q; the run endpoint fires only trigger=\"webhook\" orders",
			order.ScopedName(), order.Trigger))
	}
	dispatcher := webhookDispatcherFor(s.state)
	if dispatcher == nil {
		return nil, apierr.ServiceUnavailable.Msg("webhook dispatch is not available for this city")
	}
	result, err := webhooksink.Route(context.WithoutCancel(ctx), webhooksink.Deps{
		Dispatcher:   dispatcher,
		ResolveOrder: orderResolverFor(s.state),
	}, webhooksink.WebhookScope{Name: "order-run", Scope: "city"}, webhookmatch.MatchResult{
		Target: "order",
		Order:  order.Name,
		Rig:    order.Rig,
		Vars:   input.Body.Vars,
	})
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("dispatch failed: " + err.Error())
	}
	if !result.Dispatched {
		return nil, apierr.WebhookRejected.Msg("rejected: " + result.Reason)
	}
	out := &OrderRunOutput{Status: http.StatusAccepted, Location: runsListPath(input.CityName)}
	out.Body.Status = "dispatched"
	out.Body.ScopedName = result.Dispatch.ScopedName
	out.Body.TrackingID = result.Dispatch.TrackingID
	return out, nil
}

// resolveWebhookOrder finds an active order by its plain or rig-scoped name.
func resolveWebhookOrder(state State, name string) (orders.Order, bool) {
	name = strings.TrimSpace(name)
	for _, o := range state.Orders() {
		if o.Name == name || o.ScopedName() == name {
			return o, true
		}
	}
	return orders.Order{}, false
}

// --- wire helpers + pre-serialized responses ---

func readCappedBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}

func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var (
	// discordPongBody is Discord's required PING acknowledgement (interaction type 1).
	discordPongBody = []byte(`{"type":1}`)
	// webhookNoMatchBody acks a verified-but-unmatched delivery (a 2xx no-op).
	webhookNoMatchBody = []byte(`{"status":"ok","matched":false}`)
	// webhookDuplicateBody acks a deduplicated delivery — 2xx so the sender stops
	// retrying, deduped:true so it can observe the suppression.
	webhookDuplicateBody = []byte(`{"status":"ok","deduped":true}`)
	// webhookAcceptedBody acks a dispatched delivery.
	webhookAcceptedBody = []byte(`{"status":"accepted"}`)
	// webhookRejectedBody is the generic refusal body — deliberately reason-free so
	// the public edge learns nothing about the order catalog (the reason is logged).
	webhookRejectedBody = []byte(`{"status":"rejected"}`)
)

// Pre-serialized RFC 9457 problem responses for the /hook/ perimeter, mirroring
// the /svc problemBody convention (no json.Marshal on the reject path).
var (
	problemWebhookRouteNotFound = problemBody{
		status: http.StatusNotFound,
		body:   []byte(`{"status":404,"title":"Not Found","detail":"not_found: webhook route not found"}`),
	}
	problemWebhookMethodNotAllowed = problemBody{
		status: http.StatusMethodNotAllowed,
		body:   []byte(`{"status":405,"title":"Method Not Allowed","detail":"method_not_allowed: webhook deliveries must be POST"}`),
	}
	problemWebhookReadOnly = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"read_only: webhook dispatch is disabled in read-only mode"}`),
	}
	problemWebhookRateLimited = problemBody{
		status: http.StatusTooManyRequests,
		body:   []byte(`{"status":429,"title":"Too Many Requests","detail":"rate_limited: webhook delivery rate exceeded"}`),
	}
	problemWebhookBodyTooLarge = problemBody{
		status: http.StatusRequestEntityTooLarge,
		body:   []byte(`{"status":413,"title":"Request Entity Too Large","detail":"webhook body exceeds limit"}`),
	}
	problemWebhookBadBody = problemBody{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"could not read webhook body"}`),
	}
	problemWebhookBadPayload = problemBody{
		status: http.StatusBadRequest,
		body:   []byte(`{"status":400,"title":"Bad Request","detail":"webhook payload is not a JSON object"}`),
	}
	problemWebhookUnauthorized = problemBody{
		status: http.StatusUnauthorized,
		body:   []byte(`{"status":401,"title":"Unauthorized","detail":"signature verification failed"}`),
	}
	problemWebhookForbiddenSource = problemBody{
		status: http.StatusForbidden,
		body:   []byte(`{"status":403,"title":"Forbidden","detail":"forbidden: source address is not permitted"}`),
	}
	problemWebhookVerifierUnavailable = problemBody{
		status: http.StatusServiceUnavailable,
		body:   []byte(`{"status":503,"title":"Service Unavailable","detail":"webhook verifier unavailable"}`),
	}
	problemWebhookDispatchUnavailable = problemBody{
		status: http.StatusServiceUnavailable,
		body:   []byte(`{"status":503,"title":"Service Unavailable","detail":"webhook dispatch unavailable"}`),
	}
)
