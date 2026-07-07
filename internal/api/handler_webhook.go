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

	"github.com/danielgtaylor/huma/v2"
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
// Flow: resolve webhook (404 if unknown) → R2 perimeter → E8 rate-limit (429) →
// read raw body (capped) → R1 verifier build → verify (E4) → Discord PING→PONG →
// parse + match (E5) → E8 dedup claim → dispatch (E6) via the live E0.5 seam. Every
// accept/reject decision emits a webhook.received / webhook.rejected event (E8).
func (s *Server) handleHookProxy(w http.ResponseWriter, r *http.Request) {
	name := webhookNameFromPath(r.URL.Path)
	if name == "" {
		problemWebhookRouteNotFound.writeTo(w)
		return
	}
	cfg := s.state.Config()
	hook, ok := findWebhook(cfg, name)
	if !ok {
		// Unknown name → 404. Never leak which webhook names exist, and never
		// answer with a 403-plus-detail that would confirm the route. Deliberately
		// NOT evented: the route segment is attacker-chosen and unauthenticated, so
		// emitting here would be an event-log-flood amplifier and a name oracle.
		problemWebhookRouteNotFound.writeTo(w)
		return
	}
	scheme := strings.TrimSpace(hook.Verify.Scheme)

	// Webhooks are POST deliveries only.
	if r.Method != http.MethodPost {
		problemWebhookMethodNotAllowed.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
			Reason: reasonMethodNotAllowed, Status: http.StatusMethodNotAllowed,
		})
		return
	}

	// R2 perimeter. The effective Publication.Visibility was ALREADY capped by
	// E2's pack-guard at config load (public honored only under a city
	// allow_public grant; otherwise tenant). Read the post-guard value — do NOT
	// re-derive trust here.
	visibility := strings.ToLower(strings.TrimSpace(hook.Publication.Visibility))
	if allowed, reason := webhookRequestAllowed(w, visibility, r, s.readOnly); !allowed {
		// webhookRequestAllowed already wrote the response; reason distinguishes a
		// perimeter denial from a read-only refusal.
		s.emitWebhookRejected(WebhookRejectedPayload{Webhook: hook.Name, Scheme: scheme, Reason: reason})
		return
	}

	// E8 rate-limit: per-webhook token bucket on the RESOLVED name, upstream of the
	// expensive body-read + verify. The limit is operator-owned; a pack can only
	// LOWER its own ceiling (clamped in EffectiveRateLimit), never raise it.
	perMinute, burst := cfg.WebhookPolicy.EffectiveRateLimit(hook)
	if ok, retryAfter := s.webhookLimiter.allow(hook.Name, perMinute, burst); !ok {
		setRetryAfter(w, retryAfter)
		problemWebhookRateLimited.writeTo(w)
		// Deliberately NOT evented: this fires on every over-limit request, so on a
		// flood it would be an un-throttled per-request event/log write on a public
		// endpoint — the very amplification the limiter exists to stop. The 429 +
		// Retry-After IS the signal; a persistent flood shows up in ingress metrics.
		// (The other reject paths — perimeter_denied, verify_failed, operator_fault,
		// dispatch_* — stay evented: they are lower-volume and diagnostically useful.)
		return
	}

	// Read the raw body under a hard cap (the signature is computed over it).
	body, err := readCappedBody(w, r, s.maxWebhookBodyBytes())
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			problemWebhookBodyTooLarge.writeTo(w)
			s.emitWebhookRejected(WebhookRejectedPayload{
				Webhook: hook.Name, Scheme: scheme,
				Reason: reasonBodyTooLarge, Status: http.StatusRequestEntityTooLarge,
			})
			return
		}
		problemWebhookBadBody.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
			Reason: reasonBadBody, Status: http.StatusBadRequest,
		})
		return
	}

	// R1: build the verifier with an operator-owned secret / trust anchor.
	verifier, secret, verr := s.buildWebhookVerifier(cfg, hook)
	if verr != nil {
		// Operator fault (secret_env outside GC_WEBHOOK_*, unset, too weak; or a
		// jwt-jwks webhook with no operator [webhooks].jwt_policy; or a scheme
		// construction error) → 503, never 401: the delivery may be perfectly
		// authentic, we simply cannot check it. This is the R1 fail-closed contract.
		log.Printf("api: webhook %q verifier unavailable: %v", hook.Name, verr)
		problemWebhookVerifierUnavailable.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
			Reason: reasonOperatorFault, Status: http.StatusServiceUnavailable, BodySize: len(body),
		})
		return
	}

	vres, verifyErr := verifier.Verify(r.Context(), webhookverify.VerifyRequest{
		Body:   body,
		Header: r.Header,
		Secret: secret,
	})
	if verifyErr != nil {
		// The check could not be performed (operator fault, e.g. malformed key).
		log.Printf("api: webhook %q verify error: %v", hook.Name, verifyErr)
		problemWebhookVerifierUnavailable.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
			Reason: reasonOperatorFault, Status: http.StatusServiceUnavailable, BodySize: len(body),
		})
		return
	}
	if !vres.OK {
		problemWebhookUnauthorized.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
			Reason: reasonVerifyFailed, Status: http.StatusUnauthorized,
			EventType: vres.EventType, BodySize: len(body),
		})
		return
	}

	// Discord PING (interaction type 1) on a VERIFIED payload → PONG, no dispatch.
	// Ordered after verification so a forged type=1 body cannot elicit a PONG. A
	// protocol handshake, not a delivery, so it is neither deduped nor evented.
	if strings.EqualFold(scheme, "discord-ed25519") && isDiscordPing(body) {
		writeJSONBytes(w, http.StatusOK, discordPongBody)
		return
	}

	parsed, perr := webhookmatch.ParseBody(body)
	if perr != nil {
		// Authentic sender, malformed payload → 400.
		problemWebhookBadPayload.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
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
	}, hook.Rules)
	if merr != nil {
		// Structural arg-extraction failure on a matched rule (misconfiguration).
		log.Printf("api: webhook %q match error: %v", hook.Name, merr)
		problemInternalServerError.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
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
			Webhook: hook.Name, Scheme: scheme, EventType: vres.EventType,
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
	dedupKey := webhookDedupKeyFor(hook.Name, vres, body)
	if s.webhookDedup.seen(dedupKey) {
		// Duplicate: ack 2xx so the sender stops retrying, but do NOT dispatch.
		s.emitWebhookReceived(WebhookReceivedPayload{
			Webhook: hook.Name, Scheme: scheme, EventType: vres.EventType,
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
			Webhook: hook.Name, Scheme: scheme,
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
	}, webhookScopeFor(hook), match)
	if rerr != nil {
		s.webhookDedup.forget(dedupKey) // genuine failure: allow the sender's retry
		log.Printf("api: webhook %q dispatch failed: %v", hook.Name, rerr)
		problemWebhookDispatchUnavailable.writeTo(w)
		s.emitWebhookRejected(WebhookRejectedPayload{
			Webhook: hook.Name, Scheme: scheme,
			Reason: reasonDispatchError, Status: http.StatusServiceUnavailable,
			EventType: vres.EventType, DedupID: eventDedupID, BodySize: len(body),
		})
		return
	}

	if result.Dispatched {
		s.emitWebhookReceived(WebhookReceivedPayload{
			Webhook: hook.Name, Scheme: scheme, EventType: vres.EventType,
			DedupID: eventDedupID, Deduped: false, Matched: true, Dispatched: true,
			RuleIndex: match.RuleIndex, Order: match.Order, Rig: match.Rig,
			ScopedName: result.Dispatch.ScopedName, TrackingID: result.Dispatch.TrackingID,
			BodySize: len(body),
		})
		writeJSONBytes(w, http.StatusAccepted, webhookAcceptedBody)
		return
	}
	// Refused by a sink guard (rig scope, trigger!=webhook, missing required param,
	// conversation sink not yet wired). Deterministic, so release the dedup claim:
	// the sender's non-2xx retry should get an honest 422, not a masked 2xx dedup.
	// The detailed reason names an order/rig/param — safe to log, but the wire body
	// AND the event stay generic (reason=dispatch_refused) so the public edge learns
	// nothing about the city's order catalog.
	s.webhookDedup.forget(dedupKey)
	log.Printf("api: webhook %q refused: %s", hook.Name, result.Reason)
	s.emitWebhookRejected(WebhookRejectedPayload{
		Webhook: hook.Name, Scheme: scheme,
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
// Returns (true, "") to proceed; on false it has already written the rejection and
// returns the reason enum (reasonPerimeterDenied or reasonReadOnly) for the event.
func webhookRequestAllowed(w http.ResponseWriter, visibility string, r *http.Request, apiReadOnly bool) (bool, string) {
	public := visibility == "public"
	if !public {
		internalProxyRequest := r.Header.Get("X-GC-Request") != ""
		if !isLoopbackRemoteAddr(r.RemoteAddr) && !internalProxyRequest {
			problemWebhookRouteNotFound.writeTo(w)
			return false, reasonPerimeterDenied
		}
	}
	if apiReadOnly {
		problemWebhookReadOnly.writeTo(w)
		return false, reasonReadOnly
	}
	return true, ""
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

// webhookScopeFor builds the E6 dispatch scope from a matched webhook. config.Webhook
// carries no rig binding today, so a rig-scoped webhook fails closed in the sink's
// R4 scoping (it declares no rig); city-scoped webhooks let the rule's own rig stand.
func webhookScopeFor(w config.Webhook) webhooksink.WebhookScope {
	return webhooksink.WebhookScope{
		Name:      w.Name,
		Scope:     w.ScopeOrDefault(),
		SourceDir: w.SourceDir,
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
	Status int `json:"-"`
	Body   struct {
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
		return nil, huma.Error404NotFound("not_found: order not found: " + input.Name)
	}
	// Refuse non-webhook-trigger orders up front for a clear 422 (the sink also
	// enforces this — defense in depth).
	if strings.TrimSpace(order.Trigger) != "webhook" {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf(
			"order %q has trigger %q; the run endpoint fires only trigger=\"webhook\" orders",
			order.ScopedName(), order.Trigger))
	}
	dispatcher := webhookDispatcherFor(s.state)
	if dispatcher == nil {
		return nil, huma.Error503ServiceUnavailable("webhook dispatch is not available for this city")
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
		return nil, huma.Error503ServiceUnavailable("dispatch failed: " + err.Error())
	}
	if !result.Dispatched {
		return nil, huma.Error422UnprocessableEntity("rejected: " + result.Reason)
	}
	out := &OrderRunOutput{Status: http.StatusAccepted}
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
	problemWebhookVerifierUnavailable = problemBody{
		status: http.StatusServiceUnavailable,
		body:   []byte(`{"status":503,"title":"Service Unavailable","detail":"webhook verifier unavailable"}`),
	}
	problemWebhookDispatchUnavailable = problemBody{
		status: http.StatusServiceUnavailable,
		body:   []byte(`{"status":503,"title":"Service Unavailable","detail":"webhook dispatch unavailable"}`),
	}
)
