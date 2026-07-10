package apierr

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"
)

// ErrorModel is the Gas City problem+json body. It embeds huma.ErrorModel (so it
// inherits the RFC 9457 shape — type/title/status/detail/instance/errors — plus
// the StatusError behavior and the application/problem+json content type) and
// adds a first-class machine-readable `code`. The Go type is named ErrorModel so
// Huma's DefaultSchemaNamer keeps the OpenAPI schema name "ErrorModel"; `code` is
// an additive, omitempty member, so the wire shape stays backward compatible.
//
// The canonical machine identifier is the `type` URN (urn:gascity:error:<code>);
// `code` is a convenience projection of the URN's final segment for consumers
// that switch on short slugs. The registry entry is the single source of truth.
type ErrorModel struct {
	huma.ErrorModel
	Code string `json:"code,omitempty" doc:"Stable machine-readable error code (the final segment of the type URN)."`
}

// new builds an ErrorModel stamped with this problem type's URN, code, title, and
// status, at the given occurrence-specific detail and status.
func (pt ProblemType) new(status int, detail string, details []*huma.ErrorDetail) *ErrorModel {
	return &ErrorModel{
		ErrorModel: huma.ErrorModel{
			Type:   pt.URN(),
			Title:  pt.Title,
			Status: status,
			Detail: detail,
			Errors: details,
		},
		Code: pt.Code,
	}
}

// Msg builds an error of this problem type at its default status with a
// human-readable detail. This is the primary constructor — the one way to mint a
// gascity API error so the registered code/URN is always stamped.
func (pt ProblemType) Msg(detail string) *ErrorModel { return pt.new(pt.Status, detail, nil) }

// Msgf is Msg with a printf-style detail.
func (pt ProblemType) Msgf(format string, a ...any) *ErrorModel {
	return pt.new(pt.Status, fmt.Sprintf(format, a...), nil)
}

// With builds an error carrying an errors[] list of individual detail entries
// (RFC 9457 "errors" member) in addition to the top-level detail.
func (pt ProblemType) With(detail string, details ...*huma.ErrorDetail) *ErrorModel {
	return pt.new(pt.Status, detail, details)
}

// WithStatus overrides the default status for the rare case where one problem
// type maps to more than one status. The code/URN/title are unchanged.
func (pt ProblemType) WithStatus(status int, detail string) *ErrorModel {
	return pt.new(status, detail, nil)
}
