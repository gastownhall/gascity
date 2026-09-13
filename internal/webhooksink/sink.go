// Package webhooksink routes a verified+matched webhook delivery to its sink.
//
// It is the E6 stage of the supervisor webhook receiver. Given a
// [webhookmatch.MatchResult] (the resolved rule + extracted args from E5) it
// fires one of two sinks:
//
//   - target="order": dispatch order(args) through the shared
//     [orderdispatch.Dispatcher] seam (E0.5), which reuses the controller's
//     dispatchOne core. The order sink is fully wired here.
//   - target="conversation": route the verified payload into the realtime chat
//     path (Slack/Discord → extmsg). Only the seam ([ConversationSink]) lives
//     here; the working implementation is E7, so the default is a clean stub
//     that never touches the order path.
//
// # Guards (order sink)
//
// Before anything fires, routeOrder enforces, in order:
//
//  1. the rule's {order, rig} is within the receiving webhook's provenance scope
//     (R4): a rig-scoped webhook may target only its own rig; a city-scoped
//     webhook may target the city or any rig;
//  2. a public webhook may not fire an exec (sh -c) order (R4) — public
//     deliveries are limited to formula orders so the pack-verified public
//     ingress can never reach the in-process shell-exec sink;
//  3. the resolved order opts in with trigger="webhook" — a webhook may never
//     fire an order that did not declare itself webhook-triggered;
//  4. every declared-required param is present in the extracted args (E1).
//
// # R4 — arg namespacing
//
// The extracted args come from the untrusted payload. Required-param validation
// and the formula ExpandVars channel key on the raw declared param name, so
// [MatchResult.Vars] is passed through as-is for those. For an exec order's
// process environment — the one place an un-namespaced key could shadow a
// controller-owned variable (GC_CITY, BEADS_DIR, …) or a static [order.env]
// entry — the args are routed through [webhookmatch.ExecEnvVars], which prefixes
// every key with GC_WEBHOOK_ARG_. A payload therefore cannot inject a control
// key even if one slipped past the E2 load-time and E5 extraction guards.
package webhooksink

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/webhookmatch"
)

// OrderResolver looks up a resolved order by name and effective rig. rig is
// empty for a city-level order. It returns false when no such order is
// configured. The receiver (E3) backs this with the city's scanned order set.
type OrderResolver func(name, rig string) (orders.Order, bool)

// WebhookScope carries the receiving webhook's provenance and dispatch scope so
// the sink can constrain which {order, rig} a delivery may target (R4). The
// receiver (E3) populates it from the matched config.Webhook.
type WebhookScope struct {
	// Name is the webhook name, used in rejection reasons and audit.
	Name string
	// Scope is the webhook's dispatch scope: "city" or "rig" (empty ⇒ city).
	Scope string
	// Rig is the webhook's own rig when Scope=="rig" (empty for city scope).
	Rig string
	// Visibility is the webhook's EFFECTIVE (post pack-guard) publication
	// visibility: "public", "tenant", or "private". A public webhook's only gate
	// is its pack-authored signature verify, so the sink refuses to let it reach
	// the exec (sh -c) sink (R4); private/tenant hooks are additionally gated by
	// the receiver's internal-origin perimeter and may target exec orders.
	Visibility string
	// SourceDir is the pack/fragment provenance ("" ⇒ operator-authored root).
	// Carried for future content-scoped consent (R3); not consulted by v0 rig
	// scoping, which keys on Scope/Rig.
	SourceDir string
}

// IsPublic reports whether the webhook's effective visibility is public.
func (s WebhookScope) IsPublic() bool {
	return strings.EqualFold(strings.TrimSpace(s.Visibility), "public")
}

// ConversationSink routes a verified conversation-target delivery into the
// realtime chat path (Slack/Discord → extmsg). It is defined here so the order
// sink and the receiver depend on a stable seam; the working implementation is
// E7 (provider-normalizer → extmsg.HandleInboundNormalized).
type ConversationSink interface {
	Deliver(ctx context.Context, match webhookmatch.MatchResult) (Result, error)
}

// Deps are the collaborators the sink needs. Dispatcher fires order-target
// deliveries; ResolveOrder maps a rule's {order, rig} to a configured order;
// Conversation handles conversation-target deliveries (defaults to the stub).
type Deps struct {
	Dispatcher   orderdispatch.Dispatcher
	ResolveOrder OrderResolver
	Conversation ConversationSink
}

// Result is the outcome of routing one verified+matched delivery to its sink.
type Result struct {
	// Target is the sink that handled the delivery ("order" | "conversation").
	Target string
	// Dispatched is true when an order was accepted and launched.
	Dispatched bool
	// Rejected is true when a guard refused the delivery before any side effect.
	Rejected bool
	// Reason explains a rejection or a stubbed sink (empty on a clean dispatch).
	Reason string
	// Dispatch carries the seam result for an order-target delivery.
	Dispatch orderdispatch.DispatchResult
}

// Route sends one verified+matched delivery to the sink its rule selects.
func Route(ctx context.Context, deps Deps, scope WebhookScope, match webhookmatch.MatchResult) (Result, error) {
	switch strings.TrimSpace(strings.ToLower(match.Target)) {
	case "", "order":
		return routeOrder(ctx, deps, scope, match)
	case "conversation":
		return routeConversation(ctx, deps, match)
	default:
		return Result{Target: match.Target, Rejected: true, Reason: fmt.Sprintf("unknown sink target %q", match.Target)}, nil
	}
}

func routeOrder(ctx context.Context, deps Deps, scope WebhookScope, match webhookmatch.MatchResult) (Result, error) {
	res := Result{Target: "order"}

	// (1) R4 rig scoping: constrain the target rig to the webhook's scope.
	effectiveRig, reason := scopedRig(scope, match.Rig)
	if reason != "" {
		res.Rejected = true
		res.Reason = reason
		return res, nil
	}

	if deps.ResolveOrder == nil {
		return res, fmt.Errorf("webhooksink: no order resolver configured")
	}
	a, found := deps.ResolveOrder(match.Order, effectiveRig)
	if !found {
		res.Rejected = true
		res.Reason = fmt.Sprintf("order %q not found for rig %q", match.Order, effectiveRig)
		return res, nil
	}

	// (2) A public webhook may never fire an exec (sh -c) order. A public hook's
	// only gate is its pack-authored signature verify (R1), so reaching the
	// in-process shell-exec sink would preserve the red-team RCE path the design
	// set out to remove; public deliveries are forced through formula orders only.
	// Private/tenant hooks are additionally gated by the receiver's internal-origin
	// perimeter, so they may still target exec orders.
	if scope.IsPublic() && a.IsExec() {
		res.Rejected = true
		res.Reason = fmt.Sprintf("public webhook %q may not fire exec order %q; public deliveries are limited to formula orders", scope.Name, a.ScopedName())
		return res, nil
	}

	// (3) A webhook may fire only orders that explicitly opt in.
	if strings.TrimSpace(a.Trigger) != "webhook" {
		res.Rejected = true
		res.Reason = fmt.Sprintf("order %q has trigger %q; a webhook may only fire trigger=\"webhook\" orders", a.ScopedName(), a.Trigger)
		return res, nil
	}

	// (4) Required-param validation against the RAW extracted args (keyed by the
	// declared param name), before any namespacing.
	if err := orders.ValidateRequiredParams(a, match.Vars); err != nil {
		res.Rejected = true
		res.Reason = err.Error()
		return res, nil
	}

	if deps.Dispatcher == nil {
		return res, fmt.Errorf("webhooksink: no dispatcher configured")
	}

	// R4: raw args feed validation + formula ExpandVars; the exec-env overlay is
	// namespaced so a payload can never shadow a controller/static env key.
	out, err := deps.Dispatcher.Dispatch(ctx, orderdispatch.DispatchRequest{
		Order:   a,
		Vars:    match.Vars,
		ExecEnv: webhookmatch.ExecEnvVars(match.Vars),
		Source:  orderdispatch.SourceWebhook,
	})
	if err != nil {
		return res, err
	}
	res.Dispatch = out
	res.Dispatched = out.Fired
	res.Rejected = out.Rejected
	res.Reason = out.Reason
	return res, nil
}

// scopedRig resolves the effective target rig for a rule and enforces the
// webhook's provenance scope (R4). It returns the effective rig and an empty
// reason on success, or an empty rig and a non-empty rejection reason.
//
// SAFE interpretation (the design's R4 "constrain {order,rig} to the pack
// SourceDir scope" is otherwise ambiguous, so this picks the conservative rule
// and documents it):
//
//   - rig-scoped webhook (Scope=="rig"): may target ONLY its own rig. A rule
//     with no rig inherits the webhook's rig; a rule naming any other rig is
//     refused.
//   - city-scoped webhook (Scope=="city" or empty): may target the city
//     (rule rig empty) or any rig the rule names. A city-scoped webhook is
//     declared at city provenance, so it is not confined to a single rig.
func scopedRig(scope WebhookScope, ruleRig string) (string, string) {
	ruleRig = strings.TrimSpace(ruleRig)
	switch strings.TrimSpace(strings.ToLower(scope.Scope)) {
	case "rig":
		own := strings.TrimSpace(scope.Rig)
		if own == "" {
			return "", fmt.Sprintf("webhook %q is rig-scoped but declares no rig binding", scope.Name)
		}
		if ruleRig == "" || ruleRig == own {
			return own, ""
		}
		return "", fmt.Sprintf("webhook %q is rig-scoped to %q and may not target foreign rig %q", scope.Name, own, ruleRig)
	default:
		// city (or unspecified): the rule's rig stands as-is.
		return ruleRig, ""
	}
}

func routeConversation(ctx context.Context, deps Deps, match webhookmatch.MatchResult) (Result, error) {
	sink := deps.Conversation
	if sink == nil {
		sink = StubConversationSink{}
	}
	return sink.Deliver(ctx, match)
}

// StubConversationSink is the default conversation sink until E7 lands. It never
// touches the order path and always reports that conversation delivery is not
// yet wired, so an order-sink deployment is unaffected by the missing chat path.
type StubConversationSink struct{}

// Deliver reports that the conversation sink is not yet wired.
func (StubConversationSink) Deliver(_ context.Context, _ webhookmatch.MatchResult) (Result, error) {
	// TODO(E7): normalize the verified payload into an
	// extmsg.ExternalInboundMessage (per-provider: Slack url_verification
	// challenge + retry headers, Discord PING→PONG) and route it via
	// extmsg.HandleInboundNormalized so a session/agent inbox receives it, with
	// the provider's fast-ACK reply on the pack's registered HTTPAdapter. Until
	// then this is a clean no-op that keeps the order sink fully functional.
	return Result{Target: "conversation", Rejected: true, Reason: "conversation sink not yet wired (E7)"}, nil
}
