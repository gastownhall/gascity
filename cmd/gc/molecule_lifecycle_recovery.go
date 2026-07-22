package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

const moleculeLifecycleVersionV1 = "v1"

var (
	// errMoleculeLifecycleAlreadyClosed lets the autoclose caller distinguish a
	// clean lost transition from a failed durable prepare. When a closed bead
	// also carries a pending marker, prepare joins this with
	// errMoleculeLifecyclePending so recovery can still be notified.
	errMoleculeLifecycleAlreadyClosed = errors.New("molecule lifecycle root is already closed")
	// errMoleculeLifecyclePending prevents a new writer from replacing an intent
	// that may already own publication for the same root.
	errMoleculeLifecyclePending = errors.New("molecule lifecycle publication is already pending")
	// errMoleculeLifecycleIneligible retains a prepared intent without asking
	// the controller to hot-retry. A later graph mutation or periodic recovery
	// may make the exact same durable intent eligible again.
	errMoleculeLifecycleIneligible = errors.New("prepared molecule lifecycle intent is not currently eligible")
)

// moleculeLifecycleIntent is the durable, versioned ownership record for the
// lifecycle events belonging to a capability-less molecule close. Its random
// ID prevents a delayed publisher from emitting or clearing a racing writer's
// replacement intent.
type moleculeLifecycleIntent struct {
	Version     string    `json:"version"`
	IntentID    string    `json:"intent_id"`
	FromStatus  string    `json:"from_status"`
	Actor       string    `json:"actor"`
	RequestedAt time.Time `json:"requested_at"`
	CloseReason string    `json:"close_reason"`
}

func newMoleculeLifecycleIntent(fromStatus, actor, closeReason string, requestedAt time.Time) (moleculeLifecycleIntent, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return moleculeLifecycleIntent{}, fmt.Errorf("generate molecule lifecycle intent id: %w", err)
	}
	intent := moleculeLifecycleIntent{
		Version:     moleculeLifecycleVersionV1,
		IntentID:    hex.EncodeToString(id[:]),
		FromStatus:  strings.TrimSpace(fromStatus),
		Actor:       strings.TrimSpace(actor),
		RequestedAt: requestedAt.UTC(),
		CloseReason: strings.TrimSpace(closeReason),
	}
	if err := validateMoleculeLifecycleIntent(intent); err != nil {
		return moleculeLifecycleIntent{}, err
	}
	return intent, nil
}

func marshalMoleculeLifecycleIntent(intent moleculeLifecycleIntent) (string, error) {
	if err := validateMoleculeLifecycleIntent(intent); err != nil {
		return "", err
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("marshal molecule lifecycle intent: %w", err)
	}
	return string(raw), nil
}

func decodeMoleculeLifecycleIntent(raw string) (moleculeLifecycleIntent, error) {
	if err := validateMoleculeLifecycleIntentJSON(raw); err != nil {
		return moleculeLifecycleIntent{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var intent moleculeLifecycleIntent
	if err := decoder.Decode(&intent); err != nil {
		return moleculeLifecycleIntent{}, fmt.Errorf("decode molecule lifecycle intent: %w", err)
	}
	if err := validateMoleculeLifecycleIntent(intent); err != nil {
		return moleculeLifecycleIntent{}, err
	}
	return intent, nil
}

// validateMoleculeLifecycleIntentJSON rejects duplicate and trailing fields in
// addition to decodeMoleculeLifecycleIntent's unknown-field check. The intent
// lives in mutable metadata, so recovery treats its JSON as untrusted input.
func validateMoleculeLifecycleIntentJSON(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode molecule lifecycle intent: %w", err)
	}
	if opening != json.Delim('{') {
		return errors.New("decode molecule lifecycle intent: expected JSON object")
	}
	seen := make(map[string]struct{}, 6)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode molecule lifecycle intent field: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("decode molecule lifecycle intent: non-string field name")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("decode molecule lifecycle intent: duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode molecule lifecycle intent field %q: %w", key, err)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode molecule lifecycle intent: %w", err)
	}
	if closing != json.Delim('}') {
		return errors.New("decode molecule lifecycle intent: expected end of JSON object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode molecule lifecycle intent: trailing JSON value")
		}
		return fmt.Errorf("decode molecule lifecycle intent trailing data: %w", err)
	}
	return nil
}

func validateMoleculeLifecycleIntent(intent moleculeLifecycleIntent) error {
	if intent.Version != moleculeLifecycleVersionV1 {
		return fmt.Errorf("validate molecule lifecycle intent: unsupported version %q", intent.Version)
	}
	decodedID, err := hex.DecodeString(intent.IntentID)
	if err != nil || len(decodedID) != 16 || intent.IntentID != strings.ToLower(intent.IntentID) {
		return fmt.Errorf("validate molecule lifecycle intent: intent_id %q is not canonical 128-bit hex", intent.IntentID)
	}
	if intent.FromStatus == "" || intent.FromStatus != strings.TrimSpace(intent.FromStatus) || intent.FromStatus == "closed" || intent.FromStatus == "tombstone" {
		return fmt.Errorf("validate molecule lifecycle intent: invalid from_status %q", intent.FromStatus)
	}
	if intent.Actor == "" || intent.Actor != strings.TrimSpace(intent.Actor) {
		return errors.New("validate molecule lifecycle intent: actor is blank or not normalized")
	}
	if intent.RequestedAt.IsZero() || intent.RequestedAt.Location() != time.UTC {
		return errors.New("validate molecule lifecycle intent: requested_at must be a non-zero UTC timestamp")
	}
	if intent.CloseReason != strings.TrimSpace(intent.CloseReason) || !isMoleculeLifecycleCloseReason(intent.CloseReason) {
		return fmt.Errorf("validate molecule lifecycle intent: unsupported close_reason %q", intent.CloseReason)
	}
	return nil
}

func isMoleculeLifecycleCloseReason(reason string) bool {
	return reason == moleculeAutocloseReason || reason == moleculeSourceAutocloseReason
}

// prepareMoleculeLifecycleIntent establishes durable event ownership before a
// capability-less close. The fixed marker is deliberately a separate final
// write: external metadata batches may apply partially or in map order, so a
// visible marker must imply that both the reason and full intent were attempted
// first. Any write error aborts the close; unmarked partial metadata is inert.
func prepareMoleculeLifecycleIntent(store beads.Store, id, reason, actor string, requestedAt time.Time) (beads.Bead, moleculeLifecycleIntent, error) {
	if store == nil {
		return beads.Bead{}, moleculeLifecycleIntent{}, errors.New("prepare molecule lifecycle intent: nil store")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return beads.Bead{}, moleculeLifecycleIntent{}, errors.New("prepare molecule lifecycle intent: empty bead id")
	}
	reason = strings.TrimSpace(reason)
	var (
		before beads.Bead
		intent moleculeLifecycleIntent
	)
	err := beads.WithLifecycleMetadataTransaction(store, id, func(tx beads.LifecycleMetadataTransaction) error {
		var prepareErr error
		before, intent, prepareErr = prepareMoleculeLifecycleIntentTransaction(tx, id, reason, actor, requestedAt)
		return prepareErr
	})
	return before, intent, err
}

func prepareMoleculeLifecycleIntentTransaction(
	tx beads.LifecycleMetadataTransaction,
	id, reason, actor string,
	requestedAt time.Time,
) (beads.Bead, moleculeLifecycleIntent, error) {
	before, err := tx.Get()
	if err != nil {
		return beads.Bead{}, moleculeLifecycleIntent{}, fmt.Errorf("prepare molecule lifecycle intent: live get %q: %w", id, err)
	}
	if before.ID != id {
		return before, moleculeLifecycleIntent{}, fmt.Errorf("prepare molecule lifecycle intent: live get %q returned bead %q", id, before.ID)
	}
	pendingMarker := strings.TrimSpace(before.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey])
	pending := pendingMarker != ""
	if before.Status == "closed" {
		if pending {
			return before, moleculeLifecycleIntent{}, errors.Join(errMoleculeLifecycleAlreadyClosed, errMoleculeLifecyclePending)
		}
		return before, moleculeLifecycleIntent{}, errMoleculeLifecycleAlreadyClosed
	}
	if pending {
		existing, decodeErr := decodeMoleculeLifecycleIntent(before.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
		if pendingMarker != moleculeLifecycleVersionV1 || decodeErr != nil ||
			existing.FromStatus != before.Status || existing.CloseReason != reason ||
			before.Metadata["close_reason"] != existing.CloseReason {
			return before, moleculeLifecycleIntent{}, errMoleculeLifecyclePending
		}
		return before, existing, nil
	}

	prepared, err := newMoleculeLifecycleIntent(before.Status, actor, reason, requestedAt)
	if err != nil {
		return before, moleculeLifecycleIntent{}, fmt.Errorf("prepare molecule lifecycle intent: %w", err)
	}
	raw, err := marshalMoleculeLifecycleIntent(prepared)
	if err != nil {
		return before, moleculeLifecycleIntent{}, fmt.Errorf("prepare molecule lifecycle intent: %w", err)
	}
	if err := tx.SetMetadataBatch(map[string]string{
		"close_reason": prepared.CloseReason,
		beadmeta.MoleculeLifecycleIntentMetadataKey: raw,
	}); err != nil {
		return before, moleculeLifecycleIntent{}, fmt.Errorf("prepare molecule lifecycle intent metadata for %q: %w", id, err)
	}
	if err := tx.SetMetadata(beadmeta.MoleculeLifecyclePendingMetadataKey, moleculeLifecycleVersionV1); err != nil {
		return before, moleculeLifecycleIntent{}, fmt.Errorf("mark molecule lifecycle intent pending for %q: %w", id, err)
	}
	return before, prepared, nil
}

type moleculeLifecycleReadDisposition uint8

const (
	moleculeLifecycleRetain moleculeLifecycleReadDisposition = iota
	moleculeLifecycleRetry
	moleculeLifecycleReady
)

func classifyCurrentPendingMoleculeLifecycle(fresh beads.Bead, id string) (moleculeLifecycleIntent, moleculeLifecycleReadDisposition) {
	if fresh.ID != id {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetry
	}
	if fresh.Status != "closed" {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetry
	}
	if fresh.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	intent, err := decodeMoleculeLifecycleIntent(fresh.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	if fresh.Metadata["close_reason"] != intent.CloseReason {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	return intent, moleculeLifecycleReady
}

func classifyPreparedOpenMoleculeLifecycle(fresh beads.Bead, id string) (moleculeLifecycleIntent, moleculeLifecycleReadDisposition) {
	if fresh.ID != id {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetry
	}
	if fresh.Status == "closed" || fresh.Status == "tombstone" {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	if fresh.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	intent, err := decodeMoleculeLifecycleIntent(fresh.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil || intent.FromStatus != fresh.Status {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	if fresh.Metadata["close_reason"] != intent.CloseReason {
		return moleculeLifecycleIntent{}, moleculeLifecycleRetain
	}
	return intent, moleculeLifecycleReady
}

func preparedOpenMoleculeLifecycleEligible(
	sourceTx, rootTx beads.LifecycleReadTransaction,
	fresh beads.Bead,
	intent moleculeLifecycleIntent,
	eligibility moleculeEligibilityContext,
) (bool, error) {
	if fresh.ID == "" || fresh.ID != strings.TrimSpace(fresh.ID) || fresh.Status != intent.FromStatus {
		return false, nil
	}
	switch intent.CloseReason {
	case moleculeAutocloseReason:
		if fresh.Type != "molecule" {
			return false, nil
		}
		terminal, descendants, err := liveSubtreeTerminalExcludingRoot(rootTx, fresh.ID)
		return err == nil && terminal && descendants > 0, err
	case moleculeSourceAutocloseReason:
		if !sourceworkflow.IsWorkflowRoot(fresh) {
			return false, nil
		}
		sourceID := sourceworkflow.NormalizeSourceBeadID(fresh.Metadata[beadmeta.SourceBeadIDMetadataKey])
		if sourceID == "" {
			return false, nil
		}
		rootSourceStoreRef := sourceworkflow.NormalizeSourceStoreRef(fresh.Metadata[beadmeta.SourceStoreRefMetadataKey])
		expectedSourceID := sourceworkflow.NormalizeSourceBeadID(eligibility.sourceBeadID)
		if expectedSourceID != "" && sourceID != expectedSourceID {
			return false, nil
		}
		expectedSourceStoreRef := sourceworkflow.NormalizeSourceStoreRef(eligibility.sourceStoreRef)
		rootStoreRef := sourceworkflow.NormalizeSourceStoreRef(eligibility.rootStoreRef)
		switch {
		case eligibility.requirePhysicalRefs:
			if expectedSourceStoreRef == "" || rootStoreRef == "" || rootSourceStoreRef == "" ||
				rootSourceStoreRef != expectedSourceStoreRef {
				return false, nil
			}
		case rootSourceStoreRef != "":
			if expectedSourceStoreRef == "" || rootSourceStoreRef != expectedSourceStoreRef {
				return false, nil
			}
		case expectedSourceStoreRef != "" &&
			(rootStoreRef == "" || rootStoreRef != expectedSourceStoreRef):
			return false, nil
		}
		if sourceTx == nil {
			return false, beads.ErrLifecycleReadUnsupported
		}
		source, err := sourceTx.GetByID(sourceID)
		if errors.Is(err, beads.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !convoycore.IsTerminalStatus(source.Status) {
			return false, nil
		}
		terminal, _, err := liveSubtreeTerminalExcludingRoot(rootTx, fresh.ID)
		return err == nil && terminal, err
	default:
		return false, nil
	}
}

// liveSubtreeTerminalExcludingRoot re-proves autoclose eligibility from live
// store reads while the caller owns the scope-wide lifecycle mutation lease.
// Each breadth-first edge lookup uses the singular, backend-indexed ParentID
// selector. Every returned row is still filtered defensively before it
// participates in the walk.
func liveSubtreeTerminalExcludingRoot(reader beads.LifecycleReadTransaction, rootID string) (bool, int, error) {
	if reader == nil || strings.TrimSpace(rootID) == "" {
		return false, 0, errors.New("live molecule subtree: missing transaction or root id")
	}
	logical, err := reader.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return false, 0, err
	}
	seen := map[string]struct{}{rootID: {}}
	queue := []string{rootID}
	members := make([]beads.Bead, 0, len(logical))
	add := func(candidate beads.Bead) {
		if candidate.ID == "" {
			return
		}
		if _, exists := seen[candidate.ID]; exists {
			return
		}
		seen[candidate.ID] = struct{}{}
		members = append(members, candidate)
		queue = append(queue, candidate.ID)
	}
	for _, candidate := range logical {
		if candidate.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID {
			add(candidate)
		}
	}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		children, listErr := reader.List(beads.ListQuery{
			ParentID:      parentID,
			IncludeClosed: true,
			TierMode:      beads.TierBoth,
		})
		if listErr != nil {
			return false, 0, listErr
		}
		for _, child := range children {
			if child.ParentID == parentID {
				add(child)
			}
		}
	}
	descendants := 0
	for _, member := range members {
		if sourceworkflow.IsGeneratedSpecSidecar(member) {
			continue
		}
		descendants++
		if !convoycore.IsTerminalStatus(member.Status) {
			return false, descendants, nil
		}
	}
	return true, descendants, nil
}

type moleculeLifecycleCompletionState struct {
	done  chan struct{}
	once  sync.Once
	retry bool
}

// moleculeLifecycleCompletion tracks publication callbacks that may be queued
// behind a store observer. Done is suitable for controller shutdown ownership;
// Wait returns whether a transient condition should be retried promptly.
type moleculeLifecycleCompletion struct {
	state *moleculeLifecycleCompletionState
}

func newMoleculeLifecycleCompletion() moleculeLifecycleCompletion {
	return moleculeLifecycleCompletion{state: &moleculeLifecycleCompletionState{done: make(chan struct{})}}
}

func completedMoleculeLifecycle(retry bool) moleculeLifecycleCompletion {
	completion := newMoleculeLifecycleCompletion()
	completion.finish(retry)
	return completion
}

func (c moleculeLifecycleCompletion) finish(retry bool) {
	if c.state == nil {
		return
	}
	c.state.once.Do(func() {
		c.state.retry = retry
		close(c.state.done)
	})
}

func (c moleculeLifecycleCompletion) Done() <-chan struct{} {
	if c.state == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return c.state.done
}

func (c moleculeLifecycleCompletion) Wait() bool {
	if c.state == nil {
		return false
	}
	<-c.state.done
	return c.state.retry
}

func moleculeLifecycleCompletionAfterDeliveries(deliveries []beads.CloseObserverDelivery, retry bool) moleculeLifecycleCompletion {
	completion := newMoleculeLifecycleCompletion()
	filtered := make([]beads.CloseObserverDelivery, 0, len(deliveries))
	for _, delivery := range deliveries {
		if delivery != nil {
			filtered = append(filtered, delivery)
		}
	}
	if len(filtered) == 0 {
		completion.finish(retry)
		return completion
	}

	var (
		mu        sync.Mutex
		remaining = len(filtered)
	)
	for _, delivery := range filtered {
		delivery.AfterDelivery(func() {
			mu.Lock()
			remaining--
			finished := remaining == 0
			mu.Unlock()
			if finished {
				completion.finish(retry)
			}
		})
	}
	return completion
}

type moleculeLifecyclePublishResult struct {
	retry      bool
	deliveries []beads.CloseObserverDelivery
}

// publishPendingMoleculeLifecycle publishes at the supplied close queue
// position, or reserves a non-mutating observer barrier when no close receipt
// is available. Bare stores without sequencing run immediately. The callback
// always re-reads the live row and verifies expectedIntentID before emission.
func publishPendingMoleculeLifecycle(store beads.Store, rec events.Recorder, id, expectedIntentID string, delivery beads.CloseObserverDelivery) moleculeLifecycleCompletion {
	completion := newMoleculeLifecycleCompletion()
	publish := func() {
		lock := moleculeLifecyclePublicationLock(id)
		lock.Lock()
		result := publishPendingMoleculeLifecycleNow(store, rec, id, expectedIntentID)
		lock.Unlock()
		// Marker/intent cleanup writes performed from inside an observer callback
		// enqueue their bead.updated notifications behind that callback. Reserve a
		// final queue position instead of reporting completion early or blocking
		// reentrantly inside the current callback.
		if barrier, ok := beads.ObserverBarrierFor(store); ok {
			if cleanupDelivery := barrier.BeadObserverBarrier(id); cleanupDelivery != nil {
				result.deliveries = append(result.deliveries, cleanupDelivery)
			}
		}
		drained := moleculeLifecycleCompletionAfterDeliveries(result.deliveries, result.retry)
		go func() { completion.finish(drained.Wait()) }()
	}
	if delivery == nil {
		if barrier, ok := beads.ObserverBarrierFor(store); ok {
			delivery = barrier.BeadObserverBarrier(id)
		}
	}
	if delivery == nil {
		publish()
		return completion
	}
	delivery.AfterDelivery(publish)
	return completion
}

func publishPendingMoleculeLifecycleNow(store beads.Store, rec events.Recorder, id, expectedIntentID string) moleculeLifecyclePublishResult {
	var (
		ownershipValidated bool
		markerCleared      bool
		cleanupRetry       bool
	)
	// The authoritative read, exact intent match, records, and cleanup share the
	// same lifecycle mutation domain as Reopen and status updates. Without this
	// fence, a delayed publisher can emit its stale closed snapshot after a peer
	// clears the intent and a writer has already emitted an authoritative reopen.
	cleanupErr := beads.WithLifecycleMetadataTransaction(store, id, func(tx beads.LifecycleMetadataTransaction) error {
		closed, readErr := tx.Get()
		if readErr != nil {
			cleanupRetry = true
			return readErr
		}
		currentIntent, currentDisposition := classifyCurrentPendingMoleculeLifecycle(closed, id)
		if currentDisposition != moleculeLifecycleReady {
			cleanupRetry = currentDisposition == moleculeLifecycleRetry
			return nil
		}
		if expectedIntentID == "" || currentIntent.IntentID != expectedIntentID {
			return nil
		}
		if rec == nil {
			cleanupRetry = true
			return nil
		}
		durableRecorder, ok := rec.(events.DurableRecorder)
		if !ok {
			cleanupRetry = true
			return nil
		}
		closedEvent, resolvedEvent, eventErr := moleculeLifecycleEvents(closed, currentIntent)
		if eventErr != nil {
			cleanupRetry = true
			return eventErr
		}

		// Keep the lifecycle lease through acknowledged, ordered publication so
		// the emitted closed snapshot linearizes before any later reopen. A
		// publication error retains both ownership records for at-least-once
		// recovery; the recorder may already have appended a partial batch.
		if recordErr := durableRecorder.RecordDurably(closedEvent, resolvedEvent); recordErr != nil {
			cleanupRetry = true
			return recordErr
		}
		ownershipValidated = true

		// Stamp the completed-publication marker before clearing the pending
		// marker so a crash between the two still records that this intent was
		// published. The periodic eligible-scan reads this to distinguish an
		// operator-reopened root (published once, now open again — leave it) from
		// a never-published outage-gap root (mint it). Every eligibility-gated
		// close, edge-triggered or recovered, publishes through here, so this is
		// the single point that records the fact of publication.
		if completedErr := tx.SetMetadata(beadmeta.MoleculeLifecycleCompletedMetadataKey, expectedIntentID); completedErr != nil {
			cleanupRetry = true
			return completedErr
		}

		if clearErr := tx.SetMetadata(beadmeta.MoleculeLifecyclePendingMetadataKey, ""); clearErr != nil {
			cleanupRetry = true
			return clearErr
		}
		markerCleared = true

		// The fixed marker is the recovery query. Once it is gone, an unmarked
		// intent is harmless. Keep the defensive live re-read inside the same
		// critical section before best-effort intent cleanup.
		afterMarkerClear, readErr := tx.Get()
		if readErr == nil && moleculeLifecycleIntentStillOwnedAfterMarkerClear(afterMarkerClear, id, expectedIntentID) {
			_ = tx.SetMetadata(beadmeta.MoleculeLifecycleIntentMetadataKey, "")
		}
		return nil
	})
	if cleanupErr != nil && !markerCleared {
		cleanupRetry = true
	}
	if !ownershipValidated {
		return moleculeLifecyclePublishResult{retry: cleanupRetry}
	}
	// Generated specs are topology sidecars, not executable work. Close them
	// only after the root's lifecycle records and durable publication cleanup so
	// their bead.closed notifications cannot overtake molecule.resolved. The
	// helper is idempotent and restart recovery reaches this same path.
	sidecars, sidecarErr := sourceworkflow.CloseSpecSidecarsForRootSequenced(store, id, sourceworkflow.WorkflowSpecSidecarClosedReason)
	return moleculeLifecyclePublishResult{
		retry:      cleanupRetry || sidecarErr != nil,
		deliveries: sidecars.Deliveries,
	}
}

func moleculeLifecycleIntentStillOwnedAfterMarkerClear(fresh beads.Bead, id, expectedIntentID string) bool {
	if fresh.ID != id || fresh.Status != "closed" {
		return false
	}
	if fresh.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" {
		return false
	}
	intent, err := decodeMoleculeLifecycleIntent(fresh.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey])
	if err != nil || intent.IntentID != expectedIntentID {
		return false
	}
	return fresh.Metadata["close_reason"] == intent.CloseReason
}

func moleculeLifecycleEvents(closed beads.Bead, intent moleculeLifecycleIntent) (events.Event, events.Event, error) {
	closedPayload, err := json.Marshal(closed)
	if err != nil {
		return events.Event{}, events.Event{}, fmt.Errorf("marshal molecule lifecycle closed snapshot: %w", err)
	}
	// These events are published during recovery, potentially hours after the
	// close actually committed. Stamp the envelope with the occurrence time so a
	// bare recorder does not back-fill time.Now() at publication (which would
	// place the close in the future relative to its own payload and skew
	// run-close accounting). Prefer the closed row's UpdatedAt (the close-commit
	// time) and fall back to the durable intent's RequestedAt — the same
	// occurrence-time preference the caching-store recovery path back-dates to.
	occurredAt := intent.RequestedAt.UTC()
	if !closed.UpdatedAt.IsZero() {
		occurredAt = closed.UpdatedAt.UTC()
	}
	closedEvent := events.Event{
		Type:    events.BeadClosed,
		Ts:      occurredAt,
		Actor:   intent.Actor,
		Subject: closed.ID,
		Payload: closedPayload,
	}
	resolvedEvent := events.Event{
		Type:    events.MoleculeResolved,
		Ts:      occurredAt,
		Actor:   intent.Actor,
		Subject: closed.ID,
		Payload: api.MoleculeResolvedPayloadJSON(api.MoleculeResolvedPayload{
			IssueID:     closed.ID,
			FromStatus:  intent.FromStatus,
			ToStatus:    "closed",
			Actor:       intent.Actor,
			SessionName: closed.Metadata[beadmeta.SessionNameMetadataKey],
			SessionID:   closed.Metadata[beadmeta.SessionIDMetadataKey],
			WorkDir:     closed.Metadata[beadmeta.WorkDirMetadataKey],
			CloseReason: intent.CloseReason,
			Ts:          intent.RequestedAt,
		}),
	}
	stampBeadSnapshotCorrelation(&closedEvent, closed)
	stampBeadSnapshotCorrelation(&resolvedEvent, closed)
	return closedEvent, resolvedEvent, nil
}

var moleculeLifecyclePublicationStripes [64]sync.Mutex

func moleculeLifecyclePublicationLock(id string) *sync.Mutex {
	var hash uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		hash ^= uint32(id[i])
		hash *= 16777619
	}
	return &moleculeLifecyclePublicationStripes[hash%uint32(len(moleculeLifecyclePublicationStripes))]
}

// recoverMoleculeLifecycleIntents synchronously drains durable v1 intents and
// then discovers unmarked open roots whose terminal graph may have been missed
// while the controller was stopped. Every candidate is revalidated from live
// store state under the lifecycle lease before it can close. Malformed or
// currently ineligible intents remain durable without a hot retry. A true
// result requests a bounded prompt retry for transient failures.
func recoverMoleculeLifecycleIntents(store beads.Store, rec events.Recorder) bool {
	retry := recoverPendingMoleculeLifecycles(store, rec).Wait()
	return recoverEligibleMoleculeLifecyclesWithResolver(store, "", nil, rec) || retry
}

type moleculeLifecycleStoreResolver func(storeRef string) (beads.Store, bool)

func recoverMoleculeLifecycleIntentsWithResolver(
	rootStore beads.Store,
	rootStoreRef string,
	resolve moleculeLifecycleStoreResolver,
	rec events.Recorder,
) bool {
	retry := recoverPendingMoleculeLifecyclesWithResolver(rootStore, rootStoreRef, resolve, rec).Wait()
	return recoverEligibleMoleculeLifecyclesWithResolver(rootStore, rootStoreRef, resolve, rec) || retry
}

// recoverEligibleMoleculeLifecyclesWithResolver repairs the event-watcher gap:
// watcher cursors start at the event-log head captured during controller
// construction, so a terminal child or source close that predates construction
// is never replayed. It re-lists molecule/workflow roots by type, live-reads
// each, and decides every eligible open root by its lifecycle history:
//
//   - PREPARED marker present: a durable intent is mid-flight — complete it
//     through the exact intent it carries (never a fresh, current-status-derived
//     close), so an intent whose from-status no longer matches the live row is
//     retained.
//   - COMPLETED marker present (and no prepared marker): the root was already
//     published once and is now open again — an operator reopened it. Skip it;
//     it re-closes only via the edge-triggered path, never this level-triggered
//     scan. This is what stops the reopen treadmill.
//   - No lifecycle history at all: an eligible root that was never durably
//     scheduled to close, i.e. it became terminal while the controller was down.
//     Mint the close here (the outage-gap heal the scan exists for). The mint
//     publishes through the prepared path, which stamps the completed marker, so
//     a later reopen falls into the skip case above.
func recoverEligibleMoleculeLifecyclesWithResolver(
	rootStore beads.Store,
	rootStoreRef string,
	resolve moleculeLifecycleStoreResolver,
	rec events.Recorder,
) bool {
	if rootStore == nil || rec == nil {
		return true
	}
	handles := beads.HandlesFor(rootStore)
	queries := []beads.ListQuery{
		{Type: "molecule", TierMode: beads.TierBoth},
		{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, TierMode: beads.TierBoth},
		{Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}, TierMode: beads.TierBoth},
	}
	var rows []beads.Bead
	retry := false
	for _, query := range queries {
		listed, listErr := handles.Live.List(query)
		rows = append(rows, listed...)
		if listErr != nil {
			retry = true
		}
	}
	var minted moleculeAutocloseCompletion
	publications := make([]moleculeLifecycleCompletion, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, listed := range rows {
		id := strings.TrimSpace(listed.ID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}

		fresh, readErr := handles.Live.Get(id)
		if readErr != nil || fresh.ID != id {
			retry = true
			continue
		}
		if convoycore.IsTerminalStatus(fresh.Status) {
			continue
		}

		if strings.TrimSpace(fresh.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]) != "" {
			publication, hasPublication, needRetry := completePreparedOpenMoleculeLifecycle(
				rootStore,
				rootStoreRef,
				resolve,
				rec,
				id,
				fresh,
			)
			if needRetry {
				retry = true
			}
			if hasPublication {
				publications = append(publications, publication)
			}
			continue
		}
		if strings.TrimSpace(fresh.Metadata[beadmeta.MoleculeLifecycleCompletedMetadataKey]) != "" {
			// Published before, reopened since — leave it to the edge-triggered path.
			continue
		}
		mintEligibleMoleculeLifecycle(rootStore, rootStoreRef, resolve, rec, id, fresh, &minted)
	}
	for _, publication := range publications {
		if publication.Wait() {
			retry = true
		}
	}
	return minted.Wait() || retry
}

// mintEligibleMoleculeLifecycle heals an outage gap by minting a fresh close for
// an eligible root that carries no lifecycle history. It is the pre-restriction
// discovery path, reached only for roots with neither a prepared nor a completed
// marker, so it can never re-close an operator-reopened root. The close is
// published through the prepared path, which stamps the completed marker.
func mintEligibleMoleculeLifecycle(
	rootStore beads.Store,
	rootStoreRef string,
	resolve moleculeLifecycleStoreResolver,
	rec events.Recorder,
	id string,
	fresh beads.Bead,
	minted *moleculeAutocloseCompletion,
) {
	if sourceworkflow.IsWorkflowRoot(fresh) &&
		sourceworkflow.NormalizeSourceBeadID(fresh.Metadata[beadmeta.SourceBeadIDMetadataKey]) != "" {
		eligibility, resolved := recoveryMoleculeEligibilityContext(
			rootStore,
			rootStoreRef,
			resolve,
			fresh,
			moleculeLifecycleIntent{CloseReason: moleculeSourceAutocloseReason},
		)
		if resolved {
			announcement := announceEligibleSourceClosedMoleculeResult(
				rootStore,
				eligibility.sourceStore,
				eligibility.sourceStoreRef,
				eligibility.rootStoreRef,
				eligibility.requirePhysicalRefs,
				eligibility.sourceBeadID,
				rec,
				fresh,
				io.Discard,
			)
			switch {
			case announcement.sidecarCleanupOwned:
				minted.add(announcement)
			case announcement.closed:
				minted.addFollowup(announcement, func() moleculeLifecycleCompletion {
					return closeSpecSidecarsForRootCompletion(rootStore, id)
				})
			default:
				minted.add(announcement)
			}
			if announcement.closed || announcement.lifecycleRetryNeeded || announcement.lifecycleRetry != nil {
				return
			}
		}
	}

	if fresh.Type == "molecule" {
		minted.add(announceEligibleClosedMoleculeResult(
			rootStore,
			rec,
			fresh,
			moleculeAutocloseReason,
			io.Discard,
		))
	}
}

// completePreparedOpenMoleculeLifecycle resumes the durable close intent already
// prepared on an open root and, on a clean authoritative close, returns the
// publication that emits its ordered lifecycle pair. It never mints a new intent
// or derives eligibility from the root's current status: an intent whose
// from-status no longer matches the live row (for example an operator-reopened
// root, or one an atomic writer already transitioned) classifies as retain and
// is left untouched. hasPublication reports whether a publication was returned;
// retry requests a bounded prompt retry after a transient failure.
func completePreparedOpenMoleculeLifecycle(
	rootStore beads.Store,
	rootStoreRef string,
	resolve moleculeLifecycleStoreResolver,
	rec events.Recorder,
	id string,
	fresh beads.Bead,
) (publication moleculeLifecycleCompletion, hasPublication bool, retry bool) {
	intent, disposition := classifyPreparedOpenMoleculeLifecycle(fresh, id)
	if disposition == moleculeLifecycleRetry {
		return moleculeLifecycleCompletion{}, false, true
	}
	if disposition != moleculeLifecycleReady {
		return moleculeLifecycleCompletion{}, false, false
	}
	eligibility, resolved := recoveryMoleculeEligibilityContext(rootStore, rootStoreRef, resolve, fresh, intent)
	if !resolved {
		return moleculeLifecycleCompletion{}, false, false
	}
	result, closeErr := closeMoleculeWithPreparedLifecycle(
		rootStore,
		id,
		intent.CloseReason,
		true,
		intent.IntentID,
		eligibility,
	)
	if errors.Is(closeErr, errMoleculeLifecycleIneligible) {
		return moleculeLifecycleCompletion{}, false, false
	}
	if closeErr != nil || !result.authoritativeAfter || result.lifecycleIntentID != intent.IntentID {
		return moleculeLifecycleCompletion{}, false, true
	}
	return publishPendingMoleculeLifecycle(rootStore, rec, id, intent.IntentID, result.lifecycleDelivery), true, false
}

func recoverPendingMoleculeLifecycles(store beads.Store, rec events.Recorder) moleculeLifecycleCompletion {
	return recoverPendingMoleculeLifecyclesWithResolver(store, "", nil, rec)
}

func recoverPendingMoleculeLifecyclesWithResolver(
	rootStore beads.Store,
	rootStoreRef string,
	resolve moleculeLifecycleStoreResolver,
	rec events.Recorder,
) moleculeLifecycleCompletion {
	if rootStore == nil {
		return completedMoleculeLifecycle(true)
	}
	handles := beads.HandlesFor(rootStore)
	rows, listErr := handles.Live.List(beads.ListQuery{
		Metadata: map[string]string{
			beadmeta.MoleculeLifecyclePendingMetadataKey: moleculeLifecycleVersionV1,
		},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	retry := listErr != nil
	publications := make([]moleculeLifecycleCompletion, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, listed := range rows {
		if listed.ID == "" {
			continue
		}
		if _, duplicate := seen[listed.ID]; duplicate {
			continue
		}
		seen[listed.ID] = struct{}{}
		// The list row is discovery only. Every candidate is live-read even when
		// the listing came from a stale projection, and no event is built from
		// list/cache data.
		fresh, readErr := handles.Live.Get(listed.ID)
		if readErr != nil || fresh.ID != listed.ID {
			retry = true
			continue
		}
		if fresh.Status != "closed" {
			publication, hasPublication, needRetry := completePreparedOpenMoleculeLifecycle(
				rootStore,
				rootStoreRef,
				resolve,
				rec,
				listed.ID,
				fresh,
			)
			if needRetry {
				retry = true
			}
			if hasPublication {
				publications = append(publications, publication)
			}
			continue
		}

		intent, disposition := classifyCurrentPendingMoleculeLifecycle(fresh, listed.ID)
		if disposition == moleculeLifecycleRetry {
			retry = true
			continue
		}
		if disposition != moleculeLifecycleReady {
			continue
		}
		publications = append(publications, publishPendingMoleculeLifecycle(rootStore, rec, listed.ID, intent.IntentID, nil))
	}
	aggregate := newMoleculeLifecycleCompletion()
	go func() {
		needsRetry := retry
		for _, publication := range publications {
			if publication.Wait() {
				needsRetry = true
			}
		}
		sidecars, sidecarErr := sourceworkflow.CloseSpecSidecarsForClosedRootsSequenced(
			rootStore,
			sourceworkflow.WorkflowSpecSidecarClosedReason,
		)
		if moleculeLifecycleCompletionAfterDeliveries(sidecars.Deliveries, sidecarErr != nil).Wait() {
			needsRetry = true
		}
		aggregate.finish(needsRetry)
	}()
	return aggregate
}

func recoveryMoleculeEligibilityContext(
	rootStore beads.Store,
	rootStoreRef string,
	resolve moleculeLifecycleStoreResolver,
	fresh beads.Bead,
	intent moleculeLifecycleIntent,
) (moleculeEligibilityContext, bool) {
	if intent.CloseReason != moleculeSourceAutocloseReason {
		return moleculeEligibilityContext{}, true
	}
	sourceID := sourceworkflow.NormalizeSourceBeadID(fresh.Metadata[beadmeta.SourceBeadIDMetadataKey])
	if sourceID == "" {
		return moleculeEligibilityContext{}, false
	}
	sourceStoreRef := sourceworkflow.NormalizeSourceStoreRef(fresh.Metadata[beadmeta.SourceStoreRefMetadataKey])
	if resolve == nil {
		// Compatibility recovery can safely re-prove legacy same-store roots, but
		// a persisted physical source ref must be resolved by the controller. Do
		// not substitute a same-ID row from the root store.
		return moleculeEligibilityContext{}, sourceStoreRef == ""
	}

	resolvedRootStoreRef := sourceworkflow.NormalizeSourceStoreRef(fresh.Metadata[beadmeta.RootStoreRefMetadataKey])
	if resolvedRootStoreRef == "" {
		resolvedRootStoreRef = sourceworkflow.NormalizeSourceStoreRef(rootStoreRef)
	}
	if resolvedRootStoreRef == "" {
		return moleculeEligibilityContext{}, false
	}
	resolvedRootStore, ok := resolve(resolvedRootStoreRef)
	if !ok || !sameMoleculeLifecycleStore(resolvedRootStore, rootStore) {
		return moleculeEligibilityContext{}, false
	}

	if sourceStoreRef == "" {
		return moleculeEligibilityContext{
			sourceStore:    rootStore,
			sourceStoreRef: resolvedRootStoreRef,
			rootStoreRef:   resolvedRootStoreRef,
			sourceBeadID:   sourceID,
		}, true
	}
	sourceStore, ok := resolve(sourceStoreRef)
	if !ok || sourceStore == nil {
		return moleculeEligibilityContext{}, false
	}
	return moleculeEligibilityContext{
		sourceStore:         sourceStore,
		sourceStoreRef:      sourceStoreRef,
		rootStoreRef:        resolvedRootStoreRef,
		sourceBeadID:        sourceID,
		requirePhysicalRefs: !sameMoleculeLifecycleStore(sourceStore, rootStore),
	}, true
}
