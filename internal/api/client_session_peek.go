package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/session"
)

// maxSessionPeekLines is the wire maximum of SessionGetInput.PeekLines.
const maxSessionPeekLines = 10000

// sessionNotFoundDetailPrefix is the code token the server puts before a
// session-not-found message (humaResolveError, humaStoreError).
const sessionNotFoundDetailPrefix = "not_found: "

// sessionRefusal is a session-domain refusal the server answered for a
// strict peek. Error is the domain's own message, so the CLI prints what a
// local peek prints; Unwrap is the domain sentinel, so errors.Is works.
type sessionRefusal struct {
	reason error
	msg    string
}

func (e *sessionRefusal) Error() string { return e.msg }
func (e *sessionRefusal) Unwrap() error { return e.reason }

// PeekSession reads session target's output with gc session peek's own
// contract via GET /v0/city/{cityName}/session/{id}?peek_strict=true: the
// server peeks every open session that is not suspended and passes lines to
// the runtime (0 asks for its full capture; lines is clamped to the wire's
// [0, 10000]). A suspended or closed session returns an error that unwraps to
// session.ErrSessionInactive, and a target the live-session resolver cannot
// find one that unwraps to session.ErrSessionNotFound; both read as the local
// path's message. Other failures are the usual API errors.
func (c *Client) PeekSession(target string, lines int) (CachedRead[SessionView], error) {
	if err := c.requireCityScope(); err != nil {
		return CachedRead[SessionView]{}, err
	}
	peek := true
	peekLines := int64(min(max(lines, 0), maxSessionPeekLines))
	params := &genclient.GetV0CityByCityNameSessionByIdParams{
		Peek:       &peek,
		PeekStrict: &peek,
		PeekLines:  &peekLines,
	}
	resp, err := c.cw.GetV0CityByCityNameSessionByIdWithResponse(context.Background(), c.cityName, target, params)
	if err != nil {
		return CachedRead[SessionView]{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return CachedRead[SessionView]{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := sessionPeekProblem(resp.StatusCode(), pdOf(resp)); err != nil {
		return CachedRead[SessionView]{}, err
	}
	if resp.JSON200 == nil {
		return CachedRead[SessionView]{}, fmt.Errorf("API returned %d with no body", resp.StatusCode())
	}
	return CachedRead[SessionView]{
		Body:       sessionViewFromGen(*resp.JSON200),
		AgeSeconds: cacheAgeFromResponse(resp.HTTPResponse),
	}, nil
}

// sessionPeekProblem maps a strict peek's registered session refusals to
// their domain sentinels, keyed on the problem code, and leaves every other
// answer to apiErrorFromResponse.
func sessionPeekProblem(status int, pd *genclient.ErrorModel) error {
	if pd != nil && pd.Code != nil && pd.Detail != nil {
		switch {
		case status == http.StatusConflict && *pd.Code == apierr.SessionInactive.Code:
			return &sessionRefusal{reason: session.ErrSessionInactive, msg: *pd.Detail}
		case status == http.StatusNotFound && *pd.Code == apierr.SessionNotFound.Code:
			return &sessionRefusal{reason: session.ErrSessionNotFound, msg: strings.TrimPrefix(*pd.Detail, sessionNotFoundDetailPrefix)}
		}
	}
	return apiErrorFromResponse(status, pd)
}
