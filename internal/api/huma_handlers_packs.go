package api

import (
	"context"
	"errors"
	"sort"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/importsvc"
)

// Seams over importsvc so handler tests can drive list/add/remove without a real
// network fetch (the same injection style cmd/gc uses for its import tests).
var (
	packListImports  = importsvc.ListImports
	packAddImport    = importsvc.AddImport
	packRemoveImport = importsvc.RemoveImport
)

// PackListBody is the response body for GET /v0/packs.
type PackListBody struct {
	Packs []packResponse `json:"packs" doc:"Registered packs."`
}

// PackListOutput is the response envelope for GET /v0/packs.
type PackListOutput struct {
	Body PackListBody
}

// humaHandlePackList lists the city's direct, removable pack imports — the same
// [imports.<name>] binding namespace that humaHandlePackAdd writes and
// humaHandlePackRemove deletes by name — so list/add/remove all operate on one
// namespace. It deliberately does NOT list the legacy [packs] migration table
// nor the transitive CollectAllImports closure. GET /v0/city/{cityName}/packs.
func (s *Server) humaHandlePackList(_ context.Context, _ *PackListInput) (*PackListOutput, error) {
	imports, err := packListImports(fsys.OSFS{}, s.state.CityPath())
	if err != nil {
		return nil, packImportHTTPError(err)
	}
	names := make([]string, 0, len(imports))
	for name := range imports {
		names = append(names, name)
	}
	sort.Strings(names)
	packs := make([]packResponse, 0, len(names))
	for _, name := range names {
		imp := imports[name]
		packs = append(packs, packResponse{
			Name:    name,
			Source:  imp.Source,
			Version: imp.Version,
		})
	}
	out := &PackListOutput{}
	out.Body.Packs = packs
	return out, nil
}

// PackAddInput is the body for POST /v0/city/{cityName}/packs.
type PackAddInput struct {
	CityScope
	Body struct {
		Source  string `json:"source" minLength:"1" doc:"Pack source: a remote git URL or registry ref (a sub-path of a repo is allowed)." example:"https://github.com/org/repo/tree/main/packs/review"`
		Name    string `json:"name,omitempty" doc:"Optional local binding name override; derived from the source when omitted."`
		Version string `json:"version,omitempty" doc:"Optional semver constraint for a git-backed pack." example:"^1.2.0"`
	}
}

// PackAddedOutput echoes the binding importsvc durably wrote.
type PackAddedOutput struct {
	Body struct {
		Name      string `json:"name" doc:"The local binding name written to [imports.<name>]."`
		Source    string `json:"source" doc:"The canonical source string written to the manifest."`
		Version   string `json:"version,omitempty" doc:"The version constraint written, if any."`
		GitBacked bool   `json:"git_backed" doc:"Whether the resolved source is git-backed (has a lock entry)."`
	}
}

// humaHandlePackAdd adds a pack to the city by import (the gc-import path):
// validate the source, write the [imports.<name>] entry, resolve + lock + install,
// so the pack's templates compose into the city. POST /v0/city/{cityName}/packs.
func (s *Server) humaHandlePackAdd(_ context.Context, input *PackAddInput) (*PackAddedOutput, error) {
	res, err := packAddImport(fsys.OSFS{}, s.state.CityPath(), input.Body.Source, input.Body.Name, input.Body.Version)
	if err != nil {
		return nil, packImportHTTPError(err)
	}
	out := &PackAddedOutput{}
	out.Body.Name = res.Name
	out.Body.Source = res.Source
	out.Body.Version = res.Version
	out.Body.GitBacked = res.GitBacked
	return out, nil
}

// PackRemoveInput targets DELETE /v0/city/{cityName}/packs/{name}.
type PackRemoveInput struct {
	CityScope
	Name string `path:"name" doc:"The import binding name to remove (the [imports.<name>] key)."`
}

// PackRemovedOutput echoes the removed binding.
type PackRemovedOutput struct {
	Body struct {
		Name string `json:"name" doc:"The binding name removed."`
	}
}

// humaHandlePackRemove drops a pack import from the city; its templates leave the
// composed config on the next reload. DELETE /v0/city/{cityName}/packs/{name}.
func (s *Server) humaHandlePackRemove(_ context.Context, input *PackRemoveInput) (*PackRemovedOutput, error) {
	res, err := packRemoveImport(fsys.OSFS{}, s.state.CityPath(), input.Name)
	if err != nil {
		return nil, packImportHTTPError(err)
	}
	out := &PackRemovedOutput{}
	out.Body.Name = res.Name
	return out, nil
}

// packImportHTTPError maps importsvc sentinels to RFC 9457 problem responses.
func packImportHTTPError(err error) error {
	switch {
	case errors.Is(err, importsvc.ErrInvalidSource), errors.Is(err, importsvc.ErrScopeLoad):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, importsvc.ErrImportExists):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, importsvc.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, importsvc.ErrVersionResolveFailed):
		// Resolving the operator-named source via `git ls-remote` is a genuinely
		// upstream dependency, so a failure here is a bad gateway.
		return huma.Error502BadGateway(err.Error())
	case errors.Is(err, importsvc.ErrInstallFailed):
		// ErrInstallFailed wraps LOCAL failures too (the import-graph read,
		// manifest save, lockfile write), not just an upstream clone, so it maps
		// to a server error — matching importsvc's documented HTTP 500.
		return huma.Error500InternalServerError("pack install failed", err)
	default:
		return huma.Error500InternalServerError("pack import failed", err)
	}
}
