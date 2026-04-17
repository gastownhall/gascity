package api

// Per-domain Huma input/output types for the orders handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_orders.go.

// --- Order types ---

// OrdersFeedInput is the Huma input for GET /v0/city/{cityName}/orders/feed.
type OrdersFeedInput struct {
	CityScope
	ScopeKind string `query:"scope_kind" required:"false" doc:"Scope kind (city or rig)."`
	ScopeRef  string `query:"scope_ref" required:"false" doc:"Scope reference."`
	Limit     string `query:"limit" required:"false" doc:"Maximum number of feed items to return."`
}

// OrderListInput is the Huma input for GET /v0/city/{cityName}/orders.
type OrderListInput struct {
	CityScope
}

// OrderGetInput is the Huma input for GET /v0/city/{cityName}/order/{name}.
type OrderGetInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name."`
}

// OrderCheckInput is the Huma input for GET /v0/city/{cityName}/orders/check.
type OrderCheckInput struct {
	CityScope
}

// OrderHistoryInput is the Huma input for GET /v0/city/{cityName}/orders/history.
type OrderHistoryInput struct {
	CityScope
	ScopedName string `query:"scoped_name" required:"false" doc:"Scoped order name."`
	Limit      string `query:"limit" required:"false" doc:"Maximum number of history entries."`
	Before     string `query:"before" required:"false" doc:"Return entries before this RFC3339 timestamp."`
}

// OrderHistoryDetailInput is the Huma input for GET /v0/city/{cityName}/order/history/{bead_id}.
type OrderHistoryDetailInput struct {
	CityScope
	BeadID string `path:"bead_id" doc:"Bead ID for the order run."`
}

// OrderEnableInput is the Huma input for POST /v0/city/{cityName}/order/{name}/enable.
type OrderEnableInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name."`
}

// OrderDisableInput is the Huma input for POST /v0/city/{cityName}/order/{name}/disable.
type OrderDisableInput struct {
	CityScope
	Name string `path:"name" doc:"Order name or scoped name."`
}

