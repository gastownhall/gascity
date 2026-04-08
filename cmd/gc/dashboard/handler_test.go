package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConvoyHandlerTimeoutUsesSnapshot(t *testing.T) {
	slowServiceStarted := make(chan struct{})
	releaseSlowService := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v0/status":
			_, _ = w.Write([]byte(`{"name":"test-city","path":"/tmp/city","agent_count":0,"rig_count":0,"running":0}`))
		case "/v0/services":
			close(slowServiceStarted)
			<-releaseSlowService
			_, _ = w.Write([]byte(`{"items":[{"service_name":"healthz","kind":"workflow","mount_path":"/svc/healthz","publish_mode":"direct","public_url":"http://127.0.0.1:9443/svc/healthz","service_state":"ready","local_state":"ready","publication_state":"direct"}],"total":1}`))
		default:
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
		}
	}))
	defer srv.Close()

	handler, err := NewConvoyHandler(
		NewAPIFetcher(srv.URL, "/tmp/city", "test-city"),
		false,
		"",
		"",
		5*time.Millisecond,
		"csrf-token",
	)
	if err != nil {
		t.Fatalf("NewConvoyHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Workspace services fetch timed out") {
		t.Fatalf("response should render timeout snapshot without late service data:\n%s", body)
	}
	if !strings.Contains(body, "<span class=\"count\">n/a</span>") {
		t.Fatalf("response missing timeout count badge:\n%s", body)
	}

	select {
	case <-slowServiceStarted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timed out waiting for slow services fetch to start")
	}
	close(releaseSlowService)
}

func TestConvoyHandlerServiceErrorRendersFetchFailedState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/services":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/v0/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"test-city","path":"/tmp/city","agent_count":0,"rig_count":0,"running":0}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
		}
	}))
	defer srv.Close()

	handler, err := NewConvoyHandler(
		NewAPIFetcher(srv.URL, "/tmp/city", "test-city"),
		false,
		"",
		"",
		250*time.Millisecond,
		"csrf-token",
	)
	if err != nil {
		t.Fatalf("NewConvoyHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Workspace services fetch failed") {
		t.Fatalf("response missing fetch-failed state:\n%s", body)
	}
	if !strings.Contains(body, "<span class=\"count\">n/a</span>") {
		t.Fatalf("response missing fetch-failed count badge:\n%s", body)
	}
}
