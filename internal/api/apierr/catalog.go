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
	// Resource resolution.
	CityNotFound = Register(ProblemType{Code: "city-not-found", Status: http.StatusNotFound, Title: "City Not Found"})
	BeadNotFound = Register(ProblemType{Code: "bead-not-found", Status: http.StatusNotFound, Title: "Bead Not Found"})

	// Request validation.
	InvalidRequest   = Register(ProblemType{Code: "invalid-request", Status: http.StatusBadRequest, Title: "Invalid Request"})
	ValidationFailed = Register(ProblemType{Code: "validation-failed", Status: http.StatusUnprocessableEntity, Title: "Validation Failed"})

	// Concurrency / state conflicts.
	ConflictConcurrentDelete = Register(ProblemType{Code: "conflict-concurrent-delete", Status: http.StatusConflict, Title: "Concurrent Delete Conflict"})
	ConflictWrongState       = Register(ProblemType{Code: "conflict-wrong-state", Status: http.StatusConflict, Title: "Wrong State Conflict"})

	// Idempotency (two-phase reserve/complete).
	IdempotencyInFlight = Register(ProblemType{Code: "idempotency-in-flight", Status: http.StatusConflict, Title: "Idempotency Key In Flight"})
	IdempotencyMismatch = Register(ProblemType{Code: "idempotency-mismatch", Status: http.StatusUnprocessableEntity, Title: "Idempotency Key Body Mismatch"})

	// Backend availability.
	StoreUnavailable = Register(ProblemType{Code: "store-unavailable", Status: http.StatusServiceUnavailable, Title: "Store Unavailable"})
	Internal         = Register(ProblemType{Code: "internal", Status: http.StatusInternalServerError, Title: "Internal Server Error"})

	// Sling. The first three are frozen (already public in the spec).
	SlingMissingBead            = Register(ProblemType{Code: "sling-missing-bead", Status: http.StatusBadRequest, Title: "Sling Missing Bead"})
	SlingCrossRig               = Register(ProblemType{Code: "sling-cross-rig", Status: http.StatusBadRequest, Title: "Sling Cross-Rig"})
	SlingCrossStoreRoute        = Register(ProblemType{Code: "sling-cross-store-route", Status: http.StatusBadRequest, Title: "Sling Cross-Store Route"})
	SlingSourceWorkflowConflict = Register(ProblemType{Code: "sling-source-workflow-conflict", Status: http.StatusConflict, Title: "Sling Source Workflow Conflict"})
)
