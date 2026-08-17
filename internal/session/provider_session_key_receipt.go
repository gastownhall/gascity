package session

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// ProviderSessionKeyReceiptLabel routes the append-only hook receipt with
	// session infrastructure without making it look like a session lifecycle row.
	ProviderSessionKeyReceiptLabel = "gc:session-start-receipt"

	// ProviderSessionKeyReceiptTokenMetadataKey is the pending launch token
	// the hook must echo, rotated by the controller immediately before Start.
	ProviderSessionKeyReceiptTokenMetadataKey = "provider_session_key_receipt_token"
	// ProviderSessionKeyReceiptAuthorityMetadataKey is the immutable launch
	// authority the token was issued against.
	ProviderSessionKeyReceiptAuthorityMetadataKey = "provider_session_key_receipt_authority"
	// ProviderSessionKeyReceiptConsumeAuthorityMetadataKey is the advancing
	// authority the controller requires at consumption time.
	ProviderSessionKeyReceiptConsumeAuthorityMetadataKey = "provider_session_key_receipt_consume_authority"
	// ProviderSessionKeyReceiptIssuedAtMetadataKey is the issuance timestamp.
	ProviderSessionKeyReceiptIssuedAtMetadataKey = "provider_session_key_receipt_issued_at"

	// ProviderSessionKeyReceiptTokenEnv is the hook-side launch token variable.
	ProviderSessionKeyReceiptTokenEnv = "GC_SESSION_START_RECEIPT_TOKEN"
	// ProviderSessionKeyReceiptAuthorityEnv is the hook-side launch authority variable.
	ProviderSessionKeyReceiptAuthorityEnv = "GC_SESSION_START_RECEIPT_AUTHORITY"

	providerSessionKeyReceiptVersion = "1"

	providerSessionKeyReceiptVersionKey           = "provider_session_key_receipt_version"
	providerSessionKeyReceiptSessionIDKey         = "provider_session_key_receipt_session_id"
	providerSessionKeyReceiptGenerationKey        = "provider_session_key_receipt_generation"
	providerSessionKeyReceiptContinuationEpochKey = "provider_session_key_receipt_continuation_epoch"
	providerSessionKeyReceiptInstanceTokenKey     = "provider_session_key_receipt_instance_token"
	providerSessionKeyReceiptProviderFamilyKey    = "provider_session_key_receipt_provider_family"
	providerSessionKeyReceiptProviderKey          = "provider_session_key_receipt_provider_key"
)

var (
	// ErrProviderSessionKeyReceiptSuperseded reports a receipt whose exact
	// runtime identity, token, or authority no longer matches the live row.
	ErrProviderSessionKeyReceiptSuperseded = errors.New("provider session-key receipt superseded")
	// ErrProviderSessionKeyReceiptConflict reports divergent open receipts for
	// one session; consumption fails closed rather than guessing.
	ErrProviderSessionKeyReceiptConflict = errors.New("conflicting provider session-key receipts")
)

// IssueProviderSessionKeyReceipt rotates the random token immediately before a
// provider Start and returns the exact environment the new runtime must receive.
// The single Update is the authoritative handoff publication; old receipt beads
// are harmless because their tokens no longer match.
func IssueProviderSessionKeyReceipt(store beads.Store, info Info, now time.Time) (Info, map[string]string, error) {
	if store == nil || strings.TrimSpace(info.ID) == "" {
		return info, nil, errors.New("provider session-key receipt store unavailable")
	}
	if info.Closed || HasExecutionClaimNudgeStalled(info) || strings.TrimSpace(info.SessionKey) != "" {
		return info, nil, nil
	}
	patch := MetadataPatch{
		ProviderSessionKeyReceiptTokenMetadataKey:            NewInstanceToken(),
		ProviderSessionKeyReceiptAuthorityMetadataKey:        "",
		ProviderSessionKeyReceiptConsumeAuthorityMetadataKey: "",
		ProviderSessionKeyReceiptIssuedAtMetadataKey:         now.UTC().Format(time.RFC3339Nano),
	}
	pending := info.ApplyPatch(patch)
	authority := ProviderSessionKeyReceiptAuthority(pending)
	if !ValidExecutionStalledLifecycleAuthority(authority) {
		return info, nil, errors.New("provider session-key receipt authority unavailable")
	}
	patch[ProviderSessionKeyReceiptAuthorityMetadataKey] = authority
	patch[ProviderSessionKeyReceiptConsumeAuthorityMetadataKey] = authority
	if err := store.Update(info.ID, beads.UpdateOpts{Metadata: map[string]string(patch)}); err != nil {
		return info, nil, fmt.Errorf("issuing provider session-key receipt: %w", err)
	}
	pending = info.ApplyPatch(patch)
	return pending, ProviderSessionKeyReceiptEnvironment(pending), nil
}

// ProviderSessionKeyReceiptEnvironment returns the immutable hook coordinates
// for the currently pending provider Start.
func ProviderSessionKeyReceiptEnvironment(info Info) map[string]string {
	if strings.TrimSpace(info.ProviderSessionKeyReceiptToken) == "" ||
		strings.TrimSpace(info.ProviderSessionKeyReceiptAuthority) == "" {
		return nil
	}
	return map[string]string{
		ProviderSessionKeyReceiptTokenEnv:     info.ProviderSessionKeyReceiptToken,
		ProviderSessionKeyReceiptAuthorityEnv: info.ProviderSessionKeyReceiptAuthority,
	}
}

// BindProviderSessionKeyReceiptAuthority folds the post-start lifecycle into
// the same metadata commit. The immutable launch authority remains what the
// hook received; the consume authority advances only through this controller-
// owned commit, so an unrelated post-start mutation makes later consumption
// fail closed.
func BindProviderSessionKeyReceiptAuthority(info Info, patch MetadataPatch) MetadataPatch {
	if strings.TrimSpace(info.ProviderSessionKeyReceiptToken) == "" {
		return patch
	}
	bound := make(MetadataPatch, len(patch)+1)
	for key, value := range patch {
		bound[key] = value
	}
	expected := info.ApplyPatch(bound)
	authority := ProviderSessionKeyReceiptAuthority(expected)
	if ValidExecutionStalledLifecycleAuthority(authority) {
		bound[ProviderSessionKeyReceiptConsumeAuthorityMetadataKey] = authority
	}
	return bound
}

// ProviderSessionKeyReceiptInput is the SessionStart hook's untrusted input.
// Publish validates every field against an authoritative live session read
// before creating a non-authoritative receipt bead.
type ProviderSessionKeyReceiptInput struct {
	SessionID          string
	Generation         string
	ContinuationEpoch  string
	InstanceToken      string
	ProviderFamily     string
	ReceiptToken       string
	LaunchAuthority    string
	ProviderSessionKey string
}

// PublishProviderSessionKeyReceipt appends a receipt to the shared session
// store. It never mutates the session row, so a hook running in K8s/SSH cannot
// race the controller's stalled latch into an unauthorized session_key write.
func PublishProviderSessionKeyReceipt(store beads.Store, input ProviderSessionKeyReceiptInput) (beads.Bead, error) {
	if store == nil {
		return beads.Bead{}, errors.New("provider session-key receipt store unavailable")
	}
	input = normalizeProviderSessionKeyReceiptInput(input)
	if input.SessionID == "" || input.Generation == "" || input.ContinuationEpoch == "" ||
		input.InstanceToken == "" || input.ProviderFamily == "" || input.ReceiptToken == "" ||
		input.LaunchAuthority == "" || input.ProviderSessionKey == "" {
		return beads.Bead{}, fmt.Errorf("%w: incomplete hook coordinates", ErrProviderSessionKeyReceiptSuperseded)
	}
	raw, err := beads.HandlesFor(store).Live.Get(input.SessionID)
	if err != nil {
		return beads.Bead{}, fmt.Errorf("reading live session for provider session-key receipt: %w", err)
	}
	if !IsSessionBeadOrRepairable(raw) {
		return beads.Bead{}, fmt.Errorf("%w: %s is not a session", ErrProviderSessionKeyReceiptSuperseded, input.SessionID)
	}
	current := InfoFromPersistedBead(raw)
	currentAuthority := ProviderSessionKeyReceiptAuthority(current)
	if current.Closed || HasExecutionClaimNudgeStalled(current) || strings.TrimSpace(current.SessionKey) != "" ||
		strings.TrimSpace(current.ProviderSessionKeyReceiptToken) != input.ReceiptToken ||
		strings.TrimSpace(current.ProviderSessionKeyReceiptAuthority) != input.LaunchAuthority ||
		(strings.TrimSpace(current.ProviderSessionKeyReceiptAuthority) != currentAuthority &&
			strings.TrimSpace(current.ProviderSessionKeyReceiptConsumeAuthority) != currentAuthority) ||
		strings.TrimSpace(current.Generation) != input.Generation ||
		normalizedContinuationEpoch(current.ContinuationEpoch) != normalizedContinuationEpoch(input.ContinuationEpoch) ||
		strings.TrimSpace(current.InstanceToken) != input.InstanceToken ||
		ProviderFamilyFromInfo(current, "") != input.ProviderFamily {
		return beads.Bead{}, fmt.Errorf("%w: live session changed", ErrProviderSessionKeyReceiptSuperseded)
	}
	return store.Create(beads.Bead{
		Title: "Provider SessionStart receipt for " + input.SessionID,
		// gate is an infrastructure type excluded by every Ready backend. The
		// dedicated label still routes the row to the sessions class without
		// making it a durable session wait (it carries no gc:wait label).
		Type:      "gate",
		Labels:    []string{ProviderSessionKeyReceiptLabel},
		NoHistory: true,
		Metadata: map[string]string{
			providerSessionKeyReceiptVersionKey:           providerSessionKeyReceiptVersion,
			providerSessionKeyReceiptSessionIDKey:         input.SessionID,
			providerSessionKeyReceiptGenerationKey:        input.Generation,
			providerSessionKeyReceiptContinuationEpochKey: input.ContinuationEpoch,
			providerSessionKeyReceiptInstanceTokenKey:     input.InstanceToken,
			providerSessionKeyReceiptProviderFamilyKey:    input.ProviderFamily,
			providerSessionKeyReceiptProviderKey:          input.ProviderSessionKey,
			ProviderSessionKeyReceiptTokenMetadataKey:     input.ReceiptToken,
			ProviderSessionKeyReceiptAuthorityMetadataKey: input.LaunchAuthority,
		},
	})
}

// ConsumeProviderSessionKeyReceipts applies one exact provider key while the
// caller owns the city/session lifecycle boundary. It nevertheless performs its
// own authoritative read so stale cache state can never authorize consumption.
// Identical duplicate hook receipts are idempotent; divergent keys fail closed.
func ConsumeProviderSessionKeyReceipts(store beads.Store, sessionID string) (Info, bool, error) {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return Info{}, false, nil
	}
	raw, err := beads.HandlesFor(store).Live.Get(sessionID)
	if err != nil {
		return Info{}, false, err
	}
	if !IsSessionBeadOrRepairable(raw) {
		return Info{}, false, ErrSessionNotFound
	}
	current := InfoFromPersistedBead(raw)
	// The pending token is gone (consumed or rotated): a receipt-consume
	// fence still holding the gate is stranded by definition — its nonce IS
	// the token it fenced. Evict it before anything else so the consume lane
	// itself unjams the gate even when no receipts remain open.
	if !current.Closed && !HasExecutionClaimNudgeStalled(current) &&
		strings.TrimSpace(current.ProviderSessionKeyReceiptToken) == "" {
		_, _ = evictStaleSessionHookActivityOccupant(store, current)
	}
	receipts, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Label:         ProviderSessionKeyReceiptLabel,
		Metadata:      map[string]string{providerSessionKeyReceiptSessionIDKey: sessionID},
		IncludeClosed: true,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return current, false, fmt.Errorf("listing provider session-key receipts: %w", err)
	}
	open := make([]beads.Bead, 0, len(receipts))
	for _, receipt := range receipts {
		if receipt.Status != "closed" {
			open = append(open, receipt)
		}
	}
	if len(open) == 0 {
		return current, false, nil
	}
	closeReceipts := func(rows []beads.Bead) {
		for _, receipt := range rows {
			_ = store.Close(receipt.ID)
		}
	}
	if current.Closed || HasExecutionClaimNudgeStalled(current) || strings.TrimSpace(current.ProviderSessionKeyReceiptToken) == "" {
		closeReceipts(open)
		return current, false, nil
	}
	// Close the append-only receipt snapshot with the deterministic
	// receipt-consume fence before filtering and applying. Its nonce is the
	// pending token itself, so a controller that dies between List and Update
	// leaves a gate another controller adopts and finishes idempotently, while
	// a hook publishing concurrently cannot append into the consumed snapshot.
	fence, fenced, fenceErr := acquireProviderSessionKeyReceiptFence(store, sessionID)
	if fenceErr != nil {
		if errors.Is(fenceErr, ErrSessionHookActivityBlocked) {
			// Another consume snapshot owns the window. The receipts stay open
			// and the level-triggered callers retry on their next boundary.
			return current, false, nil
		}
		return current, false, fenceErr
	}
	if fence != nil {
		defer fence.Release() //nolint:errcheck // best-effort exact clear; adoption recovers
		current = fenced
	}
	currentAuthority := ProviderSessionKeyReceiptAuthority(current)
	if !ValidExecutionStalledLifecycleAuthority(current.ProviderSessionKeyReceiptAuthority) ||
		!ValidExecutionStalledLifecycleAuthority(current.ProviderSessionKeyReceiptConsumeAuthority) ||
		currentAuthority != strings.TrimSpace(current.ProviderSessionKeyReceiptConsumeAuthority) {
		return current, false, fmt.Errorf("%w: consume authority changed", ErrProviderSessionKeyReceiptSuperseded)
	}

	family := ProviderFamilyFromInfo(current, "")
	matching := make([]beads.Bead, 0, len(open))
	stale := make([]beads.Bead, 0, len(open))
	keys := make(map[string]struct{})
	for _, receipt := range open {
		meta := receipt.Metadata
		key := strings.TrimSpace(meta[providerSessionKeyReceiptProviderKey])
		exact := strings.TrimSpace(meta[providerSessionKeyReceiptVersionKey]) == providerSessionKeyReceiptVersion &&
			strings.TrimSpace(meta[providerSessionKeyReceiptSessionIDKey]) == strings.TrimSpace(current.ID) &&
			strings.TrimSpace(meta[ProviderSessionKeyReceiptTokenMetadataKey]) == strings.TrimSpace(current.ProviderSessionKeyReceiptToken) &&
			strings.TrimSpace(meta[ProviderSessionKeyReceiptAuthorityMetadataKey]) == strings.TrimSpace(current.ProviderSessionKeyReceiptAuthority) &&
			strings.TrimSpace(meta[providerSessionKeyReceiptGenerationKey]) == strings.TrimSpace(current.Generation) &&
			normalizedContinuationEpoch(meta[providerSessionKeyReceiptContinuationEpochKey]) == normalizedContinuationEpoch(current.ContinuationEpoch) &&
			strings.TrimSpace(meta[providerSessionKeyReceiptInstanceTokenKey]) == strings.TrimSpace(current.InstanceToken) &&
			strings.TrimSpace(meta[providerSessionKeyReceiptProviderFamilyKey]) == family && key != ""
		if !exact {
			stale = append(stale, receipt)
			continue
		}
		matching = append(matching, receipt)
		keys[key] = struct{}{}
	}
	closeReceipts(stale)
	if len(keys) == 0 {
		return current, false, nil
	}
	if len(keys) != 1 {
		return current, false, fmt.Errorf("%w for session %s", ErrProviderSessionKeyReceiptConflict, current.ID)
	}
	providerKey := ""
	for key := range keys {
		providerKey = key
	}
	if existing := strings.TrimSpace(current.SessionKey); existing != "" && existing != providerKey {
		return current, false, fmt.Errorf("%w for session %s", ErrProviderSessionKeyReceiptConflict, current.ID)
	}
	patch := MetadataPatch{
		"session_key": providerKey,
		ProviderSessionKeyReceiptTokenMetadataKey:            "",
		ProviderSessionKeyReceiptAuthorityMetadataKey:        "",
		ProviderSessionKeyReceiptConsumeAuthorityMetadataKey: "",
		ProviderSessionKeyReceiptIssuedAtMetadataKey:         "",
	}
	if err := store.Update(current.ID, beads.UpdateOpts{Metadata: map[string]string(patch)}); err != nil {
		// Update errors may be transport-ambiguous. An authoritative self-read is
		// the only safe success classification; unknown remains a loud retry.
		latestRaw, readErr := beads.HandlesFor(store).Live.Get(current.ID)
		if readErr != nil {
			return current, false, errors.Join(err, readErr)
		}
		latest := InfoFromPersistedBead(latestRaw)
		if strings.TrimSpace(latest.SessionKey) != providerKey ||
			strings.TrimSpace(latest.ProviderSessionKeyReceiptToken) != "" {
			return latest, false, err
		}
		current = latest
	} else {
		current = current.ApplyPatch(patch)
	}
	// The deferred fence release clears the persisted gate after this return;
	// fold the same clear onto the returned snapshot so callers never observe
	// a consume-fence that has already ended.
	current.SessionHookActivityGate = ""
	closeReceipts(matching)
	return current, true, nil
}

// normalizedContinuationEpoch applies the same absent-epoch convention as the
// runtime identity fence and the hook activity gate: an absent epoch is the
// first conversation epoch, never an unprovable identity.
func normalizedContinuationEpoch(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strconv.Itoa(DefaultContinuationEpoch)
	}
	return value
}

func normalizeProviderSessionKeyReceiptInput(input ProviderSessionKeyReceiptInput) ProviderSessionKeyReceiptInput {
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.Generation = strings.TrimSpace(input.Generation)
	input.ContinuationEpoch = strings.TrimSpace(input.ContinuationEpoch)
	input.InstanceToken = strings.TrimSpace(input.InstanceToken)
	input.ProviderFamily = strings.TrimSpace(input.ProviderFamily)
	input.ReceiptToken = strings.TrimSpace(input.ReceiptToken)
	input.LaunchAuthority = strings.TrimSpace(input.LaunchAuthority)
	input.ProviderSessionKey = strings.TrimSpace(input.ProviderSessionKey)
	return input
}
