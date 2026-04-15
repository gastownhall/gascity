package api

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
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
	ServerRoles       actionServerRoles
}

// actionHandler is the raw dispatch signature used internally by the framework.
type actionHandler func(s *Server, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope)
type supervisorActionHandler func(sm *SupervisorMux, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope, bool)

// actionEntry combines metadata with a runtime handler. The RequestType and
// ResponseType fields are populated by the generic RegisterAction helper so the
// spec generator can reflect on them without importing handler-specific types.
type actionEntry struct {
	Name              string
	Description       string
	IsMutation        bool
	RequiresCityScope bool
	SupportsWatch     bool
	ServerRoles       actionServerRoles
	RequestType       reflect.Type // nil for void actions
	ResponseType      reflect.Type
	Handler           actionHandler
	SupervisorHandler supervisorActionHandler
}

type actionServerRoles uint8

const (
	actionServerRoleCity actionServerRoles = 1 << iota
	actionServerRoleSupervisor
	actionServerRoleAny = actionServerRoleCity | actionServerRoleSupervisor
)

func normalizeActionServerRoles(roles actionServerRoles) actionServerRoles {
	if roles == 0 {
		return actionServerRoleAny
	}
	return roles
}

func (e *actionEntry) supportsRole(role actionServerRoles) bool {
	return normalizeActionServerRoles(e.ServerRoles)&role != 0
}

var (
	actionTableMu sync.Mutex
	actionTable   = map[string]*actionEntry{}
)

var protocolCapabilityNames = []string{
	"subscription.start",
	"subscription.stop",
}

// RegisterAction registers a typed action handler. The generic In/Out types
// drive both runtime dispatch (JSON decode/encode) AND spec generation (the
// reflect.Type is fed to the swaggest reflector). Handlers are pure business
// logic — they never see envelopes.
func RegisterAction[In, Out any](name string, def ActionDef, handler func(context.Context, *Server, In) (Out, error)) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	entry := upsertActionEntry(name, def)
	setActionTypes(entry, reflect.TypeOf((*In)(nil)).Elem(), reflect.TypeOf((*Out)(nil)).Elem())
	if entry.Handler != nil {
		panic("duplicate city handler registration for action " + name)
	}
	entry.Handler = func(s *Server, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
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
	}
}

// RegisterVoidAction registers an action that takes no payload.
func RegisterVoidAction[Out any](name string, def ActionDef, handler func(context.Context, *Server) (Out, error)) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	entry := upsertActionEntry(name, def)
	setActionTypes(entry, nil, reflect.TypeOf((*Out)(nil)).Elem())
	if entry.Handler != nil {
		panic("duplicate city handler registration for action " + name)
	}
	entry.Handler = func(s *Server, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
		result, err := handler(req.dispatchCtx, s)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err)
		}
		return socketActionResult{Result: result}, nil
	}
}

// RegisterSupervisorVoidAction registers a supervisor-scoped action that takes
// no payload. The action still participates in the shared registry used for
// capabilities and spec generation.
func RegisterSupervisorVoidAction[Out any](name string, def ActionDef, handler func(context.Context, *SupervisorMux) (Out, error)) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	entry := upsertActionEntry(name, def)
	setActionTypes(entry, nil, reflect.TypeOf((*Out)(nil)).Elem())
	if entry.SupervisorHandler != nil {
		panic("duplicate supervisor handler registration for action " + name)
	}
	entry.SupervisorHandler = func(sm *SupervisorMux, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope, bool) {
		result, err := handler(req.dispatchCtx, sm)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err), true
		}
		return socketActionResult{Result: result}, nil, true
	}
}

// registerRawAction registers an action with direct access to the request
// envelope. Use this only when the handler needs framework-level fields
// like req.dispatchIndex. Prefer RegisterAction for all other cases.
func registerRawAction(name string, def ActionDef, handler actionHandler) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	entry := upsertActionEntry(name, def)
	if entry.Handler != nil {
		panic("duplicate city handler registration for action " + name)
	}
	entry.Handler = handler
}

func registerRawSupervisorAction(name string, def ActionDef, handler supervisorActionHandler) {
	actionTableMu.Lock()
	defer actionTableMu.Unlock()
	entry := upsertActionEntry(name, def)
	if entry.SupervisorHandler != nil {
		panic("duplicate supervisor handler registration for action " + name)
	}
	entry.SupervisorHandler = handler
}

// dispatchAction is the single pipeline for all WS actions:
// scope validation → read-only guard → idempotency → handler → idempotency store → watch
func (s *Server) dispatchAction(req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope) {
	entry, ok := actionTable[req.Action]
	if !ok {
		return socketActionResult{}, newSocketError(req.ID, "not_found", "unknown action: "+req.Action)
	}
	if !entry.supportsRole(actionServerRoleCity) {
		return socketActionResult{}, unsupportedSocketActionForRole(req.ID, req.Action, "city")
	}
	if entry.Handler == nil {
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

func (sm *SupervisorMux) dispatchSupervisorAction(req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope, bool) {
	entry, ok := actionTable[req.Action]
	if !ok || !entry.supportsRole(actionServerRoleSupervisor) || entry.SupervisorHandler == nil {
		return socketActionResult{}, nil, false
	}
	if sm.readOnly && entry.IsMutation {
		return socketActionResult{}, newSocketError(req.ID, "read_only",
			"mutations disabled: server bound to non-localhost address"), true
	}
	result, apiErr, handled := entry.SupervisorHandler(sm, req)
	return result, apiErr, handled
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
func actionTableCapabilities(role actionServerRoles) []string {
	caps := make([]string, 0, len(actionTable)+len(protocolCapabilityNames))
	for name, entry := range actionTable {
		if entry.supportsRole(role) {
			caps = append(caps, name)
		}
	}
	caps = append(caps, protocolCapabilityNames...)
	sort.Strings(caps)
	return caps
}

func actionTableSupportsRole(action string, role actionServerRoles) bool {
	entry, ok := actionTable[action]
	return ok && entry.supportsRole(role)
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

// --- Shared payload types used by multiple dispatch_*.go files ---

type socketNamePayload struct {
	Name string `json:"name"`
}

type socketIDPayload struct {
	ID string `json:"id"`
}

func socketPageParams(limit *int, cursor string, defaultLimit int) pageParams {
	pp := pageParams{
		Limit:    defaultLimit,
		IsPaging: strings.TrimSpace(cursor) != "",
	}
	if limit != nil {
		switch {
		case *limit == 0:
			pp.Limit = maxPaginationLimit
		case *limit > 0:
			pp.Limit = *limit
		}
	}
	if pp.Limit > maxPaginationLimit {
		pp.Limit = maxPaginationLimit
	}
	if cursor != "" {
		pp.Offset = decodeCursor(cursor)
	}
	return pp
}

func upsertActionEntry(name string, def ActionDef) *actionEntry {
	entry, ok := actionTable[name]
	if !ok {
		entry = &actionEntry{Name: name}
		actionTable[name] = entry
	}
	if entry.Description == "" {
		entry.Description = def.Description
	}
	entry.IsMutation = entry.IsMutation || def.IsMutation
	entry.RequiresCityScope = entry.RequiresCityScope || def.RequiresCityScope
	entry.SupportsWatch = entry.SupportsWatch || def.SupportsWatch
	roles := normalizeActionServerRoles(def.ServerRoles)
	if entry.ServerRoles == 0 {
		entry.ServerRoles = roles
	} else {
		entry.ServerRoles = normalizeActionServerRoles(entry.ServerRoles) | roles
	}
	return entry
}

func setActionTypes(entry *actionEntry, requestType, responseType reflect.Type) {
	if requestType != nil {
		if entry.RequestType != nil && entry.RequestType != requestType {
			panic("conflicting request type registration for action " + entry.Name)
		}
		entry.RequestType = requestType
	}
	if responseType != nil {
		if entry.ResponseType != nil && entry.ResponseType != responseType {
			panic("conflicting response type registration for action " + entry.Name)
		}
		entry.ResponseType = responseType
	}
}
