package api

import (
	"net/http/httptest"
	"testing"
)

// Malformed query parameters should be rejected with 400, not silently
// default to the zero value. Today `index=garbage` falls through to the
// handler because strconv.ParseUint's error is discarded.
func TestBlockingIndexRejectsGarbage(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads?index=garbage"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 400 && rec.Code != 422 {
		t.Errorf("status = %d, want 400/422 for malformed index (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestBlockingWaitRejectsGarbage(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/beads?wait=ninety"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 400 && rec.Code != 422 {
		t.Errorf("status = %d, want 400/422 for malformed wait (body=%q)", rec.Code, rec.Body.String())
	}
}
