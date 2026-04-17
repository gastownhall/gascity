package api

// Per-domain Huma input/output types for the convoys handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_convoys.go.

// --- Convoy types ---

// ConvoyListInput is the Huma input for GET /v0/city/{cityName}/convoys.
type ConvoyListInput struct {
	CityScope
	BlockingParam
	PaginationParam
}

// ConvoyGetInput is the Huma input for GET /v0/city/{cityName}/convoy/{id}.
type ConvoyGetInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyCreateInput is the Huma input for POST /v0/city/{cityName}/convoys.
type ConvoyCreateInput struct {
	CityScope
	Body struct {
		Rig   string   `json:"rig,omitempty" doc:"Rig name."`
		Title string   `json:"title" doc:"Convoy title." minLength:"1"`
		Items []string `json:"items,omitempty" doc:"Bead IDs to include."`
	}
}

// ConvoyAddInput is the Huma input for POST /v0/city/{cityName}/convoy/{id}/add.
type ConvoyAddInput struct {
	CityScope
	ID   string `path:"id" doc:"Convoy ID."`
	Body struct {
		Items []string `json:"items,omitempty" doc:"Bead IDs to add."`
	}
}

// ConvoyRemoveInput is the Huma input for POST /v0/city/{cityName}/convoy/{id}/remove.
type ConvoyRemoveInput struct {
	CityScope
	ID   string `path:"id" doc:"Convoy ID."`
	Body struct {
		Items []string `json:"items,omitempty" doc:"Bead IDs to remove."`
	}
}

// ConvoyCheckInput is the Huma input for GET /v0/city/{cityName}/convoy/{id}/check.
type ConvoyCheckInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyCloseInput is the Huma input for POST /v0/city/{cityName}/convoy/{id}/close.
type ConvoyCloseInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

// ConvoyDeleteInput is the Huma input for DELETE /v0/city/{cityName}/convoy/{id}.
type ConvoyDeleteInput struct {
	CityScope
	ID string `path:"id" doc:"Convoy ID."`
}

