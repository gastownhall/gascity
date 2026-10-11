//go:build integration

package beads

// This file bridges two unexported pieces of the conditional-write machinery
// to the external integration row in bdstore_conditional_integration_test.go
// (package beads_test — the beadstest harness imports beads, so the row
// cannot live in an internal test file). Test-binary-only: the identifiers
// exist solely under the integration build tag and are never part of the
// production package surface.

// ConditionalWritesCapableForIntegration exposes the production four-verb
// capability probe so the row's skip decision IS the production decision —
// no duplicated --help grep that can drift from conditionalWritesCapable.
func ConditionalWritesCapableForIntegration(s *BdStore) (bool, error) {
	return s.conditionalWritesCapable()
}

// ClassifyConditionalWriteResultForIntegration exposes the pure classifier
// for the real-bd adversarial cells (build-spec ~line 250, input A): the
// classifier must be fed a REAL capable bd's usage echo, which BdStore's own
// verbs can never produce (they never send an unknown flag).
func ClassifyConditionalWriteResultForIntegration(out []byte, err error) error {
	return classifyConditionalWriteResult(out, err)
}
