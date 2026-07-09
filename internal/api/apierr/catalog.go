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
	CityNotFound     = Register(ProblemType{Code: "city-not-found", Status: http.StatusNotFound, Title: "City Not Found"})
	BeadNotFound     = Register(ProblemType{Code: "bead-not-found", Status: http.StatusNotFound, Title: "Bead Not Found"})
	MailNotFound     = Register(ProblemType{Code: "mail-not-found", Status: http.StatusNotFound, Title: "Mail Message Not Found"})
	RigNotFound      = Register(ProblemType{Code: "rig-not-found", Status: http.StatusNotFound, Title: "Rig Not Found"})
	SessionNotFound  = Register(ProblemType{Code: "session-not-found", Status: http.StatusNotFound, Title: "Session Not Found"})
	AgentNotFound    = Register(ProblemType{Code: "agent-not-found", Status: http.StatusNotFound, Title: "Agent Not Found"})
	ProviderNotFound = Register(ProblemType{Code: "provider-not-found", Status: http.StatusNotFound, Title: "Provider Not Found"})

	// Request validation.
	InvalidRequest   = Register(ProblemType{Code: "invalid-request", Status: http.StatusBadRequest, Title: "Invalid Request"})
	ValidationFailed = Register(ProblemType{Code: "validation-failed", Status: http.StatusUnprocessableEntity, Title: "Validation Failed"})

	// Concurrency / state conflicts.
	ConflictConcurrentDelete = Register(ProblemType{Code: "conflict-concurrent-delete", Status: http.StatusConflict, Title: "Concurrent Delete Conflict"})
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

	// Authorization / capability.
	Forbidden      = Register(ProblemType{Code: "forbidden", Status: http.StatusForbidden, Title: "Forbidden"})
	NotImplemented = Register(ProblemType{Code: "not-implemented", Status: http.StatusNotImplemented, Title: "Not Implemented"})

	// Idempotency (two-phase reserve/complete).
	IdempotencyInFlight = Register(ProblemType{Code: "idempotency-in-flight", Status: http.StatusConflict, Title: "Idempotency Key In Flight"})
	IdempotencyMismatch = Register(ProblemType{Code: "idempotency-mismatch", Status: http.StatusUnprocessableEntity, Title: "Idempotency Key Body Mismatch"})

	// Backend availability. store-unavailable is reserved for the bead-store-not-
	// live 503 emitted by the shared cacheLiveOr503 helper, whose conversion is
	// tracked separately (still legacy today); service-unavailable is the generic
	// 503 that every converted plain 503 uses — its title matches http.StatusText
	// so the wire title is preserved.
	StoreUnavailable   = Register(ProblemType{Code: "store-unavailable", Status: http.StatusServiceUnavailable, Title: "Store Unavailable"})
	ServiceUnavailable = Register(ProblemType{Code: "service-unavailable", Status: http.StatusServiceUnavailable, Title: "Service Unavailable"})
	Internal           = Register(ProblemType{Code: "internal", Status: http.StatusInternalServerError, Title: "Internal Server Error"})

	// Sling. The first three are frozen (already public in the spec).
	SlingMissingBead            = Register(ProblemType{Code: "sling-missing-bead", Status: http.StatusBadRequest, Title: "Sling Missing Bead"})
	SlingCrossRig               = Register(ProblemType{Code: "sling-cross-rig", Status: http.StatusBadRequest, Title: "Sling Cross-Rig"})
	SlingCrossStoreRoute        = Register(ProblemType{Code: "sling-cross-store-route", Status: http.StatusBadRequest, Title: "Sling Cross-Store Route"})
	SlingSourceWorkflowConflict = Register(ProblemType{Code: "sling-source-workflow-conflict", Status: http.StatusConflict, Title: "Sling Source Workflow Conflict"})
)
