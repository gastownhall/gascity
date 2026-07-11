package api

import (
	"context"
	"errors"
	"sort"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/importsvc"
)

// Seams over importsvc so handler tests can drive list/add/remove without a real
// network fetch (the same injection style cmd/gc uses for its import tests).
var (
	packListImports  = importsvc.ListImports
	packAddImport    = packAddImportFenced
	packRemoveImport = packRemoveImportFenced
)

// packAddImportFenced is the default add seam. It threads validateHTTPPackSource
// into importsvc as the untrusted-source policy so the SSRF fence covers not just
// the caller-supplied source (also pre-checked in humaHandlePackAdd) but every
// transitive import packman resolves during lock sync — a nested internal, file,
// or link-local import in an accepted public pack's pack.toml is rejected before
// its git/cache seam runs.
func packAddImportFenced(fs fsys.FS, cityPath, source, name, version string) (*importsvc.AddResult, error) {
	return importsvc.AddImportWith(fs, cityPath, source, name, version, importsvc.Deps{SourcePolicy: validateHTTPPackSource})
}

// packRemoveImportFenced is the default remove seam. Remove re-syncs the lock
// graph, so it threads the same source policy to fence any transitive import a
// re-resolution would otherwise fetch.
func packRemoveImportFenced(fs fsys.FS, cityPath, name string) (*importsvc.RemoveResult, error) {
	return importsvc.RemoveImportWith(fs, cityPath, name, importsvc.Deps{SourcePolicy: validateHTTPPackSource})
}

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
	IdempotencyKey string `header:"Idempotency-Key" required:"false" doc:"Idempotency key for safe retries."`
	Body           struct {
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
// fence the caller-supplied source, write the [imports.<name>] entry, resolve +
// lock + install, so the pack's templates compose into the city.
// POST /v0/city/{cityName}/packs.
func (s *Server) humaHandlePackAdd(_ context.Context, input *PackAddInput) (*PackAddedOutput, error) {
	// Idempotency: import at most once per Idempotency-Key — a pack add shells
	// out to git and is exactly the expensive, retry-prone create the key is
	// for. The cached value is the AddResult the response body echoes.
	res, err := withIdempotency(s, "/v0/packs", input.IdempotencyKey, input.Body,
		func() (importsvc.AddResult, error) {
			// SSRF fence: AddImport shells `git ls-remote <source>` synchronously and
			// its contract requires HTTP callers to validate the source first. Reject
			// local/file sources and internal-network destinations before the import
			// seam runs. Kept outside the write lock — it is read-only and may resolve
			// DNS.
			if err := validateHTTPPackSource(input.Body.Source); err != nil {
				return importsvc.AddResult{}, packImportHTTPError(err)
			}
			var added *importsvc.AddResult
			if err := s.serializeConfigWrite(func() error {
				var addErr error
				added, addErr = packAddImport(fsys.OSFS{}, s.state.CityPath(), input.Body.Source, input.Body.Name, input.Body.Version)
				return addErr
			}); err != nil {
				return importsvc.AddResult{}, packImportHTTPError(err)
			}
			return *added, nil
		})
	if err != nil {
		return nil, err
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
	var res *importsvc.RemoveResult
	if err := s.serializeConfigWrite(func() error {
		var rmErr error
		res, rmErr = packRemoveImport(fsys.OSFS{}, s.state.CityPath(), input.Name)
		return rmErr
	}); err != nil {
		return nil, packImportHTTPError(err)
	}
	out := &PackRemovedOutput{}
	out.Body.Name = res.Name
	return out, nil
}

// serializeConfigWrite runs fn under the per-city config write lock when the
// state supports it, so pack import add/remove serialize against the
// configedit.Editor boundary the other city-config mutation handlers use.
// A State that does not implement ConfigWriteSerializer (e.g. a read-only test
// double) runs fn directly.
func (s *Server) serializeConfigWrite(fn func() error) error {
	if ser, ok := s.state.(ConfigWriteSerializer); ok {
		return ser.SerializeConfigWrite(fn)
	}
	return fn()
}

// packImportHTTPError maps importsvc sentinels to RFC 9457 problem responses.
func packImportHTTPError(err error) error {
	switch {
	case errors.Is(err, importsvc.ErrInvalidSource), errors.Is(err, importsvc.ErrScopeLoad),
		errors.Is(err, importsvc.ErrNameDerive), errors.Is(err, importsvc.ErrReservedPrefix):
		// ErrNameDerive and ErrReservedPrefix are client input-validation failures
		// (no derivable name, or a reserved "default-rig:" name), so they are 400s
		// like ErrInvalidSource, not 500s.
		return apierr.InvalidRequest.Msg(err.Error())
	case errors.Is(err, importsvc.ErrImportExists):
		return apierr.ConflictWrongState.Msg(err.Error())
	case errors.Is(err, importsvc.ErrNotFound):
		return apierr.PackNotFound.Msg(err.Error())
	case errors.Is(err, importsvc.ErrVersionResolveFailed):
		// Resolving the operator-named source via `git ls-remote` is a genuinely
		// upstream dependency, so a failure here is a bad gateway.
		return apierr.BadGateway.Msg(err.Error())
	case errors.Is(err, importsvc.ErrInstallFailed):
		// ErrInstallFailed wraps LOCAL failures too (the import-graph read,
		// manifest save, lockfile write), not just an upstream clone, so it maps
		// to a server error — matching importsvc's documented HTTP 500.
		return apierr.Internal.With("pack install failed", &huma.ErrorDetail{Message: err.Error()})
	default:
		return apierr.Internal.With("pack import failed", &huma.ErrorDetail{Message: err.Error()})
	}
}
