package extmsg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	publicCancelKey        = beadmeta.ExtMsgCancelRequestedMetadataKey
	publicChangePendingKey = beadmeta.ExtMsgDeliveryChangePendingMetadataKey
)

// PublicDeliveryState describes handoff to GC's Sessions lane, not model work.
type PublicDeliveryState string

// PublicDeliveryQueued and the following states project the durable handoff
// from Messaging to the authoritative Sessions operation.
const (
	PublicDeliveryQueued           PublicDeliveryState = "queued"
	PublicDeliveryAdmissionPending PublicDeliveryState = "admission_pending"
	PublicDeliveryAdmissionUnknown PublicDeliveryState = "admission_unknown"
	PublicDeliveryAdmitted         PublicDeliveryState = "admitted"
	PublicDeliveryFailed           PublicDeliveryState = "failed"
	PublicDeliveryCancelRequested  PublicDeliveryState = "cancel_requested"
	PublicDeliveryCanceled         PublicDeliveryState = "canceled"
)

// PublicDeliveryError marks a definitive pre-admission refusal. Unclassified
// errors are retryable. Nothing becomes failed after a native request was armed:
// its operation may exist and must be recovered or canceled under the same key.
type PublicDeliveryError struct {
	Code  string
	Cause error
}

func (e *PublicDeliveryError) Error() string { return e.Code + ": " + e.Cause.Error() }
func (e *PublicDeliveryError) Unwrap() error { return e.Cause }

// PublicCancelResult is observed from the SAME frozen Sessions request.
// Terminal outcomes win over cancellation; no requested cancel claims success.
type PublicCancelResult struct {
	OperationID string
	Status      string
	Outcome     string
	Reason      string
	Terminal    bool
	Canceled    bool
}

// PublicCancellationLane resolves cancel against the existing operation journal,
// reserving a closed canceled proposal at its pinned key when none exists.
type PublicCancellationLane interface {
	Cancel(context.Context, PublicDelivery) (PublicCancelResult, error)
}

func publicDeliveryTerminal(d PublicDelivery) bool {
	return d.State == PublicDeliveryAdmitted || d.State == PublicDeliveryFailed || d.State == PublicDeliveryCanceled
}

func publicDeliveryChanged(a, b PublicDelivery) bool {
	a.NativePayload, b.NativePayload = "", ""
	a.StateVersion, b.StateVersion = 0, 0
	return !reflect.DeepEqual(a, b)
}

func (s *PublicAdmissionStore) recordPublicDeliveryError(ctx context.Context, row beads.Bead, d PublicDelivery, cause error) (PublicDelivery, error) {
	var refusal *PublicDeliveryError
	permanent := errors.As(cause, &refusal) && d.NativePayload == "" && d.OperationID == ""
	d.ErrorCode = "delivery_unavailable"
	if refusal != nil {
		d.ErrorCode = refusal.Code
	}
	d.ErrorDetail = cause.Error()
	d.Retryable = !permanent
	switch {
	case permanent:
		d.State = PublicDeliveryFailed
	case d.CancelRequested:
		d.State = PublicDeliveryCancelRequested
	case d.NativePayload != "":
		d.State = PublicDeliveryAdmissionUnknown
	default:
		d.State = PublicDeliveryQueued
	}
	previous, err := decodePublicDelivery(row)
	if err != nil {
		return d, err
	}
	if !publicDeliveryChanged(previous, d) {
		return d, cause
	}
	if err := s.writeDelivery(ctx, row, d, permanent); err != nil {
		return d, errors.Join(cause, err)
	}
	return d, cause
}

// RequestCancel monotonically cancels the whole already-admitted public message.
// The receipt flag and every recipient flag commit together in Messaging. This
// is NOT atomic with Sessions. A prepared/unknown handoff stays cancel_requested
// until the lane's same-key cancellation/tombstone supplies its actual outcome.
func (s *PublicAdmissionStore) RequestCancel(ctx context.Context, id string) (PublicReceipt, error) {
	for range 16 {
		if err := ctx.Err(); err != nil {
			return PublicReceipt{}, err
		}
		row, err := s.store.Get(id)
		if err != nil {
			return PublicReceipt{}, err
		}
		receipt, found, err := s.readReceipt(id, "")
		if err != nil {
			return receipt, err
		}
		if !found {
			return receipt, beads.ErrNotFound
		}
		if receipt.CancelRequested {
			return receipt, nil
		}
		items := []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: id, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{publicCancelKey: "true"}}}}}
		for _, deliveryID := range receipt.DeliveryIDs {
			deliveryRow, err := s.store.Get(deliveryID)
			if err != nil {
				return receipt, err
			}
			d, err := decodePublicDelivery(deliveryRow)
			if err != nil {
				return receipt, err
			}
			d.CancelRequested = true
			if d.State != PublicDeliveryFailed && d.State != PublicDeliveryCanceled {
				if d.NativePayload == "" && d.OperationID == "" {
					d.State = PublicDeliveryCanceled
					d.ErrorCode, d.ErrorDetail, d.Retryable = "", "", false
				} else {
					d.State = PublicDeliveryCancelRequested
				}
			}
			d.StateVersion++
			encoded, err := json.Marshal(d)
			if err != nil {
				return receipt, err
			}
			status := "open"
			if publicDeliveryTerminal(d) {
				status = "closed"
			}
			items = append(items, beads.GuardedItem{Update: &beads.GuardedUpdate{ID: deliveryID, ExpectedRevision: &deliveryRow.Revision, Opts: beads.UpdateOpts{
				Description: ptrPublic(string(encoded)), Status: ptrPublic(status), Metadata: map[string]string{publicChangePendingKey: "true"},
			}}})
		}
		_, writeErr := s.batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: "request cancellation of public message recipients", Items: items})
		current, found, readErr := s.readReceipt(id, "")
		if readErr != nil {
			return receipt, readErr
		}
		if found && current.CancelRequested {
			return current, nil
		}
		if writeErr == nil {
			return receipt, fmt.Errorf("%w: cancellation commit missing", ErrInvariantViolation)
		}
		if beads.IsPreconditionFailed(writeErr) || errors.Is(writeErr, beads.ErrAlreadyExists) {
			continue
		}
		return receipt, fmt.Errorf("public cancellation outcome unknown; retry same request: %w", writeErr)
	}
	return PublicReceipt{}, fmt.Errorf("public cancellation contention: %w", beads.ErrStoreUnavailable)
}

func (s *PublicAdmissionStore) relayPublicCancel(ctx context.Context, row beads.Bead, d PublicDelivery) (PublicDelivery, error) {
	lane, ok := s.lane.(PublicCancellationLane)
	if !ok {
		return s.recordPublicDeliveryError(ctx, row, d, fmt.Errorf("%w: lane has no public cancellation capability", beads.ErrStoreUnavailable))
	}
	result, err := lane.Cancel(ctx, d)
	if result.OperationID != "" {
		d.OperationID = result.OperationID
	}
	if err != nil {
		return s.recordPublicDeliveryError(ctx, row, d, err)
	}
	d.OperationStatus, d.OperationOutcome = result.Status, result.Outcome
	d.OperationReason = result.Reason
	d.ErrorCode, d.ErrorDetail, d.Retryable = "", "", false
	if result.Terminal {
		if result.Canceled {
			d.State = PublicDeliveryCanceled
		} else {
			d.State = PublicDeliveryAdmitted
		}
	} else {
		d.State = PublicDeliveryCancelRequested
	}
	if err := s.writeDelivery(ctx, row, d, result.Terminal); err != nil {
		return d, err
	}
	return d, nil
}

// PendingDeliveryChanges returns unacknowledged state projections even after
// their outbox rows close. Pagination is independent of live delivery retries.
func (s *PublicAdmissionStore) PendingDeliveryChanges(ctx context.Context, limit int, after *beads.SeekBoundary) ([]PublicDelivery, *beads.SeekBoundary, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.store.List(beads.ListQuery{Label: publicOutboxLabel, Metadata: map[string]string{publicChangePendingKey: "true"}, IncludeClosed: true, Limit: limit, Sort: beads.SortCreatedAsc, SeekAfter: after})
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
		next = &beads.SeekBoundary{ID: last.ID, CreatedAt: last.CreatedAt}
	}
	return out, next, nil
}

// InvalidateDelivery projects the current state version through an existing
// acknowledged event sink. A lost ack can repeat that version, never regress
// state; consumers use DeliveryID+StateVersion and reconcile the receipt.
func (s *PublicAdmissionStore) InvalidateDelivery(ctx context.Context, id string, emit func(PublicDelivery) error) error {
	for range 16 {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := s.store.Get(id)
		if err != nil {
			return err
		}
		if row.Metadata[publicChangePendingKey] != "true" {
			return nil
		}
		d, err := decodePublicDelivery(row)
		if err != nil {
			return err
		}
		if err := emit(d); err != nil {
			return err
		}
		_, err = s.batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: "acknowledge public delivery state", Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{ID: id, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{publicChangePendingKey: "false"}}}}}})
		if beads.IsPreconditionFailed(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("public delivery event contention: %w", beads.ErrStoreUnavailable)
}
