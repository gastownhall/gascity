package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// Negative limit used to bypass Huma validation (PaginationParam.Limit
// had no minimum: tag) and panic in paginate() when end < offset.
// R2-1: Limit gains minimum:"0", so malformed input returns 422.
func TestPaginationLimitRejectsNegative(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads?limit=-1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("status = %d, want 4xx for malformed limit (body=%q)", rec.Code, rec.Body.String())
	}
}

// Guard the happy path so the minimum: tag doesn't accidentally break
// valid requests.
func TestPaginationLimitZeroAccepted(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads?limit=0"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200 for limit=0 (body=%q)", rec.Code, rec.Body.String())
	}
	var body struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
