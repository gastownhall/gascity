package apicontract

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const testSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/v0/city/{cityName}/bead/{id}": {
      "get": {"operationId": "get-bead", "responses": {"200": {"description": "ok",
        "content": {"application/json": {"schema": {"type": "object", "required": ["id"],
          "properties": {"id": {"type": "string"}}, "additionalProperties": false}}}}}}
    },
    "/v0/city/{cityName}/beads/ready": {
      "get": {"operationId": "ready-beads", "responses": {"200": {"description": "ok"}}}
    },
    "/v0/city/{cityName}/beads/{id}": {
      "get": {"operationId": "beads-by-id", "responses": {"200": {"description": "ok"}}},
      "post": {"operationId": "post-beads-by-id", "responses": {"202": {"description": "ok"}}}
    },
    "/v0/events/stream": {
      "get": {"operationId": "stream", "responses": {"200": {"description": "ok"}}}
    }
  }
}`

func loadTestSpec(t *testing.T) *Spec {
	t.Helper()
	s, err := Load([]byte(testSpec))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func TestMatchPrefersLiteralSegmentsAndFiltersMethod(t *testing.T) {
	s := loadTestSpec(t)
	cases := []struct {
		method, path, want string
		ok                 bool
	}{
		{http.MethodGet, "/v0/city/c/beads/ready", "ready-beads", true},
		{http.MethodGet, "/v0/city/c/beads/gc-1", "beads-by-id", true},
		{http.MethodPost, "/v0/city/c/beads/gc-1", "post-beads-by-id", true},
		{http.MethodGet, "/v0/city/c/bead/gc-1", "get-bead", true},
		{http.MethodDelete, "/v0/city/c/bead/gc-1", "", false},
		{http.MethodGet, "/v0/city/c/bead/gc-1/extra", "", false},
	}
	for _, tc := range cases {
		op, ok := s.Match(tc.method, tc.path)
		if ok != tc.ok || op.ID != tc.want {
			t.Errorf("Match(%s %s) = (%q, %v), want (%q, %v)", tc.method, tc.path, op.ID, ok, tc.want, tc.ok)
		}
	}
}

func TestInProcessTransportValidatesAndRecords(t *testing.T) {
	s := loadTestSpec(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/city/{cityName}/bead/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") == "bad" {
			_, _ = io.WriteString(w, `{"id":"bad","extra":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"`+r.PathValue("id")+`"}`)
	})
	tr := NewInProcessTransport(s, mux)
	client := &http.Client{Transport: tr}

	resp, err := client.Get("http://in-process/v0/city/c/bead/gc-1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != `{"id":"gc-1"}` {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if got := tr.Failures(); len(got) != 0 {
		t.Fatalf("unexpected failures: %v", got)
	}

	if _, err := client.Get("http://in-process/v0/city/c/bead/bad"); err != nil {
		t.Fatalf("GET bad: %v", err)
	}
	if _, err := client.Get("http://in-process/v0/nowhere"); err != nil {
		t.Fatalf("GET nowhere: %v", err)
	}
	failures := tr.Failures()
	if len(failures) != 2 {
		t.Fatalf("failures = %v, want a schema violation and an unmatched request", failures)
	}
	if !strings.Contains(failures[0].Error(), "[get-bead]") {
		t.Errorf("schema failure not attributed to its operation: %v", failures[0])
	}
	if !strings.Contains(failures[1].Error(), "matches no operation") {
		t.Errorf("unmatched failure = %v", failures[1])
	}
	if got := tr.Exercised()["get-bead"]; got != 2 {
		t.Errorf("get-bead exercised %d times, want 2", got)
	}
}

func TestInProcessTransportStreamsEventStream(t *testing.T) {
	s := loadTestSpec(t)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/events/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	tr := NewInProcessTransport(s, mux)
	resp, err := (&http.Client{Transport: tr}).Get("http://in-process/v0/events/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	buf := make([]byte, len("data: one\n\n"))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first frame before handler returned: %v", err)
	}
	if string(buf) != "data: one\n\n" {
		t.Fatalf("frame = %q", buf)
	}
	close(release)
	_ = resp.Body.Close()
	if tr.Exercised()["stream"] != 1 {
		t.Fatalf("stream not recorded: %v", tr.Exercised())
	}
}

func TestCheckCoverage(t *testing.T) {
	ops := []Operation{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	c := CheckCoverage(ops, []string{"a", "b", "zz"}, map[string]string{"b": "dup", "c": "reason", "gone": "old"})
	if c.Exercised != 2 || c.Waived != 1 || c.Total != 4 {
		t.Fatalf("counts = %+v", c)
	}
	if strings.Join(c.Missing, ",") != "d" ||
		strings.Join(c.StaleWaivers, ",") != "gone" ||
		strings.Join(c.WaivedButExercised, ",") != "b" ||
		strings.Join(c.Unknown, ",") != "zz" {
		t.Fatalf("coverage = %+v", c)
	}
	if c.Err() == nil {
		t.Fatal("expected guard failure")
	}
	if c.Percent() != 50 {
		t.Fatalf("percent = %v", c.Percent())
	}
	clean := CheckCoverage(ops, []string{"a", "b"}, map[string]string{"c": "r", "d": "r"})
	if err := clean.Err(); err != nil {
		t.Fatalf("clean coverage failed: %v", err)
	}
}

// Sibling templates that differ only by method must validate against the
// operation the method selects, not the first template the shape matches.
func TestValidateResponseDisambiguatesSiblingTemplatesByMethod(t *testing.T) {
	s, err := Load([]byte(`{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/agent/{base}/{action}": {"post": {"operationId": "act", "responses": {"200": {"description": "ok",
      "content": {"application/json": {"schema": {"type": "object", "required": ["status"],
        "properties": {"status": {"type": "string"}}}}}}}}},
    "/agent/{dir}/{base}": {"get": {"operationId": "get-qualified", "responses": {"200": {"description": "ok"}}}}
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://x/agent/helper/resume", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Request: req}
	if err := s.ValidateResponse(req, resp, []byte(`{"status":"ok"}`)); err != nil {
		t.Fatalf("valid POST response rejected: %v", err)
	}
	if err := s.ValidateResponse(req, resp, []byte(`{"nope":1}`)); err == nil {
		t.Fatal("response missing a required property was accepted")
	}
}

func TestViolationErrorCarriesOperationID(t *testing.T) {
	s := loadTestSpec(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/city/{cityName}/bead/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"extra":1}`)
	})
	tr := NewInProcessTransport(s, mux)
	if _, err := (&http.Client{Transport: tr}).Get("http://in-process/v0/city/c/bead/x"); err != nil {
		t.Fatal(err)
	}
	var v *ViolationError
	if f := tr.Failures(); len(f) != 1 || !errors.As(f[0], &v) || v.OperationID != "get-bead" {
		t.Fatalf("failures = %v, want one ViolationError for get-bead", f)
	}
}
