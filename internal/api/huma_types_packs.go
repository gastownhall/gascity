package api

// Per-domain Huma input/output types for the packs handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_packs.go.

// --- Pack types ---

// PackListInput is the Huma input for GET /v0/city/{cityName}/packs.
type PackListInput struct {
	CityScope
}

