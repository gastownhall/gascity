package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type workflowEventProjection struct {
	Type            string               `json:"type"`
	WorkflowID      string               `json:"workflow_id"`
	RootBeadID      string               `json:"root_bead_id"`
	RootStoreRef    string               `json:"root_store_ref"`
	ScopeKind       string               `json:"scope_kind"`
	ScopeRef        string               `json:"scope_ref"`
	WatchGeneration string               `json:"watch_generation"`
	EventSeq        uint64               `json:"event_seq"`
	WorkflowSeq     uint64               `json:"workflow_seq"`
	EventTS         string               `json:"event_ts"`
	EventType       string               `json:"event_type"`
	Bead            workflowBeadResponse `json:"bead"`
	ChangedFields   []string             `json:"changed_fields"`
	LogicalNodeID   string               `json:"logical_node_id"`
	AttemptSummary  map[string]any       `json:"attempt_summary,omitempty"`
	RequiresResync  bool                 `json:"requires_resync,omitempty"`
}

type eventStreamEnvelope struct {
	events.Event
	Workflow *workflowEventProjection `json:"workflow,omitempty"`
}

type taggedEventStreamEnvelope struct {
	events.TaggedEvent
	Workflow *workflowEventProjection `json:"workflow,omitempty"`
}

func streamProjectedEventsWithWatcher(
	ctx context.Context,
	w http.ResponseWriter,
	watcher events.Watcher,
	state State,
) {
	defer watcher.Close() //nolint:errcheck

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	type result struct {
		event events.Event
		err   error
	}
	ch := make(chan result, 1)

	readNext := func() {
		go func() {
			e, err := watcher.Next()
			select {
			case ch <- result{event: e, err: err}:
			case <-ctx.Done():
			}
		}()
	}

	readNext()

	for {
		select {
		case <-ctx.Done():
			return
		case r := <-ch:
			if r.err != nil {
				return
			}
			data, err := json.Marshal(eventStreamEnvelope{
				Event:    r.event,
				Workflow: projectWorkflowEvent(state, r.event),
			})
			if err == nil {
				writeSSE(w, r.event.Type, r.event.Seq, data)
			}
			readNext()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
}

func streamProjectedGlobalEvents(
	ctx context.Context,
	w http.ResponseWriter,
	mw *events.MuxWatcher,
	cursors map[string]uint64,
	resolver CityResolver,
) {
	defer mw.Close() //nolint:errcheck

	if cursors == nil {
		cursors = make(map[string]uint64)
	}

	keepalive := time.NewTicker(sseKeepalive)
	defer keepalive.Stop()

	type result struct {
		event events.TaggedEvent
		err   error
	}
	ch := make(chan result, 1)

	readNext := func() {
		go func() {
			te, err := mw.Next()
			select {
			case ch <- result{event: te, err: err}:
			case <-ctx.Done():
			}
		}()
	}

	readNext()

	for {
		select {
		case <-ctx.Done():
			return
		case r := <-ch:
			if r.err != nil {
				return
			}
			cursors[r.event.City] = r.event.Seq
			var wfp *workflowEventProjection
			if cs := resolver.CityState(r.event.City); cs != nil {
				wfp = projectWorkflowEvent(cs, r.event.Event)
			}
			data, err := json.Marshal(taggedEventStreamEnvelope{
				TaggedEvent: r.event,
				Workflow:    wfp,
			})
			if err == nil {
				cursorID := events.FormatCursor(cursors)
				writeSSEWithStringID(w, r.event.Type, cursorID, data)
			}
			readNext()
		case <-keepalive.C:
			writeSSEComment(w)
		}
	}
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
		// GC only knows the pre-broker projection. Mission Control overwrites this
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

	if summary := workflowAttemptSummary(bead); len(summary) > 0 {
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

func workflowAttemptSummary(bead beads.Bead) map[string]any {
	attempt := workflowAttemptValue(bead)
	if attempt <= 0 {
		return nil
	}
	summary := map[string]any{
		"attempt_count":  attempt,
		"active_attempt": attempt,
	}
	if maxAttempts := metadataInt(bead.Metadata, "gc.max_attempts"); maxAttempts > 0 {
		summary["max_attempts"] = maxAttempts
	}
	return summary
}
