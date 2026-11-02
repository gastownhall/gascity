package api

import (
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// BeadListView is GET /v0/city/{cityName}/beads after every page is drained:
// the beads, plus the server's report of each backend that failed while
// building them. A caller that prints Beads as the city's complete answer must
// refuse a Partial view.
type BeadListView struct {
	Beads         []beads.Bead
	Partial       bool
	PartialErrors []string
}

// beadsFromGenList translates a genclient list body into []beads.Bead.
// Returns an empty slice (never nil) when the body is missing or holds no
// items so callers can uniformly format the empty case. Reuses beadFromGen
// from decode_convoys.go for the per-item translation.
func beadsFromGenList(body *genclient.ListBodyBead) []beads.Bead {
	if body == nil || body.Items == nil {
		return []beads.Bead{}
	}
	items := *body.Items
	out := make([]beads.Bead, 0, len(items))
	for _, item := range items {
		out = append(out, beadFromGen(item))
	}
	return out
}

// beadListDegradationFromGen reads one page's report of failed backends. A
// page that lists errors is partial even when the server left the flag unset,
// so an error message can never be dropped on the way to the caller.
func beadListDegradationFromGen(body *genclient.ListBodyBead) (partial bool, errs []string) {
	if body == nil {
		return false, nil
	}
	if body.PartialErrors != nil {
		errs = *body.PartialErrors
	}
	partial = (body.Partial != nil && *body.Partial) || len(errs) > 0
	return partial, errs
}

// beadFromGenPtr translates a non-nil *genclient.Bead into a beads.Bead.
// Returns the zero beads.Bead when given a nil pointer; callers should check
// for an empty ID to detect the missing-body case.
func beadFromGenPtr(g *genclient.Bead) beads.Bead {
	if g == nil {
		return beads.Bead{}
	}
	return beadFromGen(*g)
}
