package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The CLI's one-shot status reads ask the server to build the body for the
// read (?fresh=true) and send none of the poller or long-poll parameters
// (ga-v1vyh8.17).
func TestGetStatusAsksForABodyBuiltForTheRead(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"c","path":"/c","uptime_sec":1,"suspended":false,"agent_count":0,"rig_count":0,"running":0,` +
			`"agents":{"total":0,"running":0,"suspended":0,"quarantined":0},"rigs":{"total":0,"suspended":0},` +
			`"work":{"in_progress":0,"ready":0,"open":0},"mail":{"unread":0,"total":0}}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := NewCityScopedClient(srv.URL, "c").GetStatus(); err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	want := url.Values{"fresh": {"true"}}
	if len(got) != len(want) || got.Get("fresh") != "true" {
		t.Fatalf("GetStatus query = %v, want %v", got, want)
	}
}
