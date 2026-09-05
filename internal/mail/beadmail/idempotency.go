package beadmail

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

const (
	idempotencyKeyMetadataKey         = "mail.idempotency_key"
	idempotencyRecordMetadataKey      = "mail.idempotency_record"
	idempotencyFingerprintMetadataKey = "mail.idempotency_fingerprint"
	idempotencyMessageIDMetadataKey   = "mail.idempotency_message_id"
	idempotencyThreadIDMetadataKey    = "mail.idempotency_thread_id"
	idempotencyRecordIDMetadataKey    = "mail.idempotency_record_id"
	idempotencyRecordMarker           = "true"
	idempotencyTxAttempts             = 4
	idempotencyRetryDelay             = 10 * time.Millisecond
)

// SendIdempotent atomically creates one durable key record and one ephemeral
// message in the provider's messaging-class store. Exact retries return the
// original message; a key reused for different immutable fields fails closed.
// Stores without real transaction rollback are refused before any write.
func (p *Provider) SendIdempotent(request mail.IdempotentSendRequest) (mail.IdempotentSendResult, error) {
	if err := mail.ValidateIdempotencyKey(request.IdempotencyKey); err != nil {
		return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: %w", err)
	}
	if request.To == "" {
		return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: recipient is required")
	}
	if p == nil || p.store == nil || !beads.StoreSupportsAtomicTx(p.store) {
		return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: %w: messaging store %T does not provide atomic transaction rollback", mail.ErrIdempotencyUnsupported, idempotencyStore(p))
	}
	prefixer, ok := p.store.(interface{ IDPrefix() string })
	if !ok {
		return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: %w: messaging store %T does not declare its ID namespace", mail.ErrIdempotencyUnsupported, p.store)
	}
	prefix := strings.Trim(strings.ToLower(strings.TrimSpace(prefixer.IDPrefix())), "-")
	if prefix == "" {
		return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: %w: messaging store %T declares an empty ID namespace", mail.ErrIdempotencyUnsupported, p.store)
	}
	keyDigest := sha256.Sum256([]byte(request.IdempotencyKey))
	digest := hex.EncodeToString(keyDigest[:])
	recordID := prefix + "-mail-idem-" + digest
	messageID := prefix + "-mail-msg-" + digest
	fingerprint := idempotentRequestFingerprint(request)

	if replay, found, err := p.idempotentReplay(recordID, messageID, fingerprint, request); found || err != nil {
		return replay, err
	}

	resolvedFrom, routeMetadata, err := p.resolveSenderRoute(request.From)
	if err != nil {
		return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: %w", err)
	}
	threadID := generateThreadID()
	createdAt := time.Now().Round(0).UTC()
	record := beads.Bead{
		ID:          recordID,
		Title:       "mail idempotency " + digest[:16],
		Status:      "closed",
		Type:        messageBeadType,
		Assignee:    request.To,
		From:        resolvedFrom,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
		NoHistory:   true,
		Description: "Durable idempotency record for message " + messageID,
		Metadata: map[string]string{
			idempotencyKeyMetadataKey:         request.IdempotencyKey,
			idempotencyRecordMetadataKey:      idempotencyRecordMarker,
			idempotencyFingerprintMetadataKey: fingerprint,
			idempotencyMessageIDMetadataKey:   messageID,
			idempotencyThreadIDMetadataKey:    threadID,
		},
	}
	messageMetadata := routeMetadata
	if messageMetadata == nil {
		messageMetadata = make(map[string]string, 2)
	}
	messageMetadata[idempotencyRecordIDMetadataKey] = recordID
	messageMetadata[idempotencyFingerprintMetadataKey] = fingerprint
	messageBead := beads.Bead{
		ID:          messageID,
		Title:       deriveSendTitle(request.Subject, request.Body),
		Description: request.Body,
		Type:        messageBeadType,
		Assignee:    request.To,
		From:        resolvedFrom,
		Labels:      []string{"thread:" + threadID},
		Metadata:    messageMetadata,
		Ephemeral:   true,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}

	var created beads.Bead
	var txErr error
	for attempt := 1; attempt <= idempotencyTxAttempts; attempt++ {
		txErr = p.store.Tx("gc: send mail idempotently", func(tx beads.Tx) error {
			storedRecord, err := tx.Create(record)
			if err != nil {
				return fmt.Errorf("creating idempotency record: %w", err)
			}
			if storedRecord.ID != recordID {
				return fmt.Errorf("creating idempotency record: store replaced pinned id %q with %q", recordID, storedRecord.ID)
			}
			created, err = tx.Create(messageBead)
			if err != nil {
				return fmt.Errorf("creating message: %w", err)
			}
			if created.ID != messageID {
				return fmt.Errorf("creating message: store replaced pinned id %q with %q", messageID, created.ID)
			}
			return nil
		})
		if txErr == nil {
			return mail.IdempotentSendResult{Message: beadToMessage(created), Created: true}, nil
		}
		if replay, found, replayErr := p.idempotentReplay(recordID, messageID, fingerprint, request); found || replayErr != nil {
			return replay, replayErr
		}
		if attempt < idempotencyTxAttempts {
			time.Sleep(idempotencyRetryDelay)
		}
	}
	return mail.IdempotentSendResult{}, fmt.Errorf("beadmail idempotent send: atomic create failed after %d attempts: %w", idempotencyTxAttempts, txErr)
}

func idempotencyStore(p *Provider) beads.Store {
	if p == nil {
		return nil
	}
	return p.store
}

func idempotentRequestFingerprint(request mail.IdempotentSendRequest) string {
	h := sha256.New()
	var size [8]byte
	for _, field := range []string{request.From, request.To, request.Subject, request.Body} {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// idempotentReplay returns found=false only when the deterministic key record
// is absent. Any present-but-conflicting or malformed record is a hard error.
func (p *Provider) idempotentReplay(recordID, messageID, fingerprint string, request mail.IdempotentSendRequest) (mail.IdempotentSendResult, bool, error) {
	record, err := p.store.Get(recordID)
	if errors.Is(err, beads.ErrNotFound) {
		return mail.IdempotentSendResult{}, false, nil
	}
	if err != nil {
		return mail.IdempotentSendResult{}, false, fmt.Errorf("beadmail idempotent send: reading key record: %w", err)
	}
	if record.Type != messageBeadType || record.Metadata[idempotencyRecordMetadataKey] != idempotencyRecordMarker || record.Metadata[idempotencyKeyMetadataKey] != request.IdempotencyKey || record.Metadata[idempotencyFingerprintMetadataKey] != fingerprint {
		return mail.IdempotentSendResult{}, true, fmt.Errorf("beadmail idempotent send: %w", mail.ErrIdempotencyConflict)
	}
	if storedMessageID := strings.TrimSpace(record.Metadata[idempotencyMessageIDMetadataKey]); storedMessageID == "" || storedMessageID != messageID {
		return mail.IdempotentSendResult{}, true, fmt.Errorf("beadmail idempotent send: %w: key record has an invalid message id", mail.ErrIdempotencyRecordCorrupt)
	}
	threadID := strings.TrimSpace(record.Metadata[idempotencyThreadIDMetadataKey])
	if threadID == "" {
		return mail.IdempotentSendResult{}, true, fmt.Errorf("beadmail idempotent send: %w: key record has no thread id", mail.ErrIdempotencyRecordCorrupt)
	}
	messageBead, err := p.store.Get(messageID)
	if err == nil {
		if messageBead.Type != messageBeadType || messageBead.Metadata[idempotencyRecordIDMetadataKey] != recordID || messageBead.Metadata[idempotencyFingerprintMetadataKey] != fingerprint {
			return mail.IdempotentSendResult{}, true, fmt.Errorf("beadmail idempotent send: %w: mapped message does not match its key record", mail.ErrIdempotencyRecordCorrupt)
		}
		return mail.IdempotentSendResult{Message: beadToMessage(messageBead), Created: false}, true, nil
	}
	if !errors.Is(err, beads.ErrNotFound) {
		return mail.IdempotentSendResult{}, true, fmt.Errorf("beadmail idempotent send: reading original message: %w", err)
	}
	// Archive/purge may remove the ephemeral message, but the durable record
	// still owns the key forever. Return the original ID without recreating it.
	return mail.IdempotentSendResult{Message: mail.Message{
		ID:        messageID,
		From:      record.From,
		To:        record.Assignee,
		Subject:   deriveSendTitle(request.Subject, request.Body),
		Body:      request.Body,
		CreatedAt: record.CreatedAt,
		ThreadID:  threadID,
	}, Created: false}, true, nil
}

var _ mail.IdempotentSender = (*Provider)(nil)
