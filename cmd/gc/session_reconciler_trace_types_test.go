package main

import "testing"

// TestTraceCodeConstantValues pins every trace-code constant added by S26b to
// its exact recorded string. A typo here would silently change the bytes that
// land in the trace JSONL (site_code/reason_code/outcome_code) — this test is
// the guard against that class of corruption.
func TestTraceCodeConstantValues(t *testing.T) {
	reasons := map[TraceReasonCode]string{
		TraceReasonMaxSessionAge: "max_session_age",
		TraceReasonUserHold:      "user_hold",
		TraceReasonQuarantine:    "quarantine",
	}
	for got, want := range reasons {
		if string(got) != want {
			t.Errorf("reason constant = %q, want %q", string(got), want)
		}
	}

	outcomes := map[TraceOutcomeCode]string{
		TraceOutcomeResolutionFailed:    "resolution_failed",
		TraceOutcomeStartErrorConverged: "start_error_converged",
		TraceOutcomeSessionInitializing: "session_initializing",
		TraceOutcomeStartEnqueued:       "start_enqueued",
		TraceOutcomeDeferredUserHold:    "deferred_user_hold",
		TraceOutcomeDeferredQuarantine:  "deferred_quarantine",
		TraceOutcomeDeferredBusy:        "deferred_busy",
	}
	for got, want := range outcomes {
		if string(got) != want {
			t.Errorf("outcome constant = %q, want %q", string(got), want)
		}
	}
}
