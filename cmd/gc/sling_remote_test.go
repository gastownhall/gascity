package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func remoteTestClient(t *testing.T, url string) *api.Client {
	t.Helper()
	c, err := api.NewRemoteCityScopedClient(url, "mc", api.RemoteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseSlingVars(t *testing.T) {
	m, err := parseSlingVars([]string{"a=1", "b=two=parts"})
	if err != nil {
		t.Fatal(err)
	}
	if m["a"] != "1" || m["b"] != "two=parts" {
		t.Errorf("parsed = %v", m)
	}
	if got, _ := parseSlingVars(nil); got != nil {
		t.Errorf("empty vars should be nil, got %v", got)
	}
	if _, err := parseSlingVars([]string{"=noKey"}); err == nil {
		t.Error("missing key must error")
	}
	if _, err := parseSlingVars([]string{"noEquals"}); err == nil {
		t.Error("missing '=' must error")
	}
}

// The remote path refuses modes that need local state, before touching the wire.
func TestCmdSlingRemote_RefusesUnsupportedModes(t *testing.T) {
	// A server that fails the test if it is ever contacted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server must not be contacted for a refused mode")
		w.WriteHeader(500)
	}))
	defer srv.Close()

	base := func() *api.Client { return remoteTestClient(t, srv.URL) }
	cases := []struct {
		name   string
		invoke func() int
		want   string
	}{
		{"stdin", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), []string{"mayor"}, false, false, false, "", nil, "", false, false, false, "", false, true /*stdin*/, false, "", "", false, &out, &errb)
		}, "stdin"},
		{"dry-run", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), []string{"mayor", "BL-1"}, false, false, false, "", nil, "", false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)
		}, "dry-run"},
		{"nudge", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), []string{"mayor", "BL-1"}, false, true /*nudge*/, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
		}, "not supported"},
		{"one-arg", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), []string{"BL-1"}, false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
		}, "explicit target"},
		{"inline-text", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), []string{"mayor", "write a readme"}, false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
		}, "inline text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code := tc.invoke(); code != 1 {
				t.Fatalf("expected exit 1, got %d", code)
			}
		})
	}
}

// Happy path: a 2-arg bead sling forwards to the server and renders the result.
func TestCmdSlingRemote_RoutesBead(t *testing.T) {
	var gotPath, gotReq, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReq = r.Header.Get("X-GC-Request")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"routed","target":"mayor","bead":"BL-42","warnings":["w1"]}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), []string{"mayor", "BL-42"},
		false, false, true /*force*/, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotPath != "/v0/city/mc/sling" || gotReq == "" {
		t.Errorf("path=%q req=%q", gotPath, gotReq)
	}
	if !strings.Contains(gotBody, `"target":"mayor"`) || !strings.Contains(gotBody, `"bead":"BL-42"`) || !strings.Contains(gotBody, `"force":true`) {
		t.Errorf("body=%q", gotBody)
	}
	if !strings.Contains(out.String(), "routed") || !strings.Contains(out.String(), "mayor") {
		t.Errorf("stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "w1") {
		t.Errorf("warning not surfaced: %q", errb.String())
	}
}

// --json emits a machine-readable object.
func TestCmdSlingRemote_JSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"launched","target":"mayor","formula":"review","workflow_id":"wf-9"}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), []string{"mayor", "review"},
		true /*formula*/, false, false, "", []string{"pr=42"}, "", false, false, false, "", false, false, false, "", "", true /*json*/, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["status"] != "launched" || got["formula"] != "review" || got["workflow_id"] != "wf-9" {
		t.Errorf("json = %v", got)
	}
	// Automation-critical fields align with the local `sling --json` shape.
	if got["schema_version"] != "1" || got["success"] != true {
		t.Errorf("json missing schema_version/success: %v", got)
	}
}
