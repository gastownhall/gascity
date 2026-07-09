package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/formula"
)

// Formula write plane: read source, validate a draft, upsert, delete. Writes go
// through FormulaMutator (city-local TOML under <cityRoot>/formulas), gated by
// the supervisor's mutation guards like every other city-scoped write.

const maxFormulaBodyBytes = 1 << 20 // 1 MiB cap on a raw formula body

// withMaxFormulaBody caps the request body huma reads for the body-bearing
// formula ops, so an oversized body cannot exhaust memory before validation.
func withMaxFormulaBody(o *huma.Operation) { o.MaxBodyBytes = maxFormulaBodyBytes }

// reservedFormulaNames are the literal path siblings of /formulas/{name} that a
// same-named formula would shadow. Only "feed" collides: GET /formulas/feed is a
// sibling route, so a formula named "feed" could be written but never read back
// through the /formulas/{name} detail route. The other detail operations (runs,
// source, validate, preview) live one level deeper under /formulas/{name}/..., so
// a formula named e.g. "runs" still routes cleanly to /formulas/{name}; reserving
// those names would reject legal formulas for no routing benefit.
var reservedFormulaNames = map[string]bool{
	"feed": true,
}

// FormulaSourceInput is the input for GET /v0/city/{cityName}/formulas/{name}/source.
type FormulaSourceInput struct {
	CityScope
	Name string `path:"name" minLength:"1" pattern:"\\S" doc:"Formula name."`
}

// FormulaSourceOutput returns a formula's raw TOML source.
type FormulaSourceOutput struct {
	Body struct {
		Name   string `json:"name" doc:"Formula name."`
		Source string `json:"source" doc:"Raw formula TOML source."`
	}
}

func (s *Server) humaHandleFormulaSource(_ context.Context, input *FormulaSourceInput) (*FormulaSourceOutput, error) {
	fm, ok := s.state.(FormulaMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	src, found, err := fm.FormulaSource(input.Name)
	if err != nil {
		return nil, mutationError(err, apierr.FormulaNotFound)
	}
	if !found {
		return nil, apierr.FormulaNotFound.Msg("no editable city-local formula " + input.Name)
	}
	out := &FormulaSourceOutput{}
	out.Body.Name = input.Name
	out.Body.Source = string(src)
	return out, nil
}

// FormulaValidateInput carries a raw formula TOML body to validate.
type FormulaValidateInput struct {
	CityScope
	Name    string `path:"name" minLength:"1" pattern:"\\S" doc:"Formula name."`
	RawBody []byte `doc:"Raw formula TOML source to validate."`
}

// FormulaValidateOutput reports whether the source is valid plus any errors.
type FormulaValidateOutput struct {
	Body struct {
		Valid  bool     `json:"valid" doc:"Whether the formula source is valid."`
		Errors []string `json:"errors,omitempty" doc:"Validation errors, if any."`
	}
}

func (s *Server) humaHandleFormulaValidate(_ context.Context, input *FormulaValidateInput) (*FormulaValidateOutput, error) {
	out := &FormulaValidateOutput{}
	out.Body.Errors = validateFormulaSource(s.state.Config(), input.Name, input.RawBody)
	out.Body.Valid = len(out.Body.Errors) == 0
	return out, nil
}

// FormulaUpsertInput carries the raw formula TOML body to persist.
type FormulaUpsertInput struct {
	CityScope
	Name    string `path:"name" minLength:"1" pattern:"\\S" doc:"Formula name."`
	RawBody []byte `doc:"Raw formula TOML source."`
}

func (s *Server) humaHandleFormulaUpsert(_ context.Context, input *FormulaUpsertInput) (*OKResponse, error) {
	fm, ok := s.state.(FormulaMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	if errs := validateFormulaSource(s.state.Config(), input.Name, input.RawBody); len(errs) > 0 {
		return nil, apierr.InvalidRequest.Msg("formula validation failed: " + strings.Join(errs, "; "))
	}
	if err := fm.UpsertFormula(input.Name, input.RawBody); err != nil {
		return nil, mutationError(err, apierr.FormulaNotFound)
	}
	resp := &OKResponse{}
	resp.Body.Status = "saved"
	return resp, nil
}

// FormulaDeleteInput is the input for DELETE /v0/city/{cityName}/formulas/{name}.
type FormulaDeleteInput struct {
	CityScope
	Name string `path:"name" minLength:"1" pattern:"\\S" doc:"Formula name."`
}

func (s *Server) humaHandleFormulaDelete(_ context.Context, input *FormulaDeleteInput) (*OKResponse, error) {
	fm, ok := s.state.(FormulaMutator)
	if !ok {
		return nil, errMutationsNotSupported
	}
	if err := fm.DeleteFormula(input.Name); err != nil {
		return nil, mutationError(err, apierr.FormulaNotFound)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// validateFormulaSource parses and validates a posted formula TOML, returning
// human-readable error strings (empty = valid). It enforces the same name-slug
// rule as the editor (so validate predicts what upsert will accept), checks that
// the formula's declared name matches the path name, and resolves the draft
// against the active city formula layers so missing parents are rejected before
// save.
func validateFormulaSource(cfg *config.City, name string, content []byte) []string {
	if reservedFormulaNames[name] {
		return []string{fmt.Sprintf("formula name %q is reserved", name)}
	}
	if err := configedit.ValidFormulaName(name); err != nil {
		return []string{err.Error()}
	}
	if len(content) == 0 {
		return []string{"empty formula body"}
	}
	f, err := formula.NewParser().ParseTOML(content)
	if err != nil {
		return []string{err.Error()}
	}
	if f.Formula != "" && f.Formula != name {
		return []string{fmt.Sprintf("formula name %q does not match path name %q", f.Formula, name)}
	}
	if err := f.Validate(); err != nil {
		return []string{err.Error()}
	}
	layers := []string(nil)
	if cfg != nil {
		layers = append([]string(nil), cfg.FormulaLayers.City...)
	}
	if err := validateResolvedFormulaSource(name, content, layers); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// validateResolvedFormulaSource loads and resolves the draft against the active
// city formula layers, anchoring it at the city-local formulas directory the
// editor will save into — the highest-priority City layer, which
// config.ComputeFormulaLayers appends last. Anchoring there resolves a relative
// description_file such as "../prompts/x.md" against the draft's real post-save
// location (<city>/prompts/x.md), so a strict graph.v2 draft using the dominant
// "../prompts/..." / "../assets/..." authoring pattern is not falsely rejected
// for a file that exists at the eventual save path. The draft is parsed in
// memory (ParseTOMLAt), never staged to disk, so validation has no side effects.
func validateResolvedFormulaSource(name string, content []byte, layers []string) error {
	searchPaths := append([]string(nil), layers...)
	saveDir := ""
	if len(searchPaths) > 0 {
		saveDir = searchPaths[len(searchPaths)-1]
	}
	if saveDir == "" {
		// No city formula layers in context (a harness, or a city without a
		// formulas section). Stage an isolated temp dir as the sole search path
		// so extends resolution cannot bind to the process working directory's
		// default formula paths; a relative description_file has no real save
		// anchor here and keeps its prior best-effort handling.
		tempDir, err := os.MkdirTemp("", "gascity-formula-validate-*")
		if err != nil {
			return fmt.Errorf("creating temp formula source: %w", err)
		}
		defer func() {
			_ = os.RemoveAll(tempDir)
		}()
		searchPaths = append(searchPaths, tempDir)
		saveDir = tempDir
	}

	parser := formula.NewParser(searchPaths...)
	srcPath := filepath.Join(saveDir, name+formula.FormulaExtTOML)
	loaded, err := parser.ParseTOMLAt(content, srcPath)
	if err != nil {
		return err
	}
	if _, err := parser.Resolve(loaded); err != nil {
		return err
	}
	return nil
}
