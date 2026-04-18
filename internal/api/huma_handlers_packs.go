package api

import (
	"context"
	"sort"
)

// PackListBody is the response body for GET /v0/packs.
type PackListBody struct {
	Packs []packResponse `json:"packs" doc:"Registered packs."`
}

// PackListOutput is the response envelope for GET /v0/packs.
type PackListOutput struct {
	Body PackListBody
}

// humaHandlePackList is the Huma-typed handler for GET /v0/packs.
func (s *Server) humaHandlePackList(_ context.Context, _ *PackListInput) (*PackListOutput, error) {
	cfg := s.state.Config()
	names := make([]string, 0, len(cfg.Packs))
	for name := range cfg.Packs {
		names = append(names, name)
	}
	sort.Strings(names)
	packs := make([]packResponse, 0, len(names))
	for _, name := range names {
		src := cfg.Packs[name]
		packs = append(packs, packResponse{
			Name:   name,
			Source: src.Source,
			Ref:    src.Ref,
			Path:   src.Path,
		})
	}
	out := &PackListOutput{}
	out.Body.Packs = packs
	return out, nil
}
