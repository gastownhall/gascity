package api

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

type socketEventsListPayload struct {
	Type   string `json:"type,omitempty"`
	Actor  string `json:"actor,omitempty"`
	Since  string `json:"since,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

func init() {
	RegisterAction("events.list", ActionDef{
		Description:       "List events",
		RequiresCityScope: true,
		SupportsWatch:     true,
	}, func(_ context.Context, s *Server, payload socketEventsListPayload) (listResponse, error) {
		filter := events.Filter{Type: payload.Type, Actor: payload.Actor}
		if payload.Since != "" {
			d, err := time.ParseDuration(payload.Since)
			if err != nil {
				return listResponse{}, httpError{status: 400, code: "invalid", message: err.Error()}
			}
			filter.Since = time.Now().Add(-d)
		}
		items, err := s.Events.List(filter)
		if err != nil {
			return listResponse{}, err
		}
		pp := socketPageParams(payload.Limit, payload.Cursor, 100)
		if !pp.IsPaging {
			total := len(items)
			if pp.Limit < len(items) {
				items = items[:pp.Limit]
			}
			return listResponse{Items: items, Total: total}, nil
		}
		page, total, nextCursor := paginate(items, pp)
		if page == nil {
			page = []events.Event{}
		}
		return listResponse{Items: page, Total: total, NextCursor: nextCursor}, nil
	})

	RegisterAction("event.emit", ActionDef{
		Description:       "Emit an event",
		IsMutation:        true,
		RequiresCityScope: true,
	}, func(_ context.Context, s *Server, payload eventEmitRequest) (map[string]string, error) {
		if payload.Type == "" {
			return nil, httpError{status: 400, code: "invalid", message: "type is required"}
		}
		if payload.Actor == "" {
			return nil, httpError{status: 400, code: "invalid", message: "actor is required"}
		}
		ep := s.state.EventProvider()
		if ep == nil {
			return nil, httpError{status: 503, code: "unavailable", message: "events not enabled"}
		}
		ep.Record(events.Event{
			Type:    payload.Type,
			Actor:   payload.Actor,
			Subject: payload.Subject,
			Message: payload.Message,
		})
		return map[string]string{"status": "recorded"}, nil
	})

	registerRawSupervisorAction("events.list", ActionDef{
		Description: "List events",
		ServerRoles: actionServerRoleSupervisor,
	}, func(sm *SupervisorMux, req *socketRequestEnvelope) (socketActionResult, *socketErrorEnvelope, bool) {
		if req.Scope != nil && req.Scope.City != "" {
			return socketActionResult{}, nil, false
		}
		result, err := sm.globalEventList(req)
		if err != nil {
			return socketActionResult{}, socketErrorFor(req.ID, err), true
		}
		return socketActionResult{Result: result}, nil, true
	})
}
