package api

import (
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type workflowEventProjection struct {
	Type            string                  `json:"type"`
	WorkflowID      string                  `json:"workflow_id"`
	RootBeadID      string                  `json:"root_bead_id"`
	RootStoreRef    string                  `json:"root_store_ref"`
	ScopeKind       string                  `json:"scope_kind"`
	ScopeRef        string                  `json:"scope_ref"`
	WatchGeneration string                  `json:"watch_generation"`
	EventSeq        uint64                  `json:"event_seq"`
	WorkflowSeq     uint64                  `json:"workflow_seq"`
	EventTS         string                  `json:"event_ts"`
	EventType       string                  `json:"event_type"`
	Bead            workflowBeadResponse    `json:"bead"`
	ChangedFields   []string                `json:"changed_fields"`
	LogicalNodeID   string                  `json:"logical_node_id"`
	AttemptSummary  *WorkflowAttemptSummary `json:"attempt_summary,omitempty"`
	RequiresResync  bool                    `json:"requires_resync,omitempty"`
}

// WorkflowAttemptSummary describes retry accounting for a workflow bead.
// Emitted on workflow projections whenever a bead has a non-zero attempt
// count. MaxAttempts is omitted when no ceiling is configured.
type WorkflowAttemptSummary struct {
	AttemptCount  int `json:"attempt_count"`
	ActiveAttempt int `json:"active_attempt"`
	MaxAttempts   int `json:"max_attempts,omitempty"`
}

// WireEvent is the list-endpoint wire shape for a single event,
// emitted by GET /v0/city/{cityName}/events. Same envelope fields as
// eventStreamEnvelope minus the SSE-specific Workflow projection.
// Payload is decoded via the events registry into a typed variant when
// possible. Custom event types pass through with their raw JSON payload.
type WireEvent struct {
	Seq     uint64            `json:"seq"`
	Type    string            `json:"type"`
	Ts      time.Time         `json:"ts"`
	Actor   string            `json:"actor"`
	Subject string            `json:"subject,omitempty"`
	Message string            `json:"message,omitempty"`
	Payload EventPayloadUnion `json:"payload,omitempty"`
}

// Schema makes list endpoints use the same envelope-discriminated schema as
// the city event stream. Runtime JSON stays the struct shape above; the
// OpenAPI contract tells clients to select payload type from envelope.type.
func (WireEvent) Schema(r huma.Registry) *huma.Schema {
	return typedEventStreamEnvelopeSchema{}.Schema(r)
}

// WireTaggedEvent is the supervisor-scope list wire shape for
// GET /v0/events, carrying the City the event originated from.
type WireTaggedEvent struct {
	WireEvent
	City string `json:"city"`
}

// Schema makes supervisor event lists use the same envelope-discriminated
// schema as the supervisor event stream.
func (WireTaggedEvent) Schema(r huma.Registry) *huma.Schema {
	return typedTaggedEventStreamEnvelopeSchema{}.Schema(r)
}

// toWireEvent decodes the bus's opaque Payload into the registered typed
// variant when one exists. Custom event types are still part of the public
// event contract because `gc event emit` accepts them, so they pass through
// under the schema's custom-event branch.
func toWireEvent(e events.Event) (WireEvent, bool) {
	decoded, registered, err := events.DecodePayload(e.Type, e.Payload)
	if err != nil {
		log.Printf("api: events wire: decode payload for %q seq=%d: %v", e.Type, e.Seq, err)
		return WireEvent{}, false
	}
	payload, err := customEventPayload(e.Payload)
	if err != nil {
		log.Printf("api: events wire: decode custom payload for %q seq=%d: %v", e.Type, e.Seq, err)
		return WireEvent{}, false
	}
	if registered {
		payload = decoded
	}
	return WireEvent{
		Seq:     e.Seq,
		Type:    e.Type,
		Ts:      e.Ts,
		Actor:   e.Actor,
		Subject: e.Subject,
		Message: e.Message,
		Payload: EventPayloadUnion{Value: payload},
	}, true
}

// toWireTaggedEvent is the supervisor-scope analog of toWireEvent,
// preserving the City tag the multiplexer attached to the event.
// Same skip-not-degrade contract for corrupt registered payloads; custom
// event types pass through.
func toWireTaggedEvent(te events.TaggedEvent) (WireTaggedEvent, bool) {
	wire, ok := toWireEvent(te.Event)
	if !ok {
		return WireTaggedEvent{}, false
	}
	return WireTaggedEvent{WireEvent: wire, City: taggedEventWireCity(te)}, true
}

// eventStreamEnvelope is the wire shape emitted on
// /v0/city/{cityName}/events/stream. The envelope is a single named
// schema so generated Go and TS clients have a concrete type to work
// with; the Payload field is the discriminated union, schema-typed as
// oneOf over every registered events.Payload variant. Consumers read
// `type` to know which variant `payload` holds.
type eventStreamEnvelope struct {
	Seq      uint64                   `json:"seq"`
	Type     string                   `json:"type"`
	Ts       time.Time                `json:"ts"`
	Actor    string                   `json:"actor"`
	Subject  string                   `json:"subject,omitempty"`
	Message  string                   `json:"message,omitempty"`
	Payload  EventPayloadUnion        `json:"payload,omitempty"`
	Workflow *workflowEventProjection `json:"workflow,omitempty"`
}

// taggedEventStreamEnvelope is the supervisor-scope wire shape for
// /v0/events/stream. Structurally identical to eventStreamEnvelope
// plus a City field identifying which city emitted the event.
type taggedEventStreamEnvelope struct {
	Seq      uint64                   `json:"seq"`
	Type     string                   `json:"type"`
	Ts       time.Time                `json:"ts"`
	Actor    string                   `json:"actor"`
	Subject  string                   `json:"subject,omitempty"`
	Message  string                   `json:"message,omitempty"`
	Payload  EventPayloadUnion        `json:"payload,omitempty"`
	City     string                   `json:"city"`
	Workflow *workflowEventProjection `json:"workflow,omitempty"`
}

// EventPayloadUnion wraps any registered events.Payload or custom raw JSON
// payload for wire emission. Known event types keep their registered payload
// shape; custom event types preserve what was recorded.
type EventPayloadUnion struct {
	Value any
}

// MarshalJSON emits the concrete payload's JSON directly so the wire
// sees {"rig":...} (for mail) rather than {"Value": {...}}.
func (p EventPayloadUnion) MarshalJSON() ([]byte, error) {
	if p.Value == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(p.Value)
}

// Schema registers an "EventPayload" named component whose schema is
// a oneOf of every registered payload type, then returns a $ref.
// Named registration keeps the generated clients compact — one
// EventPayload union type — rather than inlining the oneOf in every
// envelope field reference.
func (EventPayloadUnion) Schema(r huma.Registry) *huma.Schema {
	const name = "EventPayload"
	if _, ok := r.Map()[name]; !ok {
		payloads := events.RegisteredPayloadTypes()
		// Deduplicate by Go type — several event-type constants share
		// the same payload shape (e.g. all mail.* events use
		// MailEventPayload).
		seen := map[reflect.Type]bool{}
		types := make([]reflect.Type, 0)
		for _, sample := range payloads {
			t := reflect.TypeOf(sample)
			if seen[t] {
				continue
			}
			seen[t] = true
			types = append(types, t)
		}
		// Sort by type name for a stable spec.
		sort.Slice(types, func(i, j int) bool {
			return types[i].Name() < types[j].Name()
		})
		oneOf := make([]*huma.Schema, 0, len(types))
		for _, t := range types {
			oneOf = append(oneOf, r.Schema(t, true, t.Name()))
		}
		r.Map()[name] = &huma.Schema{OneOf: oneOf}
	}
	return &huma.Schema{Ref: schemaRefPrefix + name}
}

// wireEventFrom decodes the bus's opaque Payload into the registered typed
// variant when one exists and otherwise emits a custom-event envelope.
func wireEventFrom(e events.Event, workflow *workflowEventProjection) (eventStreamEnvelope, error) {
	decoded, registered, err := events.DecodePayload(e.Type, e.Payload)
	if err != nil {
		return eventStreamEnvelope{}, fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	payload, err := customEventPayload(e.Payload)
	if err != nil {
		return eventStreamEnvelope{}, fmt.Errorf("decode custom %s payload: %w", e.Type, err)
	}
	if registered {
		payload = decoded
	}
	return eventStreamEnvelope{
		Seq:      e.Seq,
		Type:     e.Type,
		Ts:       e.Ts,
		Actor:    e.Actor,
		Subject:  e.Subject,
		Message:  e.Message,
		Payload:  EventPayloadUnion{Value: payload},
		Workflow: workflow,
	}, nil
}

// wireTaggedEventFrom is the supervisor-scope analog of wireEventFrom.
func wireTaggedEventFrom(te events.TaggedEvent, workflow *workflowEventProjection) (taggedEventStreamEnvelope, error) {
	decoded, registered, err := events.DecodePayload(te.Type, te.Payload)
	if err != nil {
		return taggedEventStreamEnvelope{}, fmt.Errorf("decode %s payload: %w", te.Type, err)
	}
	payload, err := customEventPayload(te.Payload)
	if err != nil {
		return taggedEventStreamEnvelope{}, fmt.Errorf("decode custom %s payload: %w", te.Type, err)
	}
	if registered {
		payload = decoded
	}
	return taggedEventStreamEnvelope{
		Seq:      te.Seq,
		Type:     te.Type,
		Ts:       te.Ts,
		Actor:    te.Actor,
		Subject:  te.Subject,
		Message:  te.Message,
		Payload:  EventPayloadUnion{Value: payload},
		City:     taggedEventWireCity(te),
		Workflow: workflow,
	}, nil
}

func taggedEventWireCity(te events.TaggedEvent) string {
	if te.City != "__supervisor__" {
		return te.City
	}
	if te.Subject != "" && isCityRequestResultType(te.Type) {
		return te.Subject
	}
	switch te.Type {
	case events.RequestResultCityCreate:
		var payload CityCreateSucceededPayload
		if json.Unmarshal(te.Payload, &payload) == nil && payload.Name != "" {
			return payload.Name
		}
	case events.RequestResultCityUnregister:
		var payload CityUnregisterSucceededPayload
		if json.Unmarshal(te.Payload, &payload) == nil && payload.Name != "" {
			return payload.Name
		}
	}
	return te.City
}

func isCityRequestResultType(eventType string) bool {
	switch eventType {
	case events.RequestResultCityCreate, events.RequestResultCityUnregister, events.RequestFailed:
		return true
	default:
		return false
	}
}

func customEventPayload(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON")
	}
	return raw, nil
}

func projectWorkflowEvent(state State, event events.Event) *workflowEventProjection {
	if !isWorkflowEventType(event.Type) {
		return nil
	}

	bead, ok := workflowEventBead(state, event)
	if !ok {
		return nil
	}

	info, root, ok := workflowEventRoot(state, bead)
	if !ok {
		return nil
	}

	scopeKind, scopeRef := workflowEventScope(info, root, workflowCityScopeRef(state.CityName()))
	if scopeKind == "" || scopeRef == "" {
		return nil
	}

	workflowID := resolvedWorkflowID(root)
	if workflowID == "" {
		workflowID = strings.TrimSpace(bead.Metadata["gc.workflow_id"])
	}
	if workflowID == "" {
		workflowID = root.ID
	}

	logicalNodeID := strings.TrimSpace(bead.Metadata["gc.logical_bead_id"])
	if logicalNodeID == "" {
		logicalNodeID = bead.ID
	}
	if logicalNodeID == "" {
		return nil
	}

	changedFields := workflowChangedFields(event.Type)

	projection := &workflowEventProjection{
		Type:         "workflow:event",
		WorkflowID:   workflowID,
		RootBeadID:   root.ID,
		RootStoreRef: info.ref,
		ScopeKind:    scopeKind,
		ScopeRef:     scopeRef,
		// GC only knows the pre-broker projection. The dashboard overwrites this
		// with the active relay generation before fan-out to workflow watchers.
		WatchGeneration: "pending",
		EventSeq:        event.Seq,
		WorkflowSeq:     event.Seq,
		EventTS:         event.Ts.UTC().Format(time.RFC3339),
		EventType:       event.Type,
		Bead: workflowBeadResponse{
			ID:            bead.ID,
			Title:         bead.Title,
			Status:        workflowStatus(bead),
			Kind:          workflowKind(bead),
			StepRef:       strings.TrimSpace(bead.Metadata["gc.step_ref"]),
			Attempt:       workflowAttempt(bead),
			LogicalBeadID: strings.TrimSpace(bead.Metadata["gc.logical_bead_id"]),
			ScopeRef:      strings.TrimSpace(bead.Metadata["gc.scope_ref"]),
			Assignee:      strings.TrimSpace(bead.Assignee),
			Metadata:      cloneStringMap(bead.Metadata),
		},
		ChangedFields: changedFields,
		LogicalNodeID: logicalNodeID,
	}
	if event.Type == events.BeadUpdated {
		projection.RequiresResync = true
	}

	if summary := workflowAttemptSummary(bead); summary != nil {
		projection.AttemptSummary = summary
	}

	return projection
}

func isWorkflowEventType(eventType string) bool {
	return eventType == events.BeadCreated ||
		eventType == events.BeadUpdated ||
		eventType == events.BeadClosed
}

func workflowEventBead(state State, event events.Event) (beads.Bead, bool) {
	if bead, ok := workflowEventBeadFromPayload(event.Payload); ok {
		return bead, true
	}
	return workflowEventBeadFromSubject(state, event.Subject)
}

func workflowEventBeadFromPayload(payload json.RawMessage) (beads.Bead, bool) {
	if len(payload) == 0 {
		return beads.Bead{}, false
	}
	var bead beads.Bead
	if err := json.Unmarshal(payload, &bead); err != nil {
		return beads.Bead{}, false
	}
	if strings.TrimSpace(bead.ID) == "" {
		return beads.Bead{}, false
	}
	if !workflowEventPayloadLooksWorkflow(bead) {
		return beads.Bead{}, false
	}
	return bead, true
}

func workflowEventPayloadLooksWorkflow(bead beads.Bead) bool {
	if workflowKind(bead) == "workflow" {
		return true
	}
	return strings.TrimSpace(bead.Metadata["gc.root_bead_id"]) != "" ||
		strings.TrimSpace(bead.Metadata["gc.workflow_id"]) != "" ||
		strings.TrimSpace(bead.Metadata["gc.root_store_ref"]) != ""
}

func workflowEventBeadFromSubject(state State, subjectID string) (beads.Bead, bool) {
	subjectID = strings.TrimSpace(subjectID)
	if subjectID == "" {
		return beads.Bead{}, false
	}

	matches := make([]beads.Bead, 0, 2)
	for _, info := range workflowStores(state) {
		if info.store == nil {
			continue
		}
		bead, err := info.store.Get(subjectID)
		if err == nil {
			matches = append(matches, bead)
		}
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return beads.Bead{}, false
}

func workflowEventRoot(state State, bead beads.Bead) (workflowStoreInfo, beads.Bead, bool) {
	rootID := strings.TrimSpace(bead.Metadata["gc.root_bead_id"])
	if rootID == "" && workflowKind(bead) == "workflow" {
		rootID = bead.ID
	}
	if rootID == "" {
		return workflowStoreInfo{}, beads.Bead{}, false
	}

	if info, ok := workflowStoreByRef(state, bead.Metadata["gc.root_store_ref"]); ok && info.store != nil {
		root, ok := workflowRootInStore(info.store, rootID)
		if ok {
			return info, root, true
		}
	}

	matches := make([]workflowRootMatch, 0, 2)
	for _, info := range workflowStores(state) {
		if info.store == nil {
			continue
		}
		root, ok := workflowRootInStore(info.store, rootID)
		if ok {
			matches = append(matches, workflowRootMatch{info: info, root: root})
		}
	}
	if len(matches) == 1 {
		return matches[0].info, matches[0].root, true
	}
	return workflowStoreInfo{}, beads.Bead{}, false
}

func workflowRootInStore(store beads.Store, rootID string) (beads.Bead, bool) {
	root, err := store.Get(rootID)
	if err != nil || !isWorkflowRoot(root) {
		return beads.Bead{}, false
	}
	return root, true
}

func workflowChangedFields(eventType string) []string {
	switch eventType {
	case events.BeadCreated:
		return []string{"status", "metadata"}
	case events.BeadClosed:
		return []string{"status"}
	default:
		return []string{"snapshot"}
	}
}

func workflowAttemptSummary(bead beads.Bead) *WorkflowAttemptSummary {
	attempt := workflowAttemptValue(bead)
	if attempt <= 0 {
		return nil
	}
	summary := &WorkflowAttemptSummary{
		AttemptCount:  attempt,
		ActiveAttempt: attempt,
	}
	if maxAttempts := metadataInt(bead.Metadata, "gc.max_attempts"); maxAttempts > 0 {
		summary.MaxAttempts = maxAttempts
	}
	return summary
}
