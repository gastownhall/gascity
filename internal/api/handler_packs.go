package api

import (
	"net/http"
	"sort"
)

type packResponse struct {
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`
}

func (s *Server) handlePackList(w http.ResponseWriter, _ *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{"packs": packs})
}
