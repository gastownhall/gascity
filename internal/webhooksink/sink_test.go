package webhooksink

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/orderdispatch"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/webhookmatch"
)

// fakeDispatcher records the last request it was handed so tests can assert what
// the sink forwards to the seam, without a live city.
type fakeDispatcher struct {
	calls int
	last  orderdispatch.DispatchRequest
	ret   orderdispatch.DispatchResult
	err   error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, req orderdispatch.DispatchRequest) (orderdispatch.DispatchResult, error) {
	f.calls++
	f.last = req
	return f.ret, f.err
}

func resolverFor(a orders.Order) OrderResolver {
	return func(name, rig string) (orders.Order, bool) {
		if name == a.Name && rig == a.Rig {
			return a, true
		}
		return orders.Order{}, false
	}
}

// (a) A matched order-sink delivery fires the seam with the raw args for the
// formula/validation channel, the namespaced args for the exec-env overlay, the
// webhook Source, and the correct order/rig.
func TestRouteOrderFiresSeamWithNamespacedVars(t *testing.T) {
	order := orders.Order{
		Name:    "pr-review",
		Trigger: "webhook",
		Formula: "pr-review",
		Params:  map[string]orders.OrderParam{"repo": {Required: true}, "pr": {Required: true}},
	}
	disp := &fakeDispatcher{ret: orderdispatch.DispatchResult{ScopedName: "pr-review", TrackingID: "tb-1", Fired: true}}
	deps := Deps{Dispatcher: disp, ResolveOrder: resolverFor(order)}

	match := webhookmatch.MatchResult{
		Target: "order",
		Order:  "pr-review",
		Vars:   map[string]string{"repo": "octo/demo", "pr": "1347"},
	}

	res, err := Route(context.Background(), deps, WebhookScope{Name: "github", Scope: "city"}, match)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Dispatched || res.Rejected {
		t.Fatalf("expected dispatched, got %+v", res)
	}
	if disp.calls != 1 {
		t.Fatalf("dispatcher called %d times, want 1", disp.calls)
	}
	if disp.last.Order.Name != "pr-review" || disp.last.Order.Rig != "" {
		t.Fatalf("dispatched order = %q rig %q, want pr-review/city", disp.last.Order.Name, disp.last.Order.Rig)
	}
	if disp.last.Source != orderdispatch.SourceWebhook {
		t.Fatalf("source = %q, want webhook", disp.last.Source)
	}
	// Raw vars (param-named) for validation + formula ExpandVars.
	if disp.last.Vars["repo"] != "octo/demo" || disp.last.Vars["pr"] != "1347" {
		t.Fatalf("raw Vars = %v, want repo/pr by declared name", disp.last.Vars)
	}
	// Namespaced vars for the exec-env overlay.
	if disp.last.ExecEnv["GC_WEBHOOK_ARG_repo"] != "octo/demo" || disp.last.ExecEnv["GC_WEBHOOK_ARG_pr"] != "1347" {
		t.Fatalf("ExecEnv = %v, want GC_WEBHOOK_ARG_-prefixed keys", disp.last.ExecEnv)
	}
	if _, bare := disp.last.ExecEnv["repo"]; bare {
		t.Fatalf("ExecEnv leaked bare param key: %v", disp.last.ExecEnv)
	}
	if res.Dispatch.TrackingID != "tb-1" {
		t.Fatalf("result tracking id = %q, want tb-1", res.Dispatch.TrackingID)
	}
}

// (b) A delivery targeting an order that is not trigger="webhook" is refused,
// and nothing is dispatched.
func TestRouteOrderRefusesNonWebhookTrigger(t *testing.T) {
	order := orders.Order{Name: "nightly", Trigger: "manual", Formula: "nightly"}
	disp := &fakeDispatcher{}
	deps := Deps{Dispatcher: disp, ResolveOrder: resolverFor(order)}

	res, err := Route(context.Background(), deps, WebhookScope{Name: "github", Scope: "city"},
		webhookmatch.MatchResult{Target: "order", Order: "nightly"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Rejected || res.Dispatched {
		t.Fatalf("expected refusal, got %+v", res)
	}
	if disp.calls != 0 {
		t.Fatalf("dispatcher called %d times; a non-webhook order must never dispatch", disp.calls)
	}
	if !strings.Contains(res.Reason, "trigger") || !strings.Contains(res.Reason, "webhook") {
		t.Fatalf("reason = %q, want it to explain the trigger requirement", res.Reason)
	}
}

// (c) A rig-scoped webhook targeting a foreign rig is refused before resolution.
func TestRouteOrderRefusesForeignRig(t *testing.T) {
	// Resolver would happily return an order for the foreign rig; the scope guard
	// must reject before resolution is even attempted.
	order := orders.Order{Name: "deploy", Trigger: "webhook", Rig: "intruder", Formula: "deploy"}
	disp := &fakeDispatcher{ret: orderdispatch.DispatchResult{Fired: true}}
	deps := Deps{Dispatcher: disp, ResolveOrder: resolverFor(order)}

	res, err := Route(context.Background(), deps,
		WebhookScope{Name: "maintainer-hook", Scope: "rig", Rig: "maintainer"},
		webhookmatch.MatchResult{Target: "order", Order: "deploy", Rig: "intruder"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Rejected || res.Dispatched {
		t.Fatalf("expected refusal, got %+v", res)
	}
	if disp.calls != 0 {
		t.Fatalf("dispatcher called %d times; a foreign-rig target must never dispatch", disp.calls)
	}
	if !strings.Contains(res.Reason, "maintainer") || !strings.Contains(res.Reason, "intruder") {
		t.Fatalf("reason = %q, want it to name the scoped and foreign rig", res.Reason)
	}
}

// A rig-scoped webhook targeting its OWN rig (or inheriting it via an empty rule
// rig) resolves to that rig and dispatches.
func TestRouteOrderRigScopedInheritsOwnRig(t *testing.T) {
	order := orders.Order{Name: "deploy", Trigger: "webhook", Rig: "maintainer", Formula: "deploy"}
	disp := &fakeDispatcher{ret: orderdispatch.DispatchResult{Fired: true}}
	deps := Deps{Dispatcher: disp, ResolveOrder: resolverFor(order)}

	// Empty rule rig inherits the webhook's rig.
	res, err := Route(context.Background(), deps,
		WebhookScope{Name: "maintainer-hook", Scope: "rig", Rig: "maintainer"},
		webhookmatch.MatchResult{Target: "order", Order: "deploy", Rig: ""})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Dispatched {
		t.Fatalf("expected dispatch to own rig, got %+v", res)
	}
	if disp.last.Order.Rig != "maintainer" {
		t.Fatalf("dispatched rig = %q, want maintainer", disp.last.Order.Rig)
	}
}

// (d) R4: payload-derived vars land only under GC_WEBHOOK_ARG_ and cannot shadow
// a reserved controller key even if one slips through as an arg name. The raw
// vars still carry the value for the formula/validation channel.
func TestRouteOrderNamespacesReservedArgName(t *testing.T) {
	order := orders.Order{
		Name:    "pr-review",
		Trigger: "webhook",
		Formula: "pr-review",
		Params:  map[string]orders.OrderParam{"repo": {Required: true}},
	}
	disp := &fakeDispatcher{ret: orderdispatch.DispatchResult{Fired: true}}
	deps := Deps{Dispatcher: disp, ResolveOrder: resolverFor(order)}

	// A hostile payload arg literally named GC_CITY (E2/E5 would normally reject
	// or drop it; prove the sink neutralizes it end-to-end regardless).
	match := webhookmatch.MatchResult{
		Target: "order",
		Order:  "pr-review",
		Vars:   map[string]string{"repo": "octo/demo", "GC_CITY": "pwned"},
	}
	res, err := Route(context.Background(), deps, WebhookScope{Name: "github", Scope: "city"}, match)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Dispatched {
		t.Fatalf("expected dispatch, got %+v", res)
	}
	// The exec-env overlay must NOT contain a bare GC_CITY that could shadow the
	// controller's own value; the arg is namespaced instead.
	if _, bare := disp.last.ExecEnv["GC_CITY"]; bare {
		t.Fatalf("ExecEnv leaked a bare reserved key GC_CITY: %v", disp.last.ExecEnv)
	}
	if disp.last.ExecEnv["GC_WEBHOOK_ARG_GC_CITY"] != "pwned" {
		t.Fatalf("ExecEnv = %v, want the hostile arg namespaced under GC_WEBHOOK_ARG_GC_CITY", disp.last.ExecEnv)
	}
}

// (e) A delivery missing a declared-required param is refused with a reason that
// names the param, and nothing is dispatched.
func TestRouteOrderRefusesMissingRequiredParam(t *testing.T) {
	order := orders.Order{
		Name:    "pr-review",
		Trigger: "webhook",
		Formula: "pr-review",
		Params:  map[string]orders.OrderParam{"repo": {Required: true}, "pr": {Required: true}},
	}
	disp := &fakeDispatcher{}
	deps := Deps{Dispatcher: disp, ResolveOrder: resolverFor(order)}

	// Only "pr" supplied; required "repo" is absent.
	res, err := Route(context.Background(), deps, WebhookScope{Name: "github", Scope: "city"},
		webhookmatch.MatchResult{Target: "order", Order: "pr-review", Vars: map[string]string{"pr": "1"}})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Rejected || res.Dispatched {
		t.Fatalf("expected refusal, got %+v", res)
	}
	if disp.calls != 0 {
		t.Fatalf("dispatcher called %d times; a missing required param must never dispatch", disp.calls)
	}
	if !strings.Contains(res.Reason, "repo") || !strings.Contains(res.Reason, "required") {
		t.Fatalf("reason = %q, want it to name the missing required param repo", res.Reason)
	}
}

// (f) A conversation-target delivery is handed to the conversation sink and
// never touches the order path (the dispatcher is not called).
func TestRouteConversationDoesNotTouchOrderPath(t *testing.T) {
	disp := &fakeDispatcher{}
	deps := Deps{Dispatcher: disp, ResolveOrder: func(string, string) (orders.Order, bool) {
		t.Fatal("resolver must not be called for a conversation delivery")
		return orders.Order{}, false
	}}

	res, err := Route(context.Background(), deps, WebhookScope{Name: "slack", Scope: "city"},
		webhookmatch.MatchResult{Target: "conversation"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if res.Target != "conversation" {
		t.Fatalf("target = %q, want conversation", res.Target)
	}
	if disp.calls != 0 {
		t.Fatalf("dispatcher called %d times for a conversation delivery", disp.calls)
	}
	if !strings.Contains(res.Reason, "not yet wired") {
		t.Fatalf("reason = %q, want the stub's not-yet-wired message", res.Reason)
	}
}

// A custom ConversationSink is honored over the stub.
func TestRouteConversationUsesInjectedSink(t *testing.T) {
	deps := Deps{Conversation: convFunc(func(context.Context, webhookmatch.MatchResult) (Result, error) {
		return Result{Target: "conversation", Dispatched: true}, nil
	})}
	res, err := Route(context.Background(), deps, WebhookScope{}, webhookmatch.MatchResult{Target: "conversation"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !res.Dispatched {
		t.Fatalf("expected injected conversation sink to handle delivery, got %+v", res)
	}
}

type convFunc func(context.Context, webhookmatch.MatchResult) (Result, error)

func (f convFunc) Deliver(ctx context.Context, m webhookmatch.MatchResult) (Result, error) {
	return f(ctx, m)
}
