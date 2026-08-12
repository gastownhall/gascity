package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/session"
)

// waitProblemBody picks the enumerated problem body matching status from the
// generated per-status response fields. The waits ops joined the P12 closed
// error contract (enumerated errorStatuses), so the catch-all
// ApplicationproblemJSONDefault response no longer exists on their generated
// response types; 422/500 are Huma's own additions.
func waitProblemBody(status int, p404, p422, p500, p503 *genclient.ErrorModel) *genclient.ErrorModel {
	switch status {
	case http.StatusNotFound:
		return p404
	case http.StatusUnprocessableEntity:
		return p422
	case http.StatusInternalServerError:
		return p500
	case http.StatusServiceUnavailable:
		return p503
	}
	return nil
}

// client_waits.go is the wire-serialization EDGE for durable waits: it decodes
// the generated WaitView wire type into the typed session.WaitInfo at the client
// boundary (the typed rung), and — for the deprecation window — projects raw
// beads off the generic /beads + /bead endpoints into session.WaitInfo (the
// legacy rungs, ListWaitsViaBeads / GetWaitViaBead). Because the WaitInfoFromBead
// codec is CALLED here, this file is listed in typedClassCodecEdgeFiles so the
// Tier-1 census keeps the interior at zero.
//
// DEPRECATION: the legacy legs and this file's census-edge entry are removed
// when the /v0/waits rolling-deploy window closes (tracked follow-up).

// WaitList is the client-edge decode of the /v0/waits list body.
type WaitList struct {
	Waits         []session.WaitInfo
	Capped        bool
	Partial       bool
	PartialErrors []string
}

// routeMissingError marks a 404 that carried no problem+json body — the shape an
// OLD server returns for an unknown /v0/... route (the SPA catch-all or the bare
// mux http.NotFound). A domain 404 from a registered Huma route always carries a
// problem+json body, so this cleanly separates "route not deployed yet" from
// "resource not found".
type routeMissingError struct{ path string }

func (e *routeMissingError) Error() string { return fmt.Sprintf("route missing: %s", e.path) }

// IsRouteMissing reports whether err is a routeMissingError — an old server that
// predates the requested route. The CLI uses this to fall back to a legacy leg.
func IsRouteMissing(err error) bool {
	var rm *routeMissingError
	return errors.As(err, &rm)
}

// NotAWaitError reports that the referenced bead exists but is not a durable
// wait (the server's not_a_wait: 404 detail, or the legacy leg's IsWaitBead
// rejection). The CLI renders "gc wait inspect: %s is not a wait" from it.
type NotAWaitError struct{ ID string }

// Error reports the referenced bead ID that is not a durable wait.
func (e *NotAWaitError) Error() string { return fmt.Sprintf("%s is not a wait", e.ID) }

// routeMissingFromResponse returns a routeMissingError when a 404 carried no
// problem+json body (an old server's catch-all), else nil.
func routeMissingFromResponse(status int, pd *genclient.ErrorModel, path string) error {
	if status == http.StatusNotFound && pd == nil {
		return &routeMissingError{path: path}
	}
	return nil
}

// ListWaits fetches durable waits via GET /v0/city/{cityName}/waits, decoding
// the typed WaitView projection into session.WaitInfo. On an old server that
// lacks the route it returns a routeMissingError so the CLI can fall back to the
// legacy generic-beads leg.
func (c *Client) ListWaits(state, sessionID string) (CachedRead[WaitList], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[WaitList]{}, err
	}
	params := &genclient.GetV0CityByCityNameWaitsParams{}
	if state != "" {
		params.State = &state
	}
	if sessionID != "" {
		params.Session = &sessionID
	}
	resp, err := c.cw.GetV0CityByCityNameWaitsWithResponse(context.Background(), c.cityName, params)
	if err != nil {
		return CachedRead[WaitList]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[WaitList]{}, &connError{err: fmt.Errorf("nil response")}
	}
	problem := waitProblemBody(resp.StatusCode(), resp.ApplicationproblemJSON404, resp.ApplicationproblemJSON422, resp.ApplicationproblemJSON500, resp.ApplicationproblemJSON503)
	if rmErr := routeMissingFromResponse(resp.StatusCode(), problem, "/waits"); rmErr != nil {
		return CachedRead[WaitList]{}, rmErr
	}
	if err := apiErrorFromResponse(resp.StatusCode(), problem); err != nil {
		return CachedRead[WaitList]{}, err
	}
	return CachedRead[WaitList]{
		Body:       waitListFromGen(resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// GetWait fetches one durable wait via GET /v0/city/{cityName}/wait/{id}. A
// route-missing 404 yields a routeMissingError; a not_a_wait: domain 404 yields
// a NotAWaitError; a plain not_found 404 flows through apiErrorFromResponse.
func (c *Client) GetWait(id string) (CachedRead[session.WaitInfo], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[session.WaitInfo]{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameWaitByIdWithResponse(context.Background(), c.cityName, id)
	if err != nil {
		return CachedRead[session.WaitInfo]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[session.WaitInfo]{}, &connError{err: fmt.Errorf("nil response")}
	}
	problem := waitProblemBody(resp.StatusCode(), resp.ApplicationproblemJSON404, resp.ApplicationproblemJSON422, resp.ApplicationproblemJSON500, resp.ApplicationproblemJSON503)
	if rmErr := routeMissingFromResponse(resp.StatusCode(), problem, "/wait/"+id); rmErr != nil {
		return CachedRead[session.WaitInfo]{}, rmErr
	}
	if resp.StatusCode() == http.StatusNotFound && problem != nil {
		detail := ""
		if problem.Detail != nil {
			detail = *problem.Detail
		}
		if strings.HasPrefix(detail, "not_a_wait:") {
			return CachedRead[session.WaitInfo]{}, &NotAWaitError{ID: id}
		}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), problem); err != nil {
		return CachedRead[session.WaitInfo]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[session.WaitInfo]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[session.WaitInfo]{
		Body:       waitInfoFromGen(*resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// ListWaitsViaBeads is the deprecation-window legacy leg: it reads the generic
// gc:wait label endpoint and applies the closed-exclusion + IsWaitBead filter +
// WaitInfoFromBead projection inside internal/api (serialization at the edge),
// returning the same typed shape as ListWaits. The server never changes here —
// the generic /beads endpoint keeps serving the label read indefinitely.
func (c *Client) ListWaitsViaBeads() (CachedRead[WaitList], error) {
	cr, err := c.ListBeads(ListBeadsOpts{Label: session.WaitBeadLabel, Limit: 1000})
	if err != nil {
		return CachedRead[WaitList]{}, err
	}
	waits := make([]session.WaitInfo, 0, len(cr.Body))
	for _, b := range cr.Body {
		if b.Status == "closed" {
			continue
		}
		if !session.IsWaitBead(b) {
			continue
		}
		waits = append(waits, session.WaitInfoFromBead(b))
	}
	return CachedRead[WaitList]{Body: WaitList{Waits: waits}, AgeSeconds: cr.AgeSeconds}, nil
}

// GetWaitViaBead is the deprecation-window legacy leg for GetWait over the
// generic /bead/{id} endpoint, applying the IsWaitBead guard + WaitInfoFromBead
// projection at the client edge.
func (c *Client) GetWaitViaBead(id string) (CachedRead[session.WaitInfo], error) {
	cr, err := c.GetBead(id)
	if err != nil {
		return CachedRead[session.WaitInfo]{}, err
	}
	if !session.IsWaitBead(cr.Body) {
		return CachedRead[session.WaitInfo]{}, &NotAWaitError{ID: id}
	}
	return CachedRead[session.WaitInfo]{Body: session.WaitInfoFromBead(cr.Body), AgeSeconds: cr.AgeSeconds}, nil
}

// waitListFromGen decodes the generated list body into the typed WaitList.
func waitListFromGen(body *genclient.WaitListBody) WaitList {
	if body == nil {
		return WaitList{Waits: []session.WaitInfo{}}
	}
	out := WaitList{Capped: body.Capped, Waits: []session.WaitInfo{}}
	if body.Partial != nil {
		out.Partial = *body.Partial
	}
	if body.PartialErrors != nil {
		out.PartialErrors = append([]string(nil), *body.PartialErrors...)
	}
	if body.Waits != nil {
		for _, v := range *body.Waits {
			out.Waits = append(out.Waits, waitInfoFromGen(v))
		}
	}
	return out
}

// waitInfoFromGen decodes a generated WaitView into session.WaitInfo. Optional
// fields are dereferenced (nil -> zero), preserving the nil-vs-empty DepIDs and
// zero-CreatedAt distinctions the CLI renders.
func waitInfoFromGen(g genclient.WaitView) session.WaitInfo {
	w := session.WaitInfo{
		ID:        g.Id,
		SessionID: g.SessionId,
		Kind:      g.Kind,
		State:     g.State,
		Status:    g.Status,
	}
	if g.SessionName != nil {
		w.SessionName = *g.SessionName
	}
	if g.DepIds != nil {
		w.DepIDs = append([]string(nil), *g.DepIds...)
	}
	if g.DepMode != nil {
		w.DepMode = *g.DepMode
	}
	if g.RegisteredEpoch != nil {
		w.RegisteredEpoch = *g.RegisteredEpoch
	}
	if g.DeliveryAttempt != nil {
		w.DeliveryAttempt = *g.DeliveryAttempt
	}
	if g.NudgeId != nil {
		w.NudgeID = *g.NudgeId
	}
	if g.ExpiresAt != nil {
		w.ExpiresAt = *g.ExpiresAt
	}
	if g.Note != nil {
		w.Note = *g.Note
	}
	if g.Labels != nil {
		w.Labels = append([]string(nil), *g.Labels...)
	}
	if g.CreatedAt != nil && *g.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, *g.CreatedAt); err == nil {
			w.CreatedAt = t
		}
	}
	return w
}
