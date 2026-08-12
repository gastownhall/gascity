package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
)

// The huma.NewError override is the load-bearing seam for the error contract:
// it must (a) stamp huma's built-in request-validation 422 with the
// validation-failed problem type and (b) leave every other error byte-identical
// on the wire so unconverted call sites keep their exact shape. These tests lock
// both halves.

// TestNewErrorOverride_ValidationFailedStamped verifies huma's internal 422
// ("validation failed") is re-typed as the validation-failed problem type.
func TestNewErrorOverride_ValidationFailedStamped(t *testing.T) {
	err := huma.NewError(http.StatusUnprocessableEntity, "validation failed",
		&huma.ErrorDetail{Message: "expected required property title to be present", Location: "body.title"})
	em, ok := err.(*apierr.ErrorModel)
	if !ok {
		t.Fatalf("override must return *apierr.ErrorModel, got %T", err)
	}
	if em.Type != "urn:gascity:error:validation-failed" || em.Code != "validation-failed" {
		t.Fatalf("validation 422 type/code = %q/%q, want validation-failed", em.Type, em.Code)
	}
	if em.Title != "Validation Failed" {
		t.Fatalf("validation 422 title = %q, want %q", em.Title, "Validation Failed")
	}
	if em.Status != http.StatusUnprocessableEntity || em.Detail != "validation failed" {
		t.Fatalf("validation 422 status/detail = %d/%q", em.Status, em.Detail)
	}
	if len(em.Errors) != 1 || em.Errors[0].Location != "body.title" {
		t.Fatalf("validation 422 must preserve huma's field errors, got %+v", em.Errors)
	}
}

// TestNewErrorOverride_LegacyIsByteIdentical verifies every non-validation error
// is wrapped as *apierr.ErrorModel with an empty (omitted) code, marshaling
// byte-for-byte the same as huma's default ErrorModel. This is the wire-compat
// guarantee for the ~376 unconverted call sites.
func TestNewErrorOverride_LegacyIsByteIdentical(t *testing.T) {
	cases := []struct {
		status int
		msg    string
	}{
		{http.StatusInternalServerError, "boom"},
		{http.StatusServiceUnavailable, "no bead store configured"},
		{http.StatusNotFound, "bead bd-9 not found"},
		{http.StatusConflict, "conflict: bead bd-9 was deleted concurrently"},
		{http.StatusBadRequest, "rig is required when multiple rigs are configured"},
		// A hand-written 422 whose message is NOT huma's marker must stay legacy.
		{http.StatusUnprocessableEntity, "at least one of 'title' or 'alias' is required"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status)+"/"+tc.msg, func(t *testing.T) {
			got := huma.NewError(tc.status, tc.msg)
			if _, ok := got.(*apierr.ErrorModel); !ok {
				t.Fatalf("override must return *apierr.ErrorModel, got %T", got)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal override: %v", err)
			}
			if strings.Contains(string(gotJSON), `"code"`) {
				t.Fatalf("legacy error must omit code, got %s", gotJSON)
			}
			// huma's exact default construction for this (status, msg, no errs).
			want, err := json.Marshal(&huma.ErrorModel{
				Status: tc.status,
				Title:  http.StatusText(tc.status),
				Detail: tc.msg,
			})
			if err != nil {
				t.Fatalf("marshal want: %v", err)
			}
			if string(gotJSON) != string(want) {
				t.Fatalf("legacy wire not byte-identical:\n got=%s\nwant=%s", gotJSON, want)
			}
		})
	}
}

// TestNewErrorOverride_EndToEndValidation drives a real request through the
// supervisor mux so the override is exercised on the actual serving path
// (through NewErrorWithContext, which delegates to the NewError var). A negative
// limit fails huma's built-in query validation with a 422.
func TestNewErrorOverride_EndToEndValidation(t *testing.T) {
	sm := NewSupervisorMux(emptyRoundtripResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/v0/city/anycity/beads?limit=-1", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("GET beads?limit=-1 returned %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type = %q, want application/problem+json", ct)
	}
	var body apierr.ErrorModel
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal problem body: %v (%s)", err, rec.Body.String())
	}
	if body.Type != "urn:gascity:error:validation-failed" || body.Code != "validation-failed" {
		t.Fatalf("end-to-end validation type/code = %q/%q, want validation-failed", body.Type, body.Code)
	}
}

// TestNewErrorOverride_ValidationFailedAtNon422Status locks the fix for the case
// where Huma emits its "validation failed" marker at a status other than 422 — a
// 400 for a body it cannot parse. The override must stamp validation-failed there
// too (preserving the 400), or a client branching on type/code would mis-classify
// every malformed-body request.
func TestNewErrorOverride_ValidationFailedAtNon422Status(t *testing.T) {
	sm := NewSupervisorMux(emptyRoundtripResolver{}, nil, false, "", "", time.Time{})
	// A truncated JSON body fails Huma's body parse with status 400, detail
	// "validation failed". POST needs the anti-CSRF header to reach validation.
	req := httptest.NewRequest(http.MethodPost, "/v0/city/anycity/beads", strings.NewReader(`{"title":`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "1")
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body returned %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var body apierr.ErrorModel
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal problem body: %v (%s)", err, rec.Body.String())
	}
	if body.Type != "urn:gascity:error:validation-failed" || body.Code != "validation-failed" {
		t.Fatalf("400 validation type/code = %q/%q, want validation-failed", body.Type, body.Code)
	}
	if body.Status != http.StatusBadRequest {
		t.Fatalf("validation-failed body Status = %d, want 400 (Huma's status must be preserved)", body.Status)
	}
}

// emptyRoundtripResolver is a CityResolver with no cities; huma validation runs
// before city resolution, so a validation 422 never needs a live city.
type emptyRoundtripResolver struct{}

func (emptyRoundtripResolver) ListCities() []CityInfo   { return nil }
func (emptyRoundtripResolver) CityState(_ string) State { return nil }
