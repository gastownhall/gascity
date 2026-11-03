package extmsg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

const (
	publicAdmissionLabel = "gc:extmsg-admission"
	publicOutboxLabel    = "gc:extmsg-outbox"
)

var errPublicPlanChanged = errors.New("public sequence changed during planning")

// PublicRecipient is a frozen delivery identity. Exactly one of SessionID and
// AgentIdentity is required. An agent without a live session remains a durable
// intent until its configured session can be materialized; it is never dropped.
type PublicRecipient struct {
	SessionID     string              `json:"session_id,omitempty"`
	AgentIdentity string              `json:"agent_identity,omitempty"`
	Policy        PublicMessagePolicy `json:"policy"`
}

// PublicMessagePolicy is only the JSON projection of the existing message
// policy. NormalizeMessagePolicy owns all values, defaults and validation.
type PublicMessagePolicy struct {
	When          opjournal.When     `json:"when,omitempty"`
	Wake          opjournal.Wake     `json:"wake,omitempty"`
	Role          opjournal.Role     `json:"role,omitempty"`
	OnUnsupported opjournal.Fallback `json:"on_unsupported,omitempty"`
	NowGeneration int64              `json:"now_generation,omitempty"`
	NowTurn       string             `json:"now_turn,omitempty"`
}

// PublicInboundRoute is a controller-derived snapshot of the existing public
// route. It does not select recipients: explicit qualified deliveries still do.
// The route is frozen at first admission so later group/binding edits cannot
// change native target discrimination or the public invalidation's routing IDs.
type PublicInboundRoute struct {
	TargetSessionID         string
	TargetAgentName         string
	ExplicitTargetSessionID string
}

// PublicAdmissionInput is one immutable public message and recipient selection.
// RequestKey is scoped by conversation and actor ID. ReceivedAt is not payload:
// a transport retry can observe the same message at a different time.
type PublicAdmissionInput struct {
	Caller     Caller
	RequestKey string
	Message    ExternalInboundMessage
	Recipients []PublicRecipient
	// RecipientKeys is controller-derived canonical identity, excluded from the
	// client digest. Its frozen value owns dedup/order across named/session aliases.
	RecipientKeys map[string]string
	// Route is authoritative GC routing metadata, not client-controlled input.
	// It is excluded from the client digest and persisted only on first admit.
	Route PublicInboundRoute
}

// PublicReceipt proves the public append and all recipient intents committed
// together. A receipt is admission, not evidence that a provider ran the turn.
type PublicReceipt struct {
	ID              string   `json:"id"`
	RequestKey      string   `json:"request_key"`
	PayloadDigest   string   `json:"payload_digest"`
	TranscriptID    string   `json:"transcript_id"`
	Sequence        int64    `json:"sequence"`
	DeliveryIDs     []string `json:"delivery_ids"`
	TargetSession   string   `json:"target_session,omitempty"`
	TargetAgent     string   `json:"target_agent,omitempty"`
	CancelRequested bool     `json:"cancel_requested,omitempty"`
}

// PublicDelivery is the Messaging outbox item. OperationKey is the immutable
// Sessions lane idempotency key. OperationID is filled only after lane admission
// has durably acknowledged that key. It does not mean provider completion.
type PublicDelivery struct {
	ID                      string              `json:"id"`
	ReceiptID               string              `json:"receipt_id"`
	TranscriptID            string              `json:"transcript_id"`
	Conversation            ConversationRef     `json:"conversation"`
	Recipient               PublicRecipient     `json:"recipient"`
	ResolvedSessionID       string              `json:"resolved_session_id,omitempty"`
	ExplicitTargetSessionID string              `json:"explicit_target_session_id,omitempty"`
	OperationKey            string              `json:"operation_key"`
	OperationID             string              `json:"operation_id,omitempty"`
	Sequence                int64               `json:"sequence"`
	RecipientKey            string              `json:"recipient_key"`
	AfterDeliveryID         string              `json:"after_delivery_id,omitempty"`
	NativePayload           string              `json:"native_payload,omitempty"`
	State                   PublicDeliveryState `json:"state" enum:"queued,admission_pending,admission_unknown,admitted,failed,cancel_requested,canceled" doc:"GC handoff state; admitted means the existing Sessions operation is authoritative, not model completion."`
	StateVersion            uint64              `json:"state_version"`
	ErrorCode               string              `json:"error_code,omitempty"`
	ErrorDetail             string              `json:"error_detail,omitempty"`
	Retryable               bool                `json:"retryable,omitempty"`
	CancelRequested         bool                `json:"cancel_requested,omitempty"`
	OperationStatus         string              `json:"operation_status,omitempty"`
	OperationOutcome        string              `json:"operation_outcome,omitempty"`
	OperationReason         string              `json:"operation_reason,omitempty"`
}

// PublicDeliveryLane bridges two coordination classes without pretending their
// writes share a transaction. Admit MUST use the existing Sessions journal and
// bind OperationKey to its complete payload. A lost acknowledgement is recovered
// by calling Admit again with exactly the same item; it MUST return the same op.
// Implementations must never send native input or call the legacy notify path.
type PublicDeliveryLane interface {
	Ready(context.Context) error
	ResolveRecipient(context.Context, PublicRecipient) (string, error)
	Prepare(context.Context, PublicDelivery, ConversationTranscriptRecord) (string, error)
	Admit(context.Context, PublicDelivery, ConversationTranscriptRecord) (string, error)
}

// PublicAdmissionStore atomically admits public messages in the Messaging
// class and relays their frozen delivery intents to the Sessions class.
type PublicAdmissionStore struct {
	store      beads.MailStore
	batcher    beads.GuardedBatcher
	prefix     string
	transcript *transcriptService
	lane       PublicDeliveryLane
}

// NewPublicAdmissionStore fails before public append when the Messaging WRITER
// cannot commit guarded batches. Native read acceleration does not qualify it.
func NewPublicAdmissionStore(store beads.MailStore, prefix string, lane PublicDeliveryLane) (*PublicAdmissionStore, error) {
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok || batcher == nil {
		return nil, fmt.Errorf("opening public admission: %w", beads.ErrGuardedBatchUnsupported)
	}
	if prefix == "" || strings.TrimSpace(prefix) != prefix || strings.ContainsAny(prefix, " /\\\t\r\n") {
		return nil, fmt.Errorf("%w: public admission namespace prefix required", ErrInvalidInput)
	}
	if lane == nil {
		return nil, fmt.Errorf("%w: durable Sessions lane required", ErrInvalidInput)
	}
	return &PublicAdmissionStore{store: store, batcher: batcher, prefix: prefix, transcript: newTranscriptService(store, sharedBindingLockPool(store)), lane: lane}, nil
}

// Ready verifies the two independent writers needed by this transport. A known
// unavailable Sessions journal must not become a public append with no executor.
func (s *PublicAdmissionStore) Ready(ctx context.Context) error { return s.lane.Ready(ctx) }

func publicHash(v any) string {
	b, _ := json.Marshal(v) // callers pass only typed JSON values with no custom marshaler
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *PublicAdmissionStore) rowID(kind string, identity any) string {
	return s.prefix + "-" + kind + "-" + publicHash(identity)[:32]
}

type publicPayload struct {
	Message    ExternalInboundMessage `json:"message"`
	Recipients []PublicRecipient      `json:"recipients"`
}

func normalizePublicInput(input PublicAdmissionInput) (PublicAdmissionInput, string, error) {
	ref, err := validateConversationRef(input.Message.Conversation)
	if err != nil {
		return input, "", err
	}
	if err := authorizeMutation(input.Caller, ref); err != nil {
		return input, "", err
	}
	input.RequestKey = strings.TrimSpace(input.RequestKey)
	input.Message.Conversation = ref
	input.Message.Actor.ID = strings.TrimSpace(input.Message.Actor.ID)
	if input.RequestKey == "" || input.Message.Actor.ID == "" {
		return input, "", fmt.Errorf("%w: public request key and actor ID required", ErrInvalidInput)
	}
	if len(input.Recipients) > beads.MaxGuardedItems-4 {
		return input, "", fmt.Errorf("%w: too many public recipients", ErrInvalidInput)
	}
	input.Message.ProviderMessageID = strings.TrimSpace(input.Message.ProviderMessageID)
	input.Message.Text = strings.TrimSpace(input.Message.Text)
	input.Message.ExplicitTarget = normalizeHandle(input.Message.ExplicitTarget)
	input.Message.ReplyToMessageID = strings.TrimSpace(input.Message.ReplyToMessageID)
	// DedupKey and arrival time are transport observations, not content.
	input.Message.DedupKey = ""
	input.Message.ReceivedAt = input.Message.ReceivedAt.UTC()
	input.Recipients = append([]PublicRecipient(nil), input.Recipients...)
	seen := make(map[string]bool)
	canonicalSeen := make(map[string]bool)
	for i := range input.Recipients {
		r := &input.Recipients[i]
		r.SessionID = strings.TrimSpace(r.SessionID)
		r.AgentIdentity = strings.TrimSpace(r.AgentIdentity)
		if (r.SessionID == "") == (r.AgentIdentity == "") {
			return input, "", fmt.Errorf("%w: exactly one recipient identity required", ErrInvalidInput)
		}
		policy, err := opjournal.NormalizeMessagePolicy(opjournal.MessagePolicy(r.Policy))
		if err != nil {
			return input, "", err
		}
		r.Policy = PublicMessagePolicy(policy)
		key := r.SessionID + "\x00" + r.AgentIdentity
		if seen[key] {
			return input, "", fmt.Errorf("%w: duplicate public recipient", ErrInvalidInput)
		}
		seen[key] = true
		if input.RecipientKeys != nil {
			canonical := input.RecipientKeys[key]
			if canonical == "" || canonicalSeen[canonical] {
				return input, "", fmt.Errorf("%w: duplicate or missing canonical recipient identity", ErrInvalidInput)
			}
			canonicalSeen[canonical] = true
		}
	}
	sort.Slice(input.Recipients, func(i, j int) bool {
		return input.Recipients[i].SessionID+"\x00"+input.Recipients[i].AgentIdentity < input.Recipients[j].SessionID+"\x00"+input.Recipients[j].AgentIdentity
	})
	if len(input.Message.Attachments) == 0 {
		input.Message.Attachments = nil
	}
	frozen := input.Message
	frozen.ReceivedAt = time.Time{}
	return input, publicHash(publicPayload{Message: frozen, Recipients: input.Recipients}), nil
}

// Recover returns an existing immutable admission before callers revalidate
// current routing or writer readiness. Changed payload or recipient policy is
// still a conflict; absence is not permission to bypass new-admission checks.
func (s *PublicAdmissionStore) Recover(ctx context.Context, input PublicAdmissionInput) (PublicReceipt, bool, error) {
	if err := ctx.Err(); err != nil {
		return PublicReceipt{}, false, err
	}
	input, digest, err := normalizePublicInput(input)
	if err != nil {
		return PublicReceipt{}, false, err
	}
	identity := struct {
		Conversation ConversationRef
		Actor, Key   string
	}{input.Message.Conversation, input.Message.Actor.ID, input.RequestKey}
	if r, found, err := s.readReceipt(s.rowID("xa", identity), digest); found || err != nil {
		return r, found, err
	}
	return s.readProviderReceipt(input.Message, digest)
}

// ReadAdmission projects durable receipt, transcript and recipient handoffs.
// OperationID means lane admission, never native completion.
func (s *PublicAdmissionStore) ReadAdmission(ctx context.Context, receipt PublicReceipt) (ConversationTranscriptRecord, []PublicDelivery, error) {
	if err := ctx.Err(); err != nil {
		return ConversationTranscriptRecord{}, nil, err
	}
	row, err := s.store.Get(receipt.TranscriptID)
	if err != nil {
		return ConversationTranscriptRecord{}, nil, err
	}
	record, err := decodeTranscriptBead(row)
	if err != nil {
		return record, nil, err
	}
	deliveries := make([]PublicDelivery, 0, len(receipt.DeliveryIDs))
	for _, id := range receipt.DeliveryIDs {
		row, err := s.store.Get(id)
		if err != nil {
			return record, nil, err
		}
		d, err := decodePublicDelivery(row)
		if err != nil {
			return record, nil, err
		}
		deliveries = append(deliveries, d)
	}
	return record, deliveries, nil
}

// Admit commits one transcript append, receipt and per-recipient outbox item in
// one guarded transaction. Any unknown write result is recovered by receipt ID;
// callers retain the same request key until its outcome is known.
func (s *PublicAdmissionStore) Admit(ctx context.Context, input PublicAdmissionInput) (PublicReceipt, error) {
	input, digest, err := normalizePublicInput(input)
	if err != nil {
		return PublicReceipt{}, err
	}
	identity := struct {
		Conversation ConversationRef
		Actor, Key   string
	}{input.Message.Conversation, input.Message.Actor.ID, input.RequestKey}
	id := s.rowID("xa", identity)
	for attempt := 0; attempt < 16; attempt++ {
		if err := ctx.Err(); err != nil {
			return PublicReceipt{}, err
		}
		if receipt, found, err := s.readReceipt(id, digest); err != nil || found {
			return receipt, err
		}
		if receipt, found, err := s.readProviderReceipt(input.Message, digest); err != nil || found {
			return receipt, err
		}
		// A receipt read remains available during a Sessions outage; a new
		// public append requires both writers to be ready.
		if err := s.Ready(ctx); err != nil {
			return PublicReceipt{}, err
		}
		receipt, items, err := s.planAdmission(input, id, digest)
		if errors.Is(err, errPublicPlanChanged) {
			continue
		}
		if errors.Is(err, ErrMessageConflict) {
			// A concurrent guarded admission can commit after our receipt
			// reads but before the provider-message transcript lookup. That
			// transcript and its receipt land together: re-read both pinned
			// receipt identities before classifying it as a legacy conflict.
			// The digest checks still refuse changed payloads or recipients.
			if receipt, found, recoverErr := s.readReceipt(id, digest); recoverErr != nil || found {
				return receipt, recoverErr
			}
			if receipt, found, recoverErr := s.readProviderReceipt(input.Message, digest); recoverErr != nil || found {
				return receipt, recoverErr
			}
		}
		if err != nil {
			return PublicReceipt{}, err
		}
		_, writeErr := s.batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: "admit public message and recipient intents", Items: items})
		if writeErr == nil {
			return receipt, nil
		}
		if receipt, found, err := s.readReceipt(id, digest); err != nil || found {
			return receipt, err
		}
		if receipt, found, err := s.readProviderReceipt(input.Message, digest); err != nil || found {
			return receipt, err
		}
		if errors.Is(writeErr, beads.ErrAlreadyExists) || beads.IsPreconditionFailed(writeErr) {
			continue
		}
		return PublicReceipt{}, fmt.Errorf("public admission outcome unknown; recover same request key: %w", writeErr)
	}
	return PublicReceipt{}, fmt.Errorf("public admission contention: %w", beads.ErrStoreUnavailable)
}

func (s *PublicAdmissionStore) providerReceiptID(msg ExternalInboundMessage) string {
	return s.rowID("xp", struct {
		Conversation ConversationRef
		MessageID    string
	}{msg.Conversation, msg.ProviderMessageID})
}

func (s *PublicAdmissionStore) readProviderReceipt(msg ExternalInboundMessage, digest string) (PublicReceipt, bool, error) {
	if msg.ProviderMessageID == "" {
		return PublicReceipt{}, false, nil
	}
	row, err := s.store.Get(s.providerReceiptID(msg))
	if errors.Is(err, beads.ErrNotFound) {
		return PublicReceipt{}, false, nil
	}
	if err != nil {
		return PublicReceipt{}, false, err
	}
	if !hasLabel(row, "gc:extmsg-provider-receipt") || row.Description == "" {
		return PublicReceipt{}, false, fmt.Errorf("%w: invalid provider receipt", ErrInvariantViolation)
	}
	receipt, found, err := s.readReceipt(row.Description, digest)
	if err == nil && !found {
		err = fmt.Errorf("%w: provider receipt has no admission", ErrInvariantViolation)
	}
	return receipt, true, err
}

func (s *PublicAdmissionStore) readReceipt(id, digest string) (PublicReceipt, bool, error) {
	b, err := s.store.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return PublicReceipt{}, false, nil
	}
	if err != nil {
		return PublicReceipt{}, false, err
	}
	var r PublicReceipt
	if !hasLabel(b, publicAdmissionLabel) || json.Unmarshal([]byte(b.Description), &r) != nil || r.ID != id || r.TranscriptID == "" || r.PayloadDigest == "" {
		return r, false, fmt.Errorf("%w: invalid public admission receipt %s", ErrInvariantViolation, id)
	}
	if digest != "" && r.PayloadDigest != digest {
		return PublicReceipt{}, true, ErrMessageConflict
	}
	r.CancelRequested = b.Metadata[publicCancelKey] == "true"
	return r, true, nil
}

func (s *PublicAdmissionStore) planAdmission(input PublicAdmissionInput, id, digest string) (PublicReceipt, []beads.GuardedItem, error) {
	ref := input.Message.Conversation
	if providerID := input.Message.ProviderMessageID; providerID != "" {
		existing, err := s.transcript.findTranscriptByProviderMessageLocked(ref, providerID)
		if err != nil {
			return PublicReceipt{}, nil, err
		}
		if existing != nil {
			return PublicReceipt{}, nil, fmt.Errorf("%w: existing public transcript has no durable admission receipt", ErrMessageConflict)
		}
	}
	stateItem, sequence, err := s.transcript.planGuardedTranscriptState(ref, TranscriptProvenanceLive, s.prefix)
	if err != nil {
		return PublicReceipt{}, nil, err
	}
	transcriptID := s.rowID("xt", id)
	msg := input.Message
	actor, _ := marshalOptionalJSON(msg.Actor)
	attachments, _ := marshalOptionalJSON(msg.Attachments)
	labels := []string{"gc:extmsg-transcript", labelTranscriptBase, transcriptConversationLabel(ref), transcriptBucketLabel(ref, transcriptBucket(sequence))}
	if msg.ProviderMessageID != "" {
		labels = append(labels, transcriptProviderMessageLabel(ref, msg.ProviderMessageID))
	}
	transcript := beads.Bead{ID: transcriptID, Title: fmt.Sprintf("%s#%d", conversationTitle(ref), sequence), Type: "task", Status: "open", Description: msg.Text, Labels: labels, Metadata: map[string]string{
		"schema_version": strconv.Itoa(schemaVersion), "scope_id": ref.ScopeID, "provider": ref.Provider, "account_id": ref.AccountID, "conversation_id": ref.ConversationID, "parent_conversation_id": ref.ParentConversationID, "conversation_kind": string(ref.Kind), "sequence": strconv.FormatInt(sequence, 10), "kind": string(TranscriptMessageInbound), "provenance": string(TranscriptProvenanceLive), "provider_message_id": msg.ProviderMessageID, "explicit_target": msg.ExplicitTarget, "reply_to_message_id": msg.ReplyToMessageID, "created_at": formatTime(zeroNow(msg.ReceivedAt)), "actor_json": actor, "attachments_json": attachments,
	}}
	receipt := PublicReceipt{ID: id, RequestKey: input.RequestKey, PayloadDigest: digest, TranscriptID: transcriptID, Sequence: sequence, TargetSession: input.Route.TargetSessionID, TargetAgent: input.Route.TargetAgentName}
	items := []beads.GuardedItem{stateItem, {Create: &transcript}}
	for _, target := range input.Recipients {
		deliveryID := s.rowID("xd", struct {
			Receipt string
			Target  PublicRecipient
		}{id, target})
		delivery := PublicDelivery{ID: deliveryID, ReceiptID: id, TranscriptID: transcriptID, Conversation: ref, Recipient: target, ResolvedSessionID: target.SessionID, ExplicitTargetSessionID: input.Route.ExplicitTargetSessionID, OperationKey: "extmsg:" + deliveryID}
		delivery.Sequence = sequence
		delivery.State = PublicDeliveryQueued
		delivery.StateVersion = 1
		delivery.RecipientKey = input.RecipientKeys[target.SessionID+"\x00"+target.AgentIdentity]
		if delivery.RecipientKey == "" {
			delivery.RecipientKey = target.SessionID + "\x00" + target.AgentIdentity
		}
		lastKey := "gc.extmsg.last_delivery." + publicHash(delivery.RecipientKey)[:24]
		if stateItem.Update != nil {
			previous, err := s.store.Get(stateItem.Update.ID)
			if err != nil {
				return PublicReceipt{}, nil, err
			}
			if previous.Revision != *stateItem.Update.ExpectedRevision {
				return PublicReceipt{}, nil, errPublicPlanChanged
			}
			delivery.AfterDeliveryID = previous.Metadata[lastKey]
			stateItem.Update.Opts.Metadata[lastKey] = deliveryID
		} else {
			stateItem.Create.Metadata[lastKey] = deliveryID
		}
		items[0] = stateItem
		encoded, _ := json.Marshal(delivery)
		items = append(items, beads.GuardedItem{Create: &beads.Bead{ID: deliveryID, Title: "public message recipient intent", Type: "task", Status: "open", Description: string(encoded), Labels: []string{publicOutboxLabel}, Metadata: map[string]string{publicChangePendingKey: "true"}}})
		receipt.DeliveryIDs = append(receipt.DeliveryIDs, deliveryID)
	}
	encoded, _ := json.Marshal(receipt)
	items = append(items, beads.GuardedItem{Create: &beads.Bead{ID: id, Title: "public message admission receipt", Type: "task", Status: "open", Description: string(encoded), Labels: []string{publicAdmissionLabel}}})
	if msg.ProviderMessageID != "" {
		items = append(items, beads.GuardedItem{Create: &beads.Bead{ID: s.providerReceiptID(msg), Title: "public provider-message receipt", Type: "task", Status: "open", Description: id, Labels: []string{"gc:extmsg-provider-receipt"}}})
	}
	return receipt, items, nil
}

// Receipt recovers a committed admission by opaque receipt ID. It never creates
// work or interprets absence as permission to send another public message.
func (s *PublicAdmissionStore) Receipt(ctx context.Context, id string) (PublicReceipt, error) {
	if err := ctx.Err(); err != nil {
		return PublicReceipt{}, err
	}
	r, found, err := s.readReceipt(id, "")
	if err == nil && !found {
		err = beads.ErrNotFound
	}
	return r, err
}

// Pending returns durable recipient intents, including unavailable/cold targets.
func (s *PublicAdmissionStore) Pending(ctx context.Context, limit int) ([]PublicDelivery, error) {
	out, _, err := s.PendingPage(ctx, limit, nil)
	return out, err
}

// PendingPage keyset-pages open intents so unavailable recipients at the front
// of the outbox cannot starve later, independent recipients.
func (s *PublicAdmissionStore) PendingPage(ctx context.Context, limit int, after *beads.SeekBoundary) ([]PublicDelivery, *beads.SeekBoundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.store.List(beads.ListQuery{Label: publicOutboxLabel, Status: "open", Limit: limit, Sort: beads.SortCreatedAsc, SeekAfter: after})
	if err != nil {
		return nil, nil, err
	}
	out := make([]PublicDelivery, 0, len(rows))
	for _, row := range rows {
		d, err := decodePublicDelivery(row)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, d)
	}
	var next *beads.SeekBoundary
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = &beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

func decodePublicDelivery(row beads.Bead) (PublicDelivery, error) {
	var d PublicDelivery
	if !hasLabel(row, publicOutboxLabel) || json.Unmarshal([]byte(row.Description), &d) != nil || d.ID != row.ID || d.ReceiptID == "" || d.TranscriptID == "" || d.OperationKey != "extmsg:"+d.ID {
		return d, fmt.Errorf("%w: invalid public delivery %s", ErrInvariantViolation, row.ID)
	}
	if d.State == "" {
		if d.OperationID != "" {
			d.State = PublicDeliveryAdmitted
		} else {
			d.State = PublicDeliveryQueued
		}
	}
	return d, nil
}

// Relay hands off one durable intent. The binding write precedes Sessions
// admission; the completion write follows it. Crashes at either boundary replay
// the exact same target and operation key. The operation journal owns execution.
func (s *PublicAdmissionStore) Relay(ctx context.Context, id string) (PublicDelivery, error) {
	lane := s.lane
	for attempt := 0; attempt < 16; attempt++ {
		if err := ctx.Err(); err != nil {
			return PublicDelivery{}, err
		}
		row, err := s.store.Get(id)
		if err != nil {
			return PublicDelivery{}, err
		}
		d, err := decodePublicDelivery(row)
		if err != nil {
			return d, err
		}
		if d.CancelRequested && !publicDeliveryTerminal(d) {
			return s.relayPublicCancel(ctx, row, d)
		}
		if publicDeliveryTerminal(d) {
			return d, nil
		}
		if d.OperationID != "" {
			return d, nil
		}
		if d.AfterDeliveryID != "" {
			priorRow, err := s.store.Get(d.AfterDeliveryID)
			if err != nil {
				return d, err
			}
			prior, err := decodePublicDelivery(priorRow)
			if err != nil {
				return d, err
			}
			if prior.OperationID == "" && !publicDeliveryTerminal(prior) {
				return d, fmt.Errorf("%w: earlier public recipient intent has not reached its lane", beads.ErrStoreUnavailable)
			}
		}
		if d.ResolvedSessionID == "" {
			d.ResolvedSessionID, err = lane.ResolveRecipient(ctx, d.Recipient)
			if err != nil {
				return s.recordPublicDeliveryError(ctx, row, d, err)
			}
			if strings.TrimSpace(d.ResolvedSessionID) == "" {
				return d, fmt.Errorf("%w: recipient not ready", beads.ErrStoreUnavailable)
			}
			if err = s.writeDelivery(ctx, row, d, false); err != nil {
				if beads.IsPreconditionFailed(err) {
					continue
				}
				return d, err
			}
			// Re-read after any durable resolution before making a Sessions write.
			continue
		}
		transcript, err := s.store.Get(d.TranscriptID)
		if err != nil {
			return d, err
		}
		record, err := decodeTranscriptBead(transcript)
		if err != nil {
			return d, err
		}
		if d.NativePayload == "" {
			d.NativePayload, err = lane.Prepare(ctx, d, record)
			if err != nil {
				return s.recordPublicDeliveryError(ctx, row, d, err)
			}
			if strings.TrimSpace(d.NativePayload) == "" {
				return d, fmt.Errorf("%w: no native public-message payload", ErrInvariantViolation)
			}
			if err = s.writeDelivery(ctx, row, d, false); err != nil {
				if beads.IsPreconditionFailed(err) {
					continue
				}
				return d, err
			}
			continue
		}
		if d.State == PublicDeliveryQueued {
			d.State = PublicDeliveryAdmissionPending
			d.ErrorCode, d.ErrorDetail, d.Retryable = "", "", false
			if err := s.writeDelivery(ctx, row, d, false); err != nil {
				if beads.IsPreconditionFailed(err) {
					continue
				}
				return d, err
			}
			continue
		}
		d.OperationID, err = lane.Admit(ctx, d, record)
		if err != nil {
			return s.recordPublicDeliveryError(ctx, row, d, err)
		}
		if strings.TrimSpace(d.OperationID) == "" {
			return d, fmt.Errorf("%w: lane acknowledged no operation", ErrInvariantViolation)
		}
		d.State = PublicDeliveryAdmitted
		d.ErrorCode, d.ErrorDetail, d.Retryable = "", "", false
		if err = s.writeDelivery(ctx, row, d, true); err != nil {
			if beads.IsPreconditionFailed(err) {
				continue
			}
			return d, err
		}
		return d, nil
	}
	return PublicDelivery{}, fmt.Errorf("public relay contention: %w", beads.ErrStoreUnavailable)
}

func (s *PublicAdmissionStore) writeDelivery(ctx context.Context, row beads.Bead, d PublicDelivery, done bool) error {
	previous, err := decodePublicDelivery(row)
	if err != nil {
		return err
	}
	if d.State == "" {
		d.State = previous.State
	}
	metadata := map[string]string{}
	if publicDeliveryChanged(previous, d) {
		d.StateVersion = previous.StateVersion + 1
		metadata[publicChangePendingKey] = "true"
	} else {
		d.StateVersion = previous.StateVersion
	}
	encoded, err := json.Marshal(d)
	if err != nil {
		return err
	}
	opts := beads.UpdateOpts{Description: ptrPublic(string(encoded)), Metadata: metadata}
	if done {
		opts.Status = ptrPublic("closed")
	}
	_, err = s.batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: "record public recipient lane handoff", Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: opts}}}})
	return err
}

func ptrPublic(s string) *string { return &s }

// PendingInvalidations returns receipts whose public-history invalidation is
// not yet acknowledged. Receipt status is independent of recipient execution.
func (s *PublicAdmissionStore) PendingInvalidations(ctx context.Context, limit int, after *beads.SeekBoundary) ([]PublicReceipt, *beads.SeekBoundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.store.List(beads.ListQuery{Label: publicAdmissionLabel, Status: "open", Limit: limit, Sort: beads.SortCreatedAsc, SeekAfter: after})
	if err != nil {
		return nil, nil, err
	}
	out := make([]PublicReceipt, 0, len(rows))
	for _, row := range rows {
		receipt, found, err := s.readReceipt(row.ID, "")
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return nil, nil, fmt.Errorf("%w: admission disappeared", ErrInvariantViolation)
		}
		out = append(out, receipt)
	}
	var next *beads.SeekBoundary
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = &beads.SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return out, next, nil
}

// Invalidate emits through an acknowledged existing event recorder, then marks
// the receipt. A crash/unknown ack between those writes may repeat the SAME
// receipt/transcript invalidation. It never appends or dispatches again.
func (s *PublicAdmissionStore) Invalidate(ctx context.Context, id string, emit func(PublicReceipt, ConversationTranscriptRecord) error) error {
	for range 16 {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := s.store.Get(id)
		if err != nil {
			return err
		}
		receipt, found, err := s.readReceipt(id, "")
		if err != nil {
			return err
		}
		if !found {
			return beads.ErrNotFound
		}
		if row.Status == "closed" {
			return nil
		}
		record, _, err := s.ReadAdmission(ctx, receipt)
		if err != nil {
			return err
		}
		if err := emit(receipt, record); err != nil {
			return err
		}
		_, err = s.batcher.ApplyGuarded(ctx, beads.GuardedBatch{
			Reason: "acknowledge public history invalidation",
			Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{
				ID: id, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: ptrPublic("closed")},
			}}},
		})
		if beads.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("public invalidation contention: %w", beads.ErrStoreUnavailable)
}
