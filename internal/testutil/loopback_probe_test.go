package testutil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsForeignLoopbackProbe(t *testing.T) {
	for _, tc := range []struct {
		method, target string
		upgrade        string
		want           bool
	}{
		{"GET", "/", "", true},
		{"GET", "/", "websocket", false},
		{"GET", "/?x=1", "", false},
		{"HEAD", "/", "", false},
		{"POST", "/", "", false},
		{"GET", "/v0/city/mc-city/events", "", false},
		{"GET", "/health", "", false},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		if tc.upgrade != "" {
			r.Header.Set("Upgrade", tc.upgrade)
		}
		if got := IsForeignLoopbackProbe(r); got != tc.want {
			t.Errorf("%s %s upgrade=%q: IsForeignLoopbackProbe = %v, want %v", tc.method, tc.target, tc.upgrade, got, tc.want)
		}
	}
	if IsForeignLoopbackProbe(nil) {
		t.Error("nil request reads as a probe")
	}
}

func TestIgnoreForeignLoopbackProbesAnswersTheProbeAndPassesTheRest(t *testing.T) {
	var seen []string
	h := IgnoreForeignLoopbackProbes(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
	}))
	probe := httptest.NewRecorder()
	h.ServeHTTP(probe, httptest.NewRequest("GET", "/", nil))
	if probe.Code != http.StatusNotFound {
		t.Fatalf("probe status = %d, want 404", probe.Code)
	}
	upgrade := httptest.NewRequest("GET", "/", nil)
	upgrade.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(httptest.NewRecorder(), upgrade)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v0/cities", nil))
	if len(seen) != 2 || seen[0] != "/" || seen[1] != "/v0/cities" {
		t.Fatalf("handler saw %v, want the upgrade and the API request only", seen)
	}
}
