package api

import (
	"context"
	"errors"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// humaHandleServiceList is the Huma-typed handler for GET /v0/services.
func (s *Server) humaHandleServiceList(_ context.Context, _ *ServiceListInput) (*ListOutput[workspacesvc.Status], error) {
	reg := s.state.ServiceRegistry()
	index := s.latestIndex()
	if reg == nil {
		return &ListOutput[workspacesvc.Status]{
			Index: index,
			Body:  ListBody[workspacesvc.Status]{Items: []workspacesvc.Status{}, Total: 0},
		}, nil
	}
	items := reg.List()
	return &ListOutput[workspacesvc.Status]{
		Index: index,
		Body:  ListBody[workspacesvc.Status]{Items: items, Total: len(items)},
	}, nil
}

// humaHandleServiceGet is the Huma-typed handler for GET /v0/service/{name}.
func (s *Server) humaHandleServiceGet(_ context.Context, input *ServiceGetInput) (*IndexOutput[workspacesvc.Status], error) {
	reg := s.state.ServiceRegistry()
	if reg == nil {
		return nil, apierr.ServiceNotFound.Msg("service " + input.Name + " not found")
	}
	item, ok := reg.Get(input.Name)
	if !ok {
		return nil, apierr.ServiceNotFound.Msg("service " + input.Name + " not found")
	}
	return &IndexOutput[workspacesvc.Status]{
		Index: s.latestIndex(),
		Body:  item,
	}, nil
}

// humaHandleServiceRestart is the Huma-typed handler for POST /v0/service/{name}/restart.
func (s *Server) humaHandleServiceRestart(_ context.Context, input *ServiceRestartInput) (*ServiceRestartOutput, error) {
	name := input.Name
	reg := s.state.ServiceRegistry()
	if reg == nil {
		return nil, apierr.ServiceNotFound.Msg("service " + name + " not found")
	}
	if err := reg.Restart(name); err != nil {
		if errors.Is(err, workspacesvc.ErrServiceNotFound) {
			return nil, apierr.ServiceNotFound.Msg(err.Error())
		}
		return nil, apierr.Internal.Msg(err.Error())
	}
	out := &ServiceRestartOutput{}
	out.Body.Status = "ok"
	out.Body.Action = "restart"
	out.Body.Service = name
	return out, nil
}
