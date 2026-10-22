package api

import "github.com/gastownhall/gascity/internal/readywork"

// WorkReadyInput is the Huma input for GET /v0/city/{cityName}/work/ready: the
// `gc ready` flags, as query parameters with the same meaning.
type WorkReadyInput struct {
	CityScope
	Assignee      string   `query:"assignee" required:"false" doc:"Only work whose assignee is exactly this identity."`
	Unassigned    bool     `query:"unassigned" required:"false" doc:"Only work with no assignee."`
	MetadataField []string `query:"metadata_field,explode" required:"false" doc:"Require metadata key=value, or a bare key for any non-empty value. Repeatable; every one must hold."`
	ExcludeType   []string `query:"exclude_type,explode" required:"false" doc:"Drop beads of this issue type. Repeatable."`
	ExcludeLabel  []string `query:"exclude_label,explode" required:"false" doc:"Drop beads carrying this label. Repeatable."`
	Sort          string   `query:"sort" required:"false" enum:"oldest,newest" doc:"Order: oldest or newest by created_at. Omitted is the canonical ready order (priority, created_at, id)."`
	Limit         int      `query:"limit" required:"false" minimum:"0" doc:"At most this many rows, cut after the merge and the order; 0 is unlimited."`
	Status        string   `query:"status" required:"false" enum:"open,in_progress,blocked,closed" doc:"List beads in this status instead of ready work."`
}

// readyWorkQuery is the input as the reader's query.
func (in *WorkReadyInput) readyWorkQuery() readywork.Query {
	return readywork.Query{
		Assignee:       in.Assignee,
		Unassigned:     in.Unassigned,
		MetadataFields: in.MetadataField,
		ExcludeTypes:   in.ExcludeType,
		ExcludeLabels:  in.ExcludeLabel,
		Sort:           in.Sort,
		Limit:          in.Limit,
		Status:         in.Status,
	}
}

// WorkReadyOutput is the response of GET /v0/city/{cityName}/work/ready.
type WorkReadyOutput struct {
	Body WorkReadyBody
}

// WorkReadyBody carries the answer: the rows `gc ready` prints, in its order.
type WorkReadyBody struct {
	Items []readywork.ReadyBead `json:"items" doc:"The ready work, in the query's order, cut to its limit: exactly the rows gc ready prints."`
}
