package api

import "github.com/danielgtaylor/huma/v2"

const (
	slingMissingBeadProblemType = "urn:gascity:error:sling-missing-bead"
	slingCrossRigProblemType    = "urn:gascity:error:sling-cross-rig"
)

var documentedProblemTypes = []string{
	slingMissingBeadProblemType,
	slingCrossRigProblemType,
}

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
	for _, problemType := range documentedProblemTypes {
		if !hasProblemTypeExample(typeSchema.Examples, problemType) {
			typeSchema.Examples = append(typeSchema.Examples, problemType)
		}
	}
	if typeSchema.Extensions == nil {
		typeSchema.Extensions = map[string]any{}
	}
	typeSchema.Extensions["x-gascity-problem-types"] = append([]string(nil), documentedProblemTypes...)
}

func hasProblemTypeExample(examples []any, problemType string) bool {
	for _, example := range examples {
		if s, ok := example.(string); ok && s == problemType {
			return true
		}
	}
	return false
}
