package apierr

import "net/http"

// The catalog: every machine-readable problem type the API emits, registered at
// package init. Keep this the single reviewable taxonomy file. Codes are
// generic by default (bead-not-found, not bead-N-not-found) and refined only
// where a client must branch differently (two distinct 409 conflicts). Adding a
// code is additive; removing or renaming one is a breaking change.
//
// The three sling-* URNs are frozen: they are already public in the OpenAPI spec
// via x-gascity-problem-types and must stay byte-identical.
var (
	// Resource resolution. Codes are per-resource (city-not-found, not a generic
	// not-found) so a client branches on which resource was missing; rig-not-found
	// is shared across the domains that resolve a rig.
	CityNotFound        = Register(ProblemType{Code: "city-not-found", Status: http.StatusNotFound, Title: "City Not Found"})
	BeadNotFound        = Register(ProblemType{Code: "bead-not-found", Status: http.StatusNotFound, Title: "Bead Not Found"})
	MailNotFound        = Register(ProblemType{Code: "mail-not-found", Status: http.StatusNotFound, Title: "Mail Message Not Found"})
	RigNotFound         = Register(ProblemType{Code: "rig-not-found", Status: http.StatusNotFound, Title: "Rig Not Found"})
	SessionNotFound     = Register(ProblemType{Code: "session-not-found", Status: http.StatusNotFound, Title: "Session Not Found"})
	AgentNotFound       = Register(ProblemType{Code: "agent-not-found", Status: http.StatusNotFound, Title: "Agent Not Found"})
	ProviderNotFound    = Register(ProblemType{Code: "provider-not-found", Status: http.StatusNotFound, Title: "Provider Not Found"})
	ConvoyNotFound      = Register(ProblemType{Code: "convoy-not-found", Status: http.StatusNotFound, Title: "Convoy Not Found"})
	WorkflowNotFound    = Register(ProblemType{Code: "workflow-not-found", Status: http.StatusNotFound, Title: "Workflow Not Found"})
	FormulaNotFound     = Register(ProblemType{Code: "formula-not-found", Status: http.StatusNotFound, Title: "Formula Not Found"})
	OrderNotFound       = Register(ProblemType{Code: "order-not-found", Status: http.StatusNotFound, Title: "Order Not Found"})
	ExtmsgGroupNotFound = Register(ProblemType{Code: "extmsg-group-not-found", Status: http.StatusNotFound, Title: "External-Message Group Not Found"})
	// ScopeNotFound is a city-or-rig scope reference that does not resolve (the
	// detail names which kind); it is distinct from the resource the scope was
	// being resolved for (e.g. a formula).
	ScopeNotFound   = Register(ProblemType{Code: "scope-not-found", Status: http.StatusNotFound, Title: "Scope Not Found"})
	ServiceNotFound = Register(ProblemType{Code: "service-not-found", Status: http.StatusNotFound, Title: "Service Not Found"})
	PatchNotFound   = Register(ProblemType{Code: "patch-not-found", Status: http.StatusNotFound, Title: "Patch Not Found"})
	PackNotFound    = Register(ProblemType{Code: "pack-not-found", Status: http.StatusNotFound, Title: "Pack Not Found"})

	// Request validation.
	InvalidRequest   = Register(ProblemType{Code: "invalid-request", Status: http.StatusBadRequest, Title: "Invalid Request"})
	ValidationFailed = Register(ProblemType{Code: "validation-failed", Status: http.StatusUnprocessableEntity, Title: "Validation Failed"})
	// WebhookRejected is a well-formed webhook request the receiver declined to
	// dispatch (unknown/unwired sink, policy) — distinct from validation-failed,
	// which is huma's schema-validation auto-stamp.
	WebhookRejected = Register(ProblemType{Code: "webhook-rejected", Status: http.StatusUnprocessableEntity, Title: "Webhook Rejected"})

	// Concurrency / state conflicts. concurrent-delete/concurrent-modify are
	// retryable lost-update races (the target changed under the write); wrong-state
	// is a terminal precondition failure (the target is in a state the request
	// cannot proceed from).
	ConflictConcurrentDelete = Register(ProblemType{Code: "conflict-concurrent-delete", Status: http.StatusConflict, Title: "Concurrent Delete Conflict"})
	ConflictConcurrentModify = Register(ProblemType{Code: "conflict-concurrent-modify", Status: http.StatusConflict, Title: "Concurrent Modify Conflict"})
	ConflictWrongState       = Register(ProblemType{Code: "conflict-wrong-state", Status: http.StatusConflict, Title: "Wrong State Conflict"})
	// SessionConflict is the one code for the session 409s. Many carry a
	// differentiating detail prefix the CLI already branches on
	// (ambiguous:/pending_interaction:/no_pending:/invalid_interaction:/
	// illegal_transition:), mirroring sling-source-workflow-conflict; the rest
	// share a generic "conflict:" (or no) prefix. A later slice may split the
	// create-time name/alias-uniqueness conflicts into their own code, since a
	// client cannot today distinguish "pick a different name" from "resume/stop
	// first" by code or prefix.
	SessionConflict = Register(ProblemType{Code: "session-conflict", Status: http.StatusConflict, Title: "Session State Conflict"})

	// AmbiguousReference is a name/reference that matched more than one resource;
	// the client should re-address with a scoped/qualified name, not retry or wait.
	AmbiguousReference = Register(ProblemType{Code: "ambiguous-reference", Status: http.StatusConflict, Title: "Ambiguous Reference"})
	// OperationInProgress is a transient 409 — another operation on the same target
	// is running; the client may retry — distinct from a terminal "already exists"
	// wrong-state conflict.
	OperationInProgress = Register(ProblemType{Code: "operation-in-progress", Status: http.StatusConflict, Title: "Operation In Progress"})

	// Authorization / capability.
	Forbidden      = Register(ProblemType{Code: "forbidden", Status: http.StatusForbidden, Title: "Forbidden"})
	NotImplemented = Register(ProblemType{Code: "not-implemented", Status: http.StatusNotImplemented, Title: "Not Implemented"})

	// Idempotency (two-phase reserve/complete).
	IdempotencyInFlight = Register(ProblemType{Code: "idempotency-in-flight", Status: http.StatusConflict, Title: "Idempotency Key In Flight"})
	IdempotencyMismatch = Register(ProblemType{Code: "idempotency-mismatch", Status: http.StatusUnprocessableEntity, Title: "Idempotency Key Body Mismatch"})

	// Backend availability. store-unavailable is the bead-store-not-live 503 emitted
	// by the shared cacheLiveOr503 helper; service-unavailable is the generic 503
	// that every other converted plain 503 uses — its title matches http.StatusText
	// so the wire title is preserved.
	StoreUnavailable   = Register(ProblemType{Code: "store-unavailable", Status: http.StatusServiceUnavailable, Title: "Store Unavailable"})
	ServiceUnavailable = Register(ProblemType{Code: "service-unavailable", Status: http.StatusServiceUnavailable, Title: "Service Unavailable"})
	Internal           = Register(ProblemType{Code: "internal", Status: http.StatusInternalServerError, Title: "Internal Server Error"})

	// Generic transport statuses. Titles match http.StatusText so converting a
	// plain error of these statuses preserves the wire title.
	MethodNotAllowed = Register(ProblemType{Code: "method-not-allowed", Status: http.StatusMethodNotAllowed, Title: "Method Not Allowed"})
	BadGateway       = Register(ProblemType{Code: "bad-gateway", Status: http.StatusBadGateway, Title: "Bad Gateway"})
	GatewayTimeout   = Register(ProblemType{Code: "gateway-timeout", Status: http.StatusGatewayTimeout, Title: "Gateway Timeout"})

	// Sling. The first three are frozen (already public in the spec).
	SlingMissingBead            = Register(ProblemType{Code: "sling-missing-bead", Status: http.StatusBadRequest, Title: "Sling Missing Bead"})
	SlingCrossRig               = Register(ProblemType{Code: "sling-cross-rig", Status: http.StatusBadRequest, Title: "Sling Cross-Rig"})
	SlingCrossStoreRoute        = Register(ProblemType{Code: "sling-cross-store-route", Status: http.StatusBadRequest, Title: "Sling Cross-Store Route"})
	SlingSourceWorkflowConflict = Register(ProblemType{Code: "sling-source-workflow-conflict", Status: http.StatusConflict, Title: "Sling Source Workflow Conflict"})
)
