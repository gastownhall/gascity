package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// humaHandleServiceList is the Huma-typed handler for GET /v0/services.
func (s *Server) humaHandleServiceList(_ context.Context, _ *ServiceListInput) (*ListOutput[json.RawMessage], error) {
	reg := s.state.ServiceRegistry()
	index := s.latestIndex()
	if reg == nil {
		return &ListOutput[json.RawMessage]{
			Index: index,
			Body:  ListBody[json.RawMessage]{Items: []json.RawMessage{}, Total: 0},
		}, nil
	}
	items := reg.List()
	rawItems := make([]json.RawMessage, len(items))
	for i, item := range items {
		b, _ := json.Marshal(item)
		rawItems[i] = b
	}
	return &ListOutput[json.RawMessage]{
		Index: index,
		Body:  ListBody[json.RawMessage]{Items: rawItems, Total: len(rawItems)},
	}, nil
}

// humaHandleServiceGet is the Huma-typed handler for GET /v0/service/{name}.
func (s *Server) humaHandleServiceGet(_ context.Context, input *ServiceGetInput) (*IndexOutput[json.RawMessage], error) {
	reg := s.state.ServiceRegistry()
	if reg == nil {
		return nil, huma.Error404NotFound("service " + input.Name + " not found")
	}
	item, ok := reg.Get(input.Name)
	if !ok {
		return nil, huma.Error404NotFound("service " + input.Name + " not found")
	}
	raw, _ := json.Marshal(item)
	return &IndexOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  raw,
	}, nil
}

// humaHandleServiceRestart is the Huma-typed handler for POST /v0/service/{name}/restart.
func (s *Server) humaHandleServiceRestart(_ context.Context, input *ServiceRestartInput) (*ServiceRestartOutput, error) {
	name := input.Name
	reg := s.state.ServiceRegistry()
	if reg == nil {
		return nil, huma.Error404NotFound("service " + name + " not found")
	}
	if err := reg.Restart(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, huma.Error404NotFound(err.Error())
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}
	out := &ServiceRestartOutput{}
	out.Body.Status = "ok"
	out.Body.Action = "restart"
	out.Body.Service = name
	return out, nil
}
