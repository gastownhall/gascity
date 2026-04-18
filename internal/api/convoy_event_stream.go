package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type workflowEventProjection struct {
	Type            string                   `json:"type"`
	WorkflowID      string                   `json:"workflow_id"`
	RootBeadID      string                   `json:"root_bead_id"`
	RootStoreRef    string                   `json:"root_store_ref"`
	ScopeKind       string                   `json:"scope_kind"`
	ScopeRef        string                   `json:"scope_ref"`
	WatchGeneration string                   `json:"watch_generation"`
	EventSeq        uint64                   `json:"event_seq"`
	WorkflowSeq     uint64                   `json:"workflow_seq"`
	EventTS         string                   `json:"event_ts"`
	EventType       string                   `json:"event_type"`
	Bead            workflowBeadResponse     `json:"bead"`
	ChangedFields   []string                 `json:"changed_fields"`
	LogicalNodeID   string                   `json:"logical_node_id"`
	AttemptSummary  *WorkflowAttemptSummary  `json:"attempt_summary,omitempty"`
	RequiresResync  bool                     `json:"requires_resync,omitempty"`
}

// WorkflowAttemptSummary describes retry accounting for a workflow bead.
// Emitted on workflow projections whenever a bead has a non-zero attempt
// count. MaxAttempts is omitted when no ceiling is configured.
type WorkflowAttemptSummary struct {
	AttemptCount  int `json:"attempt_count"`
	ActiveAttempt int `json:"active_attempt"`
	MaxAttempts   int `json:"max_attempts,omitempty"`
}

// eventStreamEnvelope is the wire shape emitted on
// /v0/city/{cityName}/events/stream. Unlike events.Event (the bus's
// internal storage shape, which keeps Payload as opaque bytes), the
// Payload field here is a typed variant decoded via the events
// registry so consumers receive a discriminated-union wire schema
// they can switch on `type` against.
type eventStreamEnvelope struct {
	Seq      uint64                   `json:"seq"`
	Type     string                   `json:"type"`
	Ts       time.Time                `json:"ts"`
	Actor    string                   `json:"actor"`
	Subject  string                   `json:"subject,omitempty"`
	Message  string                   `json:"message,omitempty"`
	Payload  events.Payload           `json:"payload,omitempty"`
	Workflow *workflowEventProjection `json:"workflow,omitempty"`
}

// Schema emits a oneOf over every registered event-type variant so
// the OpenAPI spec describes the discriminated union consumers receive
// on /v0/city/{cityName}/events/stream. Each variant has a `type`
// const matching one of events.KnownEventTypes and a `payload` $ref
// to the corresponding payload schema (Principle 7).
func (eventStreamEnvelope) Schema(r huma.Registry) *huma.Schema {
	return eventVariantsSchema(r, false)
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
	Payload  events.Payload           `json:"payload,omitempty"`
	City     string                   `json:"city"`
	Workflow *workflowEventProjection `json:"workflow,omitempty"`
}

// Schema emits the same oneOf discrimination as eventStreamEnvelope,
// with an added City property on every variant.
func (taggedEventStreamEnvelope) Schema(r huma.Registry) *huma.Schema {
	return eventVariantsSchema(r, true)
}

// eventVariantsSchema builds a oneOf of per-event-type schema variants.
// Each variant has a `type: const <eventType>` property and a
// `payload: $ref <payloadSchema>` property, plus the envelope fields.
// tagged=true adds the City property required on supervisor-scope
// events. Variants are emitted in sorted order so the spec is stable
// across regenerations.
func eventVariantsSchema(r huma.Registry, tagged bool) *huma.Schema {
	payloads := events.RegisteredPayloadTypes()
	names := make([]string, 0, len(payloads))
	for name := range payloads {
		names = append(names, name)
	}
	sort.Strings(names)

	variants := make([]*huma.Schema, 0, len(names))
	for _, eventType := range names {
		sample := payloads[eventType]
		payloadType := reflect.TypeOf(sample)
		payloadSchema := r.Schema(payloadType, true, payloadType.Name())

		props := map[string]*huma.Schema{
			"seq":     {Type: huma.TypeInteger, Format: "int64"},
			"type":    {Type: huma.TypeString, Enum: []any{eventType}},
			"ts":      {Type: huma.TypeString, Format: "date-time"},
			"actor":   {Type: huma.TypeString},
			"subject": {Type: huma.TypeString},
			"message": {Type: huma.TypeString},
			"payload": payloadSchema,
			"workflow": r.Schema(reflect.TypeOf(workflowEventProjection{}), true, "WorkflowEventProjection"),
		}
		required := []string{"seq", "type", "ts", "actor", "payload"}
		if tagged {
			props["city"] = &huma.Schema{Type: huma.TypeString}
			required = append(required, "city")
		}
		variants = append(variants, &huma.Schema{
			Type:                 huma.TypeObject,
			Properties:           props,
			Required:             required,
			AdditionalProperties: false,
		})
	}
	return &huma.Schema{OneOf: variants}
}

// wireEventFrom decodes the bus's opaque Payload into the registered
// typed variant and returns a wire envelope ready for SSE emission on
// the per-city stream. Unregistered event types cause an error —
// Principle 7's strict policy enforced at emission time
// (the registry-coverage test catches this at CI).
func wireEventFrom(e events.Event, workflow *workflowEventProjection) (eventStreamEnvelope, error) {
	decoded, registered, err := events.DecodePayload(e.Type, e.Payload)
	if err != nil {
		return eventStreamEnvelope{}, fmt.Errorf("decode %s payload: %w", e.Type, err)
	}
	if !registered {
		return eventStreamEnvelope{}, fmt.Errorf("event type %q has no registered payload (see internal/api/event_payloads.go)", e.Type)
	}
	payload, _ := decoded.(events.Payload)
	return eventStreamEnvelope{
		Seq:      e.Seq,
		Type:     e.Type,
		Ts:       e.Ts,
		Actor:    e.Actor,
		Subject:  e.Subject,
		Message:  e.Message,
		Payload:  payload,
		Workflow: workflow,
	}, nil
}

// wireTaggedEventFrom is the supervisor-scope analog of wireEventFrom.
func wireTaggedEventFrom(te events.TaggedEvent, workflow *workflowEventProjection) (taggedEventStreamEnvelope, error) {
	decoded, registered, err := events.DecodePayload(te.Type, te.Payload)
	if err != nil {
		return taggedEventStreamEnvelope{}, fmt.Errorf("decode %s payload: %w", te.Type, err)
	}
	if !registered {
		return taggedEventStreamEnvelope{}, fmt.Errorf("event type %q has no registered payload (see internal/api/event_payloads.go)", te.Type)
	}
	payload, _ := decoded.(events.Payload)
	return taggedEventStreamEnvelope{
		Seq:      te.Seq,
		Type:     te.Type,
		Ts:       te.Ts,
		Actor:    te.Actor,
		Subject:  te.Subject,
		Message:  te.Message,
		Payload:  payload,
		City:     te.City,
		Workflow: workflow,
	}, nil
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
