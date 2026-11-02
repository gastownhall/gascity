package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// partialListError reports a list answer the server marked partial: one or
// more of the stores it aggregates failed (partial_errors names them), so its
// items are not the whole list. It is not a fallback condition
// (ShouldFallbackForRead leaves it alone): a routed read fails with it, the way
// a store read that fails fails the local route.
type partialListError struct {
	errs []string
}

// Error names every store failure the server reported.
func (e *partialListError) Error() string {
	if len(e.errs) == 0 {
		return "partial result: a store the server reads failed"
	}
	return "partial result: " + strings.Join(e.errs, "; ")
}

// partialListErrorOf returns a *partialListError when body is marked partial
// (partial=true or any partial_errors), and nil otherwise.
func partialListErrorOf(body *genclient.ListBodyBead) error {
	if body == nil {
		return nil
	}
	var errs []string
	if body.PartialErrors != nil {
		errs = append(errs, *body.PartialErrors...)
	}
	if (body.Partial == nil || !*body.Partial) && len(errs) == 0 {
		return nil
	}
	return &partialListError{errs: errs}
}
