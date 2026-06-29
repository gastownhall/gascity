package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/formula"
)

// Formula write plane: read source, validate a draft, upsert, delete. Writes go
// through FormulaMutator (city-local TOML under <cityRoot>/formulas), gated by
// the supervisor's mutation guards like every other city-scoped write.

const maxFormulaBodyBytes = 1 << 20 // 1 MiB cap on a raw formula body

// withMaxFormulaBody caps the request body huma reads for the body-bearing
// formula ops, so an oversized body cannot exhaust memory before validation.
func withMaxFormulaBody(o *huma.Operation) { o.MaxBodyBytes = maxFormulaBodyBytes }

// reservedFormulaNames are path literals under /formulas that a same-named
// formula would shadow; saving one is rejected to avoid confusing routing.
var reservedFormulaNames = map[string]bool{
	"feed": true, "runs": true, "source": true, "validate": true, "preview": true,
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
		return nil, mutationError(err)
	}
	if !found {
		return nil, huma.Error404NotFound("no editable city-local formula " + input.Name)
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
	out.Body.Errors = validateFormulaSource(input.Name, input.RawBody)
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
	if errs := validateFormulaSource(input.Name, input.RawBody); len(errs) > 0 {
		return nil, huma.Error400BadRequest("formula validation failed: " + strings.Join(errs, "; "))
	}
	if err := fm.UpsertFormula(input.Name, input.RawBody); err != nil {
		return nil, mutationError(err)
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
		return nil, mutationError(err)
	}
	resp := &OKResponse{}
	resp.Body.Status = "deleted"
	return resp, nil
}

// validateFormulaSource parses and validates a posted formula TOML, returning
// human-readable error strings (empty = valid). It also enforces that the
// formula's declared name matches the path name so a save can't be misfiled.
func validateFormulaSource(name string, content []byte) []string {
	if reservedFormulaNames[name] {
		return []string{fmt.Sprintf("formula name %q is reserved", name)}
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
	return nil
}
