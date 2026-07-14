package api

import (
	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
)

// validationFailedDetail is the exact detail string Huma uses for its built-in
// request-validation failure. Huma emits WriteErr(..., "validation failed", ...)
// for schema/param/body validation, and the accompanying status is NOT always
// 422: it is 400 for an unparseable body and 415 for an unsupported content type
// (huma.go validateBody), besides the usual 422. We therefore key the stamp on
// this exact marker string at whatever status Huma chose, preserving that status
// — so every built-in validation failure carries the validation-failed type,
// while the many hand-written huma.Error*(...) call sites (distinct messages)
// stay on the legacy path until explicitly converted.
const validationFailedDetail = "validation failed"

// init replaces huma.NewError so every error the API produces is an
// *apierr.ErrorModel — the RFC 9457 problem+json body with a first-class machine
// `code`. This runs at package-init time, before NewSupervisorMux calls
// huma.Register, so every registered error response and every served error flows
// through it.
//
// Two behaviors:
//
//   - Huma's built-in request validation ("validation failed", at 422, or 400
//     for an unparseable body / 415 for an unsupported content type) is stamped
//     with the validation-failed problem type (type URN + code + title), while
//     preserving Huma's status and the occurrence detail + field-level errors[].
//     This is the one auto-stamped fallback; it gives request validation — the
//     most common client-visible error, emitted by Huma itself rather than at
//     our call sites — a stable machine identity.
//
//   - Every other error is wrapped verbatim: we take Huma's own ErrorModel and
//     re-home it inside *apierr.ErrorModel with an empty Code. Because Code is
//     omitempty and Type stays empty, the JSON is byte-identical to Huma's
//     default. Absence of a code is the signal for "legacy / not-yet-converted
//     call site"; converted sites mint their error through the apierr
//     constructors instead, which bypass this override entirely.
//
// Overriding NewError also covers NewErrorWithContext: Huma's default
// NewErrorWithContext delegates to the NewError package var at call time, and
// the serving path (WriteErr) goes through NewErrorWithContext.
func init() {
	base := huma.NewError
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		// Reuse Huma's own construction so the embedded model — including its
		// exact errs→ErrorDetail conversion — is never reimplemented here.
		model, ok := base(status, msg, errs...).(*huma.ErrorModel)
		if !ok {
			// A non-default base (another override chained ahead of us) — leave it
			// untouched rather than guess at its shape.
			return base(status, msg, errs...)
		}
		if msg == validationFailedDetail {
			return &apierr.ErrorModel{
				ErrorModel: huma.ErrorModel{
					Type:     apierr.ValidationFailed.URN(),
					Title:    apierr.ValidationFailed.Title,
					Status:   model.Status,
					Detail:   model.Detail,
					Instance: model.Instance,
					Errors:   model.Errors,
				},
				Code: apierr.ValidationFailed.Code,
			}
		}
		return &apierr.ErrorModel{ErrorModel: *model}
	}
}
