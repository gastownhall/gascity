// Package apicontract validates live HTTP exchanges against the supervisor's
// OpenAPI document and maps requests back to the operationIds they exercise.
//
// It is test support shared by the in-process API contract suite
// (cmd/gc), the internal/api response-validation breadth check, and the
// black-box live contract test (test/integration). Keeping one validator and
// one operation matcher means every layer agrees on what "matches the spec"
// and on which operation a request exercised.
package apicontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi-validator/responses"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
)

// Operation is one (method, path template) pair declared by the spec.
type Operation struct {
	ID     string
	Method string
	Path   string
	// Statuses lists the declared response status keys ("200", "default", ...).
	Statuses []string
}

// Spec is a parsed OpenAPI document plus a response validator and an
// operation index built from it.
type Spec struct {
	model     *v3.Document
	responses responses.ResponseBodyValidator
	ops       []Operation
	matchers  []opMatcher
}

type opMatcher struct {
	op       Operation
	segments []string
	literals int
}

// Load parses specJSON, builds the libopenapi response validator, and indexes
// every operation. It fails when the document cannot be validated against or
// declares an operation without an operationId.
func Load(specJSON []byte) (*Spec, error) {
	doc, err := libopenapi.NewDocument(specJSON)
	if err != nil {
		return nil, fmt.Errorf("parsing OpenAPI document: %w", err)
	}
	model, err := doc.BuildV3Model()
	if err != nil {
		return nil, fmt.Errorf("building OpenAPI model: %w", err)
	}
	ops, err := Operations(specJSON)
	if err != nil {
		return nil, err
	}
	s := &Spec{model: &model.Model, responses: responses.NewResponseBodyValidator(&model.Model), ops: ops}
	for _, op := range ops {
		segs := splitPath(op.Path)
		lit := 0
		for _, seg := range segs {
			if !isParam(seg) {
				lit++
			}
		}
		s.matchers = append(s.matchers, opMatcher{op: op, segments: segs, literals: lit})
	}
	return s, nil
}

// Operations lists every operation declared in specJSON, sorted by
// operationId.
func Operations(specJSON []byte) ([]Operation, error) {
	var raw struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(specJSON, &raw); err != nil {
		return nil, fmt.Errorf("decoding OpenAPI paths: %w", err)
	}
	var ops []Operation
	for path, item := range raw.Paths {
		for method, body := range item {
			upper := strings.ToUpper(method)
			switch upper {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				continue
			}
			var op struct {
				OperationID string                     `json:"operationId"`
				Responses   map[string]json.RawMessage `json:"responses"`
			}
			if err := json.Unmarshal(body, &op); err != nil {
				return nil, fmt.Errorf("decoding operation %s %s: %w", upper, path, err)
			}
			if op.OperationID == "" {
				return nil, fmt.Errorf("operation %s %s has no operationId", upper, path)
			}
			statuses := make([]string, 0, len(op.Responses))
			for code := range op.Responses {
				statuses = append(statuses, code)
			}
			sort.Strings(statuses)
			ops = append(ops, Operation{ID: op.OperationID, Method: upper, Path: path, Statuses: statuses})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops, nil
}

// Operations returns the indexed operations, sorted by operationId.
func (s *Spec) Operations() []Operation {
	out := make([]Operation, len(s.ops))
	copy(out, s.ops)
	return out
}

// Match returns the operation a request with this method and URL path
// exercises. When several templates match, the one with the most literal
// segments wins (so /beads/ready beats /bead/{id}-style siblings).
func (s *Spec) Match(method, urlPath string) (Operation, bool) {
	segs := splitPath(urlPath)
	best := -1
	var found Operation
	for _, m := range s.matchers {
		if m.op.Method != method || len(m.segments) != len(segs) {
			continue
		}
		ok := true
		for i, seg := range m.segments {
			if !isParam(seg) && seg != segs[i] {
				ok = false
				break
			}
		}
		if ok && m.literals > best {
			best = m.literals
			found = m.op
		}
	}
	return found, best >= 0
}

// ValidateResponse checks one exchange against the spec. The request is
// matched to its operation with Match (method-aware, literal segments first)
// rather than the validator's own path lookup, which cannot disambiguate
// sibling templates such as /agent/{base}/{action} (POST) and
// /agent/{dir}/{base} (GET). The response body is supplied separately
// (already drained by the caller) and is restored on resp.Body. The returned
// error lists every schema violation with its field path.
func (s *Spec) ValidateResponse(req *http.Request, resp *http.Response, body []byte) error {
	op, ok := s.Match(req.Method, req.URL.Path)
	if !ok {
		return fmt.Errorf("%s %s matches no operation in the OpenAPI document", req.Method, req.URL.Path)
	}
	pathItem, ok := s.model.Paths.PathItems.Get(op.Path)
	if !ok || pathItem == nil {
		return fmt.Errorf("operation %s: path %s missing from the OpenAPI model", op.ID, op.Path)
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	valid, valErrs := s.responses.ValidateResponseBodyWithPathItem(req, resp, pathItem, op.Path)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if valid && len(valErrs) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -> %d does not match operation %s in the OpenAPI document:\n", req.Method, req.URL.Path, resp.StatusCode, op.ID)
	for _, ve := range valErrs {
		fmt.Fprintf(&b, "  %s: %s\n", ve.Message, ve.Reason)
		for _, se := range ve.SchemaValidationErrors {
			fmt.Fprintf(&b, "    %s at %s\n", se.Reason, se.FieldPath)
		}
	}
	fmt.Fprintf(&b, "  body: %s", truncate(body, 2048))
	return fmt.Errorf("%s", b.String())
}

func splitPath(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

func isParam(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}
