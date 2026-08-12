package api

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// documentProblemTypes annotates the generated OpenAPI ErrorModel schema with
// the catalog of machine-readable problem-type URNs the API can return. It
// generates the `x-gascity-problem-types` extension and the `type` examples
// directly from the apierr registry (apierr.Registered()), so the published
// contract stays in lockstep with the codes the server actually mints — adding
// a catalog entry surfaces in the spec with no edit here.
func documentProblemTypes(oapi *huma.OpenAPI) {
	if oapi == nil || oapi.Components == nil || oapi.Components.Schemas == nil {
		return
	}
	errorModel := oapi.Components.Schemas.Map()["ErrorModel"]
	if errorModel == nil || errorModel.Properties == nil {
		return
	}
	typeSchema := errorModel.Properties["type"]
	if typeSchema == nil {
		return
	}

	urns := make([]string, 0, len(apierr.Registered()))
	for _, pt := range apierr.Registered() {
		urns = append(urns, pt.URN())
	}

	for _, urn := range urns {
		if !hasProblemTypeExample(typeSchema.Examples, urn) {
			typeSchema.Examples = append(typeSchema.Examples, urn)
		}
	}
	if typeSchema.Extensions == nil {
		typeSchema.Extensions = map[string]any{}
	}
	typeSchema.Extensions["x-gascity-problem-types"] = urns
}

func hasProblemTypeExample(examples []any, problemType string) bool {
	for _, example := range examples {
		if s, ok := example.(string); ok && s == problemType {
			return true
		}
	}
	return false
}
