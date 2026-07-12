package api

import (
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

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

// readyPartialFromGen extracts the partial-aggregation signal from a federated
// ready-list response body. Returns (false, nil) when the body or its partial
// fields are absent, so a complete read is reported as authoritative.
func readyPartialFromGen(body *genclient.ListBodyBead) (bool, []string) {
	if body == nil {
		return false, nil
	}
	partial := body.Partial != nil && *body.Partial
	var errs []string
	if body.PartialErrors != nil && len(*body.PartialErrors) > 0 {
		errs = append(errs, *body.PartialErrors...)
	}
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
