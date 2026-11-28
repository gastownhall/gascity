package beads

import (
	"context"

	"github.com/gastownhall/gascity/internal/telemetry"
)

// Conflict classes recorded with a write that used its whole retry budget.
// They are a closed set: the class is a metric label.
const (
	// writeConflictClassSerialization is a Dolt/MySQL serialization conflict.
	writeConflictClassSerialization = "serialization"
	// writeConflictClassCAS is a *CASRetriesExhaustedError from the metadata
	// compare-and-set emulation.
	writeConflictClassCAS = "cas"
	// writeConflictClassTransient is any other error a store's retry loop
	// treats as worth another attempt (a sqlite busy marker, a catalog
	// prepare failure, an ambiguous bd connection failure).
	writeConflictClassTransient = "transient"
)

// writeConflictClass names the conflict class of an error that a retry loop
// accepted for another attempt.
func writeConflictClass(err error) string {
	switch {
	case IsCASRetriesExhausted(err):
		return writeConflictClassCAS
	case IsRetryableConflict(err):
		return writeConflictClassSerialization
	default:
		return writeConflictClassTransient
	}
}

// recordWriteBudget reports how a retried write ended when it ran to the end
// of its attempt budget, and is silent otherwise. attempts is how many times
// the write ran and budget how many it may run. err is the final error;
// retryable says whether the store's retry loop would have retried it, so an
// error the loop gives up on at once (a permanent failure on the last lap)
// is not reported as exhaustion. operation is a closed store-verb token and
// id the bead the write targeted, empty when the write spans several.
func recordWriteBudget(operation, id string, attempts, budget int, err error, retryable bool) {
	if attempts < budget {
		return
	}
	switch {
	case err == nil:
		telemetry.RecordWriteConflict(context.Background(), operation, id,
			writeConflictClassSerialization, telemetry.WriteConflictRecoveredFinal, attempts)
	case retryable:
		telemetry.RecordWriteConflict(context.Background(), operation, id,
			writeConflictClass(err), telemetry.WriteConflictExhausted, attempts)
	}
}
