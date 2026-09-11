// Package orderdispatch defines the reusable seam through which any caller fires
// a resolved Gas City order — the controller tick loop today, and the supervisor
// webhook receiver (E3/E6) next — so there is one dispatch core, not a per-caller
// reimplementation.
//
// The seam is deliberately thin: the caller pre-resolves the target order (so it
// can enforce its own policy — required-param validation, trigger and rig
// scoping — before anything fires) and hands the whole order to a [Dispatcher].
// The Dispatcher performs store resolution, tracking-bead creation, and the
// formula/exec dispatch itself. The concrete Dispatcher lives in cmd/gc, where it
// wraps the controller's live order dispatcher and reuses the same dispatchOne
// core the tick loop uses; this package holds only the contract so downstream
// packages (internal/webhooksink, internal/api) can depend on it without
// importing package main.
package orderdispatch

import (
	"context"

	"github.com/gastownhall/gascity/internal/orders"
)

// Source identifies what triggered a dispatch routed through the seam. It is
// carried for provenance/audit and lets a caller apply source-specific policy
// (the webhook sink pre-namespaces its untrusted exec-env args, R4).
type Source string

const (
	// SourceTick is the controller trigger-evaluation loop.
	SourceTick Source = "tick"
	// SourceManual is an operator-invoked `gc order run`.
	SourceManual Source = "manual"
	// SourceWebhook is the supervisor webhook receiver.
	SourceWebhook Source = "webhook"
)

// DispatchRequest is one order-fire intent. The Order is pre-resolved by the
// caller, which has already enforced its own guards.
type DispatchRequest struct {
	// Order is the resolved target order to fire.
	Order orders.Order
	// Vars are the raw, param-named dispatch args. They drive required-param
	// validation and the formula ExpandVars channel. Keys are declared order
	// param names, never controller env keys.
	Vars map[string]string
	// ExecEnv is the environment overlay applied when Order is an exec order.
	// Untrusted callers (the webhook sink) MUST pre-namespace these so a payload
	// value can never shadow a controller-owned or static [order.env] key (R4) —
	// see webhookmatch.ExecEnvVars. When nil the dispatcher falls back to Vars,
	// which preserves the trusted tick/CLI raw-overlay semantics.
	ExecEnv map[string]string
	// Source records what triggered this dispatch.
	Source Source
}

// DispatchResult reports the outcome of routing a request through the seam.
type DispatchResult struct {
	// ScopedName is the fired order's rig-qualified name.
	ScopedName string
	// TrackingID is the created tracking bead id (empty when nothing fired).
	TrackingID string
	// Fired is true when the order was accepted and dispatch was launched.
	Fired bool
	// Rejected is true when a guard refused the dispatch before firing.
	Rejected bool
	// Reason explains a rejection (empty on success).
	Reason string
}

// Dispatcher fires a pre-resolved order. cmd/gc implements it over the same
// dispatchOne core the controller tick loop uses; tests inject a fake.
type Dispatcher interface {
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)
}
