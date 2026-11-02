package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// Client.Sling posts the mutation to /v0/city/{city}/sling, carrying the CSRF
// header and — when a grant source is configured — a request-bound
// X-GC-City-Write grant, and maps the JSON200 body into a SlingResult.
func TestClientSling_PostsAndParses(t *testing.T) {
	var gotPath, gotReqHdr, gotGrant, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReqHdr = r.Header.Get("X-GC-Request")
		gotGrant = r.Header.Get("X-GC-City-Write")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"routed","target":"mayor","bead":"BL-42","workflow_id":"wf-1","warnings":["heads up"]}`))
	}))
	defer srv.Close()

	// A grant source that binds the request the transport computed.
	var boundDigest string
	c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{
		Grant: func(b GrantBinding) (string, error) {
			boundDigest = b.ReqDigest
			return "payload.sig", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.Sling(SlingRequest{Target: "mayor", Bead: "BL-42", Force: true})
	if err != nil {
		t.Fatalf("Sling: %v", err)
	}
	if gotPath != "/v0/city/mc/sling" {
		t.Errorf("path = %q", gotPath)
	}
	if gotReqHdr == "" {
		t.Error("X-GC-Request (CSRF) header must be present")
	}
	if gotGrant != "payload.sig" {
		t.Errorf("grant header = %q", gotGrant)
	}
	// The grant is bound to the exact wire request the server received.
	wantDigest := citywriteauth.ReqDigest(http.MethodPost, "/v0/city/mc/sling", "", []byte(gotBody))
	if boundDigest != wantDigest {
		t.Errorf("grant digest %q != server-recomputed %q", boundDigest, wantDigest)
	}
	if !strings.Contains(gotBody, `"target":"mayor"`) || !strings.Contains(gotBody, `"bead":"BL-42"`) || !strings.Contains(gotBody, `"force":true`) {
		t.Errorf("request body = %q", gotBody)
	}
	if res.Status != "routed" || res.Target != "mayor" || res.Bead != "BL-42" || res.WorkflowID != "wf-1" {
		t.Errorf("result = %+v", res)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "heads up" {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

// A read-only city (403) surfaces as an error, and — because this is a remote
// client — is never fallback-eligible (gate G1).
func TestClientSling_ErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"detail":"read only"}`))
	}))
	defer srv.Close()

	c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sling(SlingRequest{Target: "mayor", Bead: "BL-1"}); err == nil {
		t.Fatal("a 403 must surface as an error")
	} else if ShouldFallback(c, err) {
		t.Error("a remote sling error must never be fallback-eligible (gate G1)")
	}
}

// Vars and formula mode marshal into the request body.
func TestClientSling_FormulaAndVars(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"launched","target":"mayor","formula":"review"}`))
	}))
	defer srv.Close()

	c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sling(SlingRequest{Target: "mayor", Formula: "review", Vars: map[string]string{"pr": "42"}}); err != nil {
		t.Fatal(err)
	}
	if gotBody["formula"] != "review" {
		t.Errorf("formula not sent: %v", gotBody["formula"])
	}
	vars, _ := gotBody["vars"].(map[string]any)
	if vars["pr"] != "42" {
		t.Errorf("vars not sent: %v", gotBody["vars"])
	}
}

// Client.CreateBead posts to /v0/city/{city}/beads with the CSRF header and a
// request-bound X-GC-City-Write grant, sends the rig that selects the store,
// and maps the 201 body into a bead.
func TestClientCreateBead_PostsAndParses(t *testing.T) {
	var gotPath, gotReqHdr, gotGrant, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReqHdr = r.Header.Get("X-GC-Request")
		gotGrant = r.Header.Get("X-GC-City-Write")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"rg-abc1","title":"write a readme","status":"open","type":"task","created_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	var boundDigest string
	c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{
		Grant: func(b GrantBinding) (string, error) {
			boundDigest = b.ReqDigest
			return "payload.sig", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.CreateBead(CreateBeadRequest{Rig: "rg", Title: "write a readme", Description: "details", Type: "task"})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if gotPath != "/v0/city/mc/beads" {
		t.Errorf("path = %q", gotPath)
	}
	if gotReqHdr == "" {
		t.Error("X-GC-Request (CSRF) header must be present")
	}
	if gotGrant != "payload.sig" {
		t.Errorf("grant header = %q", gotGrant)
	}
	if want := citywriteauth.ReqDigest(http.MethodPost, "/v0/city/mc/beads", "", []byte(gotBody)); boundDigest != want {
		t.Errorf("grant digest %q != server-recomputed %q", boundDigest, want)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("request body %q: %v", gotBody, err)
	}
	if body["rig"] != "rg" || body["title"] != "write a readme" || body["description"] != "details" || body["type"] != "task" {
		t.Errorf("request body = %q", gotBody)
	}
	if got.ID != "rg-abc1" || got.Title != "write a readme" {
		t.Errorf("bead = %+v", got)
	}
}

// CreateBead omits empty optional fields, so a city-scoped create sends no rig.
func TestClientCreateBead_OmitsEmptyRig(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"gc-1","title":"t","status":"open","type":"task","created_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateBead(CreateBeadRequest{Title: "t"}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if strings.Contains(gotBody, `"rig"`) || strings.Contains(gotBody, `"description"`) {
		t.Errorf("empty optional fields must be omitted: %q", gotBody)
	}
}

// A refused create surfaces the server's problem detail as an error.
func TestClientCreateBead_ErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":400,"detail":"unknown rig \"nope\""}`))
	}))
	defer srv.Close()
	c, err := NewRemoteCityScopedClient(srv.URL, "mc", RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateBead(CreateBeadRequest{Rig: "nope", Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "unknown rig") {
		t.Fatalf("err = %v, want the server detail", err)
	}
}
