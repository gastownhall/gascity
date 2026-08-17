package session

import (
	"errors"
	"fmt"
	"strings"
)

// ExecutionClaimNudgeStalledMetadataKey is the durable fence set after a pool
// worker exhausts its bounded claim-nudge budget. While it is non-empty, no
// managed wake, start, attach, or message delivery may revive or command the
// session; the execution-stalled recovery lane exclusively owns convergence.
const ExecutionClaimNudgeStalledMetadataKey = "execution_claim_nudge_stalled"

// ErrExecutionStalledRecoveryPending reports that the execution-stalled
// recovery lane currently owns a session's lifecycle.
var ErrExecutionStalledRecoveryPending = errors.New("execution-stalled session recovery pending")

// HasExecutionClaimNudgeStalled reports whether a projected session carries
// the durable execution-stalled lifecycle fence.
func HasExecutionClaimNudgeStalled(info Info) bool {
	return strings.TrimSpace(info.ExecutionClaimNudgeStalled) != "" ||
		IsExecutionStalledActivityFence(info.SessionHookActivityGate)
}

// HasExecutionClaimNudgeStalledMetadata is the raw-metadata form used at the
// few store boundaries that must reject work before projection.
func HasExecutionClaimNudgeStalledMetadata(metadata map[string]string) bool {
	return strings.TrimSpace(metadata[ExecutionClaimNudgeStalledMetadataKey]) != "" ||
		IsExecutionStalledActivityFence(metadata[SessionHookActivityGateMetadataKey])
}

func executionStalledRecoveryPendingError(id string, metadata map[string]string) error {
	if !HasExecutionClaimNudgeStalledMetadata(metadata) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrExecutionStalledRecoveryPending, id)
}
