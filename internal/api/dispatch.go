package api

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"sync"

	"github.com/gastownhall/gascity/internal/api/specgen"
)

// ActionDef holds the metadata for registering a WebSocket action.
// Used by RegisterAction and RegisterVoidAction.
type ActionDef struct {
	Description       string
	IsMutation        bool
	RequiresCityScope bool
	SupportsWatch     bool
}

// actionHandler is the raw dispatch signature used internally by the framework.
type actionHandler func(s *Server, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope)

// actionEntry combines metadata with a runtime handler. The RequestType and
// ResponseType fields are populated by the generic RegisterAction helper so the
// spec generator can reflect on them without importing handler-specific types.
type actionEntry struct {
	Name              string
	Description       string
	IsMutation        bool
	RequiresCityScope bool
	SupportsWatch     bool
	RequestType       reflect.Type // nil for void actions
	ResponseType      reflect.Type
	Handler           actionHandler // nil = legacy fallback during migration
}

var (
	actionTableMu sync.Mutex
	actionTable   = map[string]*actionEntry{}
)

// RegisterAction registers a typed action handler. The generic In/Out types
// drive both runtime dispatch (JSON decode/encode) AND spec generation (the
// reflect.Type is fed to the swaggest reflector). Handlers are pure business
// logic — they never see envelopes.
func RegisterAction[In, Out any](name string, def ActionDef, handler func(context.Context, *Server, In) (Out, error)) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	actionTable[name] = &actionEntry{
		Name:              name,
		Description:       def.Description,
		IsMutation:        def.IsMutation,
		RequiresCityScope: def.RequiresCityScope,
		SupportsWatch:     def.SupportsWatch,
		RequestType:       reflect.TypeOf((*In)(nil)).Elem(),
		ResponseType:      reflect.TypeOf((*Out)(nil)).Elem(),
		Handler: func(s *Server, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
			var input In
			if len(req.Payload) > 0 {
				if err := json.Unmarshal(req.Payload, &input); err != nil {
					return socketActionResult{}, newSocketError(req.ID, "invalid", err.Error())
				}
			}
			result, err := handler(req.dispatchCtx, s, input)
			if err != nil {
				return socketActionResult{}, socketErrorFor(req.ID, err)
			}
			return socketActionResult{Result: result}, nil
		},
	}
}

// RegisterVoidAction registers an action that takes no payload.
func RegisterVoidAction[Out any](name string, def ActionDef, handler func(context.Context, *Server) (Out, error)) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	actionTable[name] = &actionEntry{
		Name:              name,
		Description:       def.Description,
		IsMutation:        def.IsMutation,
		RequiresCityScope: def.RequiresCityScope,
		SupportsWatch:     def.SupportsWatch,
		ResponseType:      reflect.TypeOf((*Out)(nil)).Elem(),
		Handler: func(s *Server, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
			result, err := handler(req.dispatchCtx, s)
			if err != nil {
				return socketActionResult{}, socketErrorFor(req.ID, err)
			}
			return socketActionResult{Result: result}, nil
		},
	}
}

// registerRawAction registers an action with direct access to the request
// envelope. Use this only when the handler needs framework-level fields
// like req.dispatchIndex. Prefer RegisterAction for all other cases.
func registerRawAction(name string, def ActionDef, handler actionHandler) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	actionTable[name] = &actionEntry{
		Name:              name,
		Description:       def.Description,
		IsMutation:        def.IsMutation,
		RequiresCityScope: def.RequiresCityScope,
		SupportsWatch:     def.SupportsWatch,
		Handler:           handler,
	}
}

// RegisterMeta registers action metadata without a handler. Used during
// incremental migration — the action appears in capabilities and spec
// but dispatch falls back to the legacy switch.
func RegisterMeta(name string, def ActionDef) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	actionTable[name] = &actionEntry{
		Name:              name,
		Description:       def.Description,
		IsMutation:        def.IsMutation,
		RequiresCityScope: def.RequiresCityScope,
		SupportsWatch:     def.SupportsWatch,
	}
}

// dispatchAction is the single pipeline for all WS actions:
// scope validation → read-only guard → idempotency → handler → idempotency store → watch
func (s *Server) dispatchAction(req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
	entry, ok := actionTable[req.Action]
	if !ok || entry.Handler == nil {
		return socketActionResult{}, newSocketError(req.ID, "not_found", "unknown action: "+req.Action)
	}

	// 1. Scope validation
	if req.Scope != nil && req.Scope.City != "" {
		if cityName := s.state.CityName(); req.Scope.City != cityName {
			return socketActionResult{}, newSocketError(req.ID, "invalid",
				"scope.city "+req.Scope.City+" does not match this city "+cityName)
		}
	}

	// 2. Read-only guard
	if s.readOnly && entry.IsMutation {
		return socketActionResult{}, newSocketError(req.ID, "read_only",
			"mutations disabled: server bound to non-localhost address")
	}

	// 3. Idempotency check (framework concern — handlers never see this)
	var idemKey, bodyHash string
	if req.IdempotencyKey != "" && entry.IsMutation {
		idemKey = socketScopedIdemKey(s.state.CityName(), req.Action, req.IdempotencyKey)
		bodyHash = hashBody(req.Payload)
		cached, handled, idemErr := s.idem.checkIdempotent(idemKey, bodyHash)
		if idemErr != nil {
			idemErr.ID = req.ID
			return socketActionResult{}, idemErr
		}
		if handled {
			return socketActionResult{Result: cached}, nil
		}
	}

	// 4. Snapshot the event index ONCE (single I/O call, shared by handler and response).
	// Set on req.dispatchIndex so handlers that need it (e.g., workflow.get) can read it
	// without a redundant LatestSeq() call.
	index := s.latestIndex()
	req.dispatchIndex = index

	// 5. Call handler (pure business logic)
	result, apiErr := entry.Handler(s, req)

	// 6. Set the index on the response envelope
	if apiErr == nil {
		result.Index = index
	}

	// 6. Idempotency store/unreserve
	if idemKey != "" {
		if apiErr != nil {
			s.idem.unreserve(idemKey)
		} else {
			s.idem.storeResponse(idemKey, bodyHash, 200, result.Result)
		}
	}

	// 6. Watch semantics (framework concern — handlers never see this)
	if apiErr == nil && req.Watch != nil && entry.SupportsWatch {
		if ep := s.state.EventProvider(); ep != nil {
			bp := socketBlockingParams(req.Watch)
			if bp.HasIndex {
				result.Index = waitForChange(req.dispatchCtx, ep, bp)
			}
		}
	}

	return result, apiErr
}

// ActionTableRegistry builds a specgen.Registry from the action table.
// Used by cmd/specgen to generate specs from the same source of truth
// that drives runtime dispatch.
func ActionTableRegistry() *specgen.Registry {
	r := specgen.NewRegistry()
	for _, entry := range actionTable {
		r.Register(specgen.ActionDef{
			Action:       entry.Name,
			Description:  entry.Description,
			RequestType:  entry.RequestType,
			ResponseType: entry.ResponseType,
			IsMutation:   entry.IsMutation,
		})
	}
	return r
}

// EnvelopeTypes returns the actual protocol envelope types for spec generation.
// These are the REAL types used on the wire — no mirrors, no drift.
func EnvelopeTypes() specgen.EnvelopeTypes {
	return specgen.EnvelopeTypes{
		Request:           new(RequestEnvelope),
		Response:          new(ResponseEnvelope),
		Hello:             new(HelloEnvelope),
		Error:             new(ErrorEnvelope),
		Event:             new(EventEnvelope),
		SubscriptionStart: new(SubscriptionStartPayload),
		SubscriptionStop:  new(SubscriptionStopPayload),
	}
}

// actionTableCapabilities returns sorted action names for the hello envelope.
func actionTableCapabilities() []string {
	caps := make([]string, 0, len(actionTable))
	for name := range actionTable {
		caps = append(caps, name)
	}
	sort.Strings(caps)
	return caps
}

// actionTableRequiresCityScope checks whether an action requires city scope.
func actionTableRequiresCityScope(action string) bool {
	if entry, ok := actionTable[action]; ok {
		return entry.RequiresCityScope
	}
	return false
}

// actionTableSupportsWatch checks whether an action supports blocking queries.
func actionTableSupportsWatch(action string) bool {
	if entry, ok := actionTable[action]; ok {
		return entry.SupportsWatch
	}
	return false
}
