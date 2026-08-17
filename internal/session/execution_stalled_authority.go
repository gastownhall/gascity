package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

const executionStalledLifecycleAuthorityPrefix = "v1:sha256:"

// ExecutionStalledLifecycleAuthority returns the canonical, collision-resistant
// authority for an execution-stalled drain. Its field order is deliberately
// fixed and its domain includes the session id. Execution-stalled marker fields
// are excluded; the provider SessionStart handoff coordinates are included
// because they are controller-owned lifecycle authority, not hook-owned data.
func ExecutionStalledLifecycleAuthority(info Info) string {
	return hashSessionLifecycleAuthority(
		"gascity.execution-stalled.lifecycle-authority.v1",
		info.ID,
		executionStalledLifecycleAuthorityValues(info, true),
	)
}

// ValidExecutionStalledLifecycleAuthority reports whether authority has the
// exact versioned SHA-256 wire shape emitted above.
func ValidExecutionStalledLifecycleAuthority(authority string) bool {
	if len(authority) != len(executionStalledLifecycleAuthorityPrefix)+sha256.Size*2 ||
		!strings.HasPrefix(authority, executionStalledLifecycleAuthorityPrefix) {
		return false
	}
	_, err := hex.DecodeString(authority[len(executionStalledLifecycleAuthorityPrefix):])
	return err == nil
}

// ProviderSessionKeyReceiptAuthority binds a controller-issued SessionStart
// receipt token to the same complete lifecycle/launch identity as the stalled
// authority. The stored launch/consume authority fields themselves are omitted
// to avoid a self-referential digest; the random token and issue timestamp
// remain covered.
func ProviderSessionKeyReceiptAuthority(info Info) string {
	return hashSessionLifecycleAuthority(
		"gascity.provider-session-key-receipt-authority.v1",
		info.ID,
		executionStalledLifecycleAuthorityValues(info, false),
	)
}

func hashSessionLifecycleAuthority(domain, sessionID string, values []string) string {
	canonical := make([]string, 0, len(values)+2)
	canonical = append(canonical, domain, sessionID)
	canonical = append(canonical, values...)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "" // []string cannot fail today; fail closed if its codec changes.
	}
	digest := sha256.Sum256(encoded)
	return executionStalledLifecycleAuthorityPrefix + hex.EncodeToString(digest[:])
}

func executionStalledLifecycleAuthorityValues(info Info, includeReceiptAuthority bool) []string {
	values := []string{
		info.Template,
		info.AgentName,
		info.Alias,
		info.CommonName,
		info.ConfiguredNamedIdentity,
		strconv.FormatBool(info.ConfiguredNamedSession),
		info.ConfiguredNamedMode,
		strconv.FormatBool(info.PoolManaged),
		info.PoolSlot,
		info.SessionOrigin,
		info.CanonicalInstanceNameMetadata,
		info.CanonicalPoolSlotMetadata,
		info.Provider,
		info.ProviderKind,
		info.BuiltinAncestor,
		info.TransportMetadata,
		info.Command,
		info.WorkDir,
		info.SessionKey,
		info.ResumeFlag,
		info.ResumeStyle,
		info.ResumeCommand,
		info.SessionIDFlag,
		info.WakeMode,
		info.TemplateOverrides,
		info.StartedConfigHash,
		info.StartedProvisionHash,
		info.StartedLaunchHash,
		info.StartedLiveHash,
		info.PinAwake,
		info.ContinuityEligible,
		strconv.FormatBool(info.Closed),
		info.SessionNameMetadata,
		info.MetadataState,
		info.StateReason,
		info.SleepReason,
		info.SleepIntent,
		info.HeldUntil,
		info.WaitHold,
		info.QuarantinedUntil,
		info.WakeRequest,
		info.WakeRequestedAt,
		info.WakeRequestToken,
		info.WakeAttemptsMetadata,
		info.ChurnCount,
		info.RestartRequested,
		info.ContinuationResetPending,
		info.PendingCreateClaimMetadata,
		info.PendingCreateStartedAt,
		info.LastWokeAt,
		info.AwakeStartedAt,
		info.CreationCompleteAt,
		info.Generation,
		info.ContinuationEpoch,
		info.InstanceToken,
		info.ProviderSessionKeyReceiptToken,
		info.ProviderSessionKeyReceiptIssuedAt,
	}
	if includeReceiptAuthority {
		values = append(values,
			info.ProviderSessionKeyReceiptAuthority,
			info.ProviderSessionKeyReceiptConsumeAuthority,
		)
	}
	return values
}
