package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

func remoteTestTarget(url string) *remoteTarget {
	return &remoteTarget{BaseURL: url, CityName: "mc", Source: remoteSourceURLFlag}
}

// newRemoteTestServer serves h on a loopback server that is closed when the
// test ends.
func newRemoteTestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// newCannedSlingServer serves resp as the JSON answer to every request and
// records the last request body. It is closed when the test ends.
func newCannedSlingServer(t *testing.T, resp string) (*httptest.Server, *string) {
	t.Helper()
	var gotBody string
	srv := newRemoteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	})
	return srv, &gotBody
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
			return cmdSlingRemote(base(), remoteTestTarget(srv.URL), []string{"mayor"}, false, false, false, "", nil, "", false, false, false, "", false, true /*stdin*/, false, "", "", false, &out, &errb)
		}, "stdin"},
		{"dry-run", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), remoteTestTarget(srv.URL), []string{"mayor", "BL-1"}, false, false, false, "", nil, "", false, false, false, "", false, false, true /*dryRun*/, "", "", false, &out, &errb)
		}, "dry-run"},
		{"nudge", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), remoteTestTarget(srv.URL), []string{"mayor", "BL-1"}, false, true /*nudge*/, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
		}, "not supported"},
		{"one-arg", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), remoteTestTarget(srv.URL), []string{"BL-1"}, false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
		}, "explicit target"},
		{"inline-text", func() int {
			var out, errb bytes.Buffer
			return cmdSlingRemote(base(), remoteTestTarget(srv.URL), []string{"mayor", "write a readme"}, false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
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
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-42"},
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
	// The resolved remote target is echoed (human mode) so a mutation to a remote
	// control plane is never silent -- matching `gc rig add`.
	if !strings.Contains(errb.String(), "target:") || !strings.Contains(errb.String(), "mc @") {
		t.Errorf("remote sling did not echo the resolved target: %q", errb.String())
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
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "review"},
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
	// JSON mode must not emit the human target echo (JSONL/stderr purity).
	if strings.Contains(errb.String(), "target:") {
		t.Errorf("json-mode remote sling leaked a human target echo: %q", errb.String())
	}
}

// A 2-arg bead sling with --reassign forwards reassign:true to the server. It is
// no longer refused now that RouteOpts + SlingInput carry the field end-to-end
// (RouteOpts.Reassign -> SlingOpts.Reassign -> DoSling), closing the one sling
// envelope gap the execution plan named for Phase 3.
func TestCmdSlingRemote_ForwardsReassign(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"routed","target":"mayor","bead":"BL-7"}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-7"},
		false, false, false /*force*/, "", nil, "", false, false, true /*reassign*/, "", false, false, false, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(gotBody, `"reassign":true`) {
		t.Errorf("request body missing reassign: %q", gotBody)
	}
}

// TestCmdSlingRemote_ForwardsMetadataFlags proves --merge/--no-convoy/--no-formula
// forward to the server instead of being refused (C7).
func TestCmdSlingRemote_ForwardsMetadataFlags(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"routed","target":"mayor","bead":"BL-9"}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-9"},
		false, false, false, "", nil, "direct" /*merge*/, true /*noConvoy*/, false /*owned*/, false, "", true /*noFormula*/, false, false, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	for _, want := range []string{`"merge":"direct"`, `"no_convoy":true`, `"no_formula":true`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body %q missing %q", gotBody, want)
		}
	}
}

// TestCmdSlingRemote_ForwardsOn proves --on forwards to the server as the
// API's formula + attached_bead_id pair. It was refused while the server
// attached the wisp to a convoy container instead of each child; POST /sling now
// goes through the same sling.(*Sling).Dispatch as the local CLI, so a current
// server expands a convoy per child. TestCmdSlingRemote_OnAttachToOlderServer
// covers a server that predates that.
func TestCmdSlingRemote_ForwardsOn(t *testing.T) {
	srv, gotBody := newCannedSlingServer(t, `{"status":"slung","target":"mayor","formula":"review","attached_bead_id":"BL-3","root_bead_id":"BL-3","mode":"attached","molecule_id":"BL-3m"}`)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-3"},
		false, false, false, "", nil, "", false, false, false, "review" /*onFormula*/, false, false, false, "", "", false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	for _, want := range []string{`"formula":"review"`, `"attached_bead_id":"BL-3"`} {
		if !strings.Contains(*gotBody, want) {
			t.Errorf("body %q missing %q", *gotBody, want)
		}
	}
	if strings.Contains(*gotBody, `"bead":`) {
		t.Errorf("body %q carries bead alongside attached_bead_id; the API treats them as mutually exclusive", *gotBody)
	}
}

// TestCmdSlingRemote_JSONCarriesConvoyAndBatch proves the remote --json output
// carries the convoy_id, molecule_id and batch fields POST /sling now returns,
// under the same names the local `gc sling --json` uses.
func TestCmdSlingRemote_JSONCarriesConvoyAndBatch(t *testing.T) {
	srv, _ := newCannedSlingServer(t, `{"status":"slung","target":"mayor","bead":"BL-1","mode":"direct","molecule_id":"BL-m","convoy_id":"BL-c","batch":{"container_type":"convoy","total":3,"routed":2,"failed":0,"skipped":1,"idempotent":1}}`)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-1"},
		false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", true /*json*/, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["molecule_id"] != "BL-m" || got["convoy_id"] != "BL-c" {
		t.Errorf("json = %v, want molecule_id BL-m and convoy_id BL-c", got)
	}
	batch, ok := got["batch"].(map[string]any)
	if !ok {
		t.Fatalf("json batch = %v, want an object", got["batch"])
	}
	if batch["container_type"] != "convoy" || batch["total"] != float64(3) || batch["routed"] != float64(2) || batch["skipped"] != float64(1) || batch["idempotent"] != float64(1) {
		t.Errorf("batch = %v", batch)
	}
}

const partialConvoySlingResponse = `{"status":"partial","target":"mayor","bead":"BL-c","mode":"direct",` +
	`"batch":{"container_type":"convoy","total":3,"routed":1,"failed":1,"skipped":1,"idempotent":0},` +
	`"children":[{"bead_id":"BL-1","outcome":"routed"},{"bead_id":"BL-2","outcome":"failed","reason":"setting gc.routed_to on BL-2: boom"},{"bead_id":"BL-3","outcome":"skipped","status":"closed"}]}`

// A remote convoy sling that routed some children and failed others prints the
// same per-child lines the local path prints, and exits non-zero as it does.
func TestCmdSlingRemote_PartialConvoyPrintsPerChildResults(t *testing.T) {
	srv, _ := newCannedSlingServer(t, partialConvoySlingResponse)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-c"},
		false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1 for a partial convoy; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	for _, want := range []string{
		"Expanding convoy BL-c (3 children, 2 open)",
		"Slung BL-1 → mayor",
		"Skipped BL-3 (status: closed)",
		"Slung 1/3 children of BL-c → mayor",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{"Failed BL-2: setting gc.routed_to on BL-2: boom", "1/2 children failed"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb.String())
		}
	}
}

// A remote --on convoy sling sends the convoy as attached_bead_id, so the
// server echoes no bead. The convoy header and summary still name the convoy:
// it is the one id an operator needs to retry a partial failure.
func TestCmdSlingRemote_OnConvoyNamesTheConvoy(t *testing.T) {
	srv, _ := newCannedSlingServer(t, `{"status":"partial","target":"mayor","formula":"review","attached_bead_id":"BL-c","root_bead_id":"BL-c","mode":"attached",`+
		`"batch":{"container_type":"convoy","total":2,"routed":1,"failed":1,"skipped":0,"idempotent":0},`+
		`"children":[{"bead_id":"BL-1","outcome":"routed","formula":"review","molecule_id":"BL-m1"},{"bead_id":"BL-2","outcome":"failed","reason":"boom"}]}`)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-c"},
		false, false, false, "", nil, "", false, false, false, "review" /*onFormula*/, false, false, false, "", "", false, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1 for a partial convoy; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	for _, want := range []string{
		"Expanding convoy BL-c (2 children, 2 open)",
		`Attached wisp BL-m1 (formula "review") → BL-1`,
		"Slung 1/2 children of BL-c → mayor",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, out.String())
		}
	}
}

// --json for a partial convoy reports success=false with the per-child
// outcomes, and exits non-zero.
func TestCmdSlingRemote_PartialConvoyJSON(t *testing.T) {
	srv, _ := newCannedSlingServer(t, partialConvoySlingResponse)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-c"},
		false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", true /*json*/, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1 for a partial convoy; stderr=%q", code, errb.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["success"] != false || got["status"] != "partial" {
		t.Errorf("json = %v, want success=false status=partial", got)
	}
	children, ok := got["children"].([]any)
	if !ok || len(children) != 3 {
		t.Fatalf("json children = %v, want 3 entries", got["children"])
	}
	failed, _ := children[1].(map[string]any)
	if failed["bead_id"] != "BL-2" || failed["outcome"] != "failed" || failed["reason"] == "" {
		t.Errorf("failed child = %v", failed)
	}
}

const conflictConvoySlingResponse = `{"status":"partial","target":"worker","bead":"BL-c","mode":"direct",` +
	`"batch":{"container_type":"convoy","total":2,"routed":1,"failed":1,"skipped":0,"idempotent":0},` +
	`"children":[{"bead_id":"BL-1","outcome":"routed"},` +
	`{"bead_id":"BL-2","outcome":"failed","reason":"setting gc.routed_to on BL-2: source bead BL-2 already has live workflow(s): wf-1",` +
	`"conflict":{"source_bead_id":"BL-2","blocking_workflow_ids":["wf-1"],"hint":"gc workflow delete-source BL-2 --store-ref rig:r --apply"}}]}`

// A remote convoy sling whose failed child was refused for a live source
// workflow prints the conflict lines gc sling prints locally, with the server's
// cleanup command, and exits 3 as the local path does, in place of the
// "children failed" summary.
func TestCmdSlingRemote_PartialConvoyConflictExits3(t *testing.T) {
	srv, _ := newCannedSlingServer(t, conflictConvoySlingResponse)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"worker", "BL-c"},
		false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", false, &out, &errb)
	if code != 3 {
		t.Fatalf("exit %d, want 3 for a child refused on a source-workflow conflict; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Slung BL-1 → worker") {
		t.Errorf("stdout missing the routed child:\n%s", out.String())
	}
	for _, want := range []string{
		"Failed BL-2: setting gc.routed_to on BL-2",
		"gc sling: source bead BL-2 already has live workflow(s): wf-1\n",
		"gc sling: use --force to override, or gc workflow delete-source BL-2 --store-ref rig:r --apply to clean up\n",
	} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errb.String())
		}
	}
	if strings.Contains(errb.String(), "children failed") {
		t.Errorf("stderr carries the children-failed summary the local path skips on a conflict:\n%s", errb.String())
	}
}

// --json for a partial convoy with a conflicted child keeps the conflict on
// that child and exits 3.
func TestCmdSlingRemote_PartialConvoyConflictJSON(t *testing.T) {
	srv, _ := newCannedSlingServer(t, conflictConvoySlingResponse)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"worker", "BL-c"},
		false, false, false, "", nil, "", false, false, false, "", false, false, false, "", "", true /*json*/, &out, &errb)
	if code != 3 {
		t.Fatalf("exit %d, want 3 for a child refused on a source-workflow conflict; stderr=%q", code, errb.String())
	}
	var got struct {
		Success  bool `json:"success"`
		Children []struct {
			BeadID   string `json:"bead_id"`
			Outcome  string `json:"outcome"`
			Conflict *struct {
				SourceBeadID        string   `json:"source_bead_id"`
				BlockingWorkflowIDs []string `json:"blocking_workflow_ids"`
				Hint                string   `json:"hint"`
			} `json:"conflict"`
		} `json:"children"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got.Success || len(got.Children) != 2 {
		t.Fatalf("json = %s, want success=false with 2 children", out.String())
	}
	if got.Children[0].Conflict != nil {
		t.Errorf("routed child carries a conflict: %s", out.String())
	}
	c := got.Children[1].Conflict
	if c == nil || c.SourceBeadID != "BL-2" || !slices.Equal(c.BlockingWorkflowIDs, []string{"wf-1"}) ||
		c.Hint != "gc workflow delete-source BL-2 --store-ref rig:r --apply" {
		t.Errorf("conflicted child = %s, want its conflict with source_bead_id, blocking_workflow_ids and hint", out.String())
	}
	if !strings.Contains(errb.String(), "gc sling: source bead BL-2 already has live workflow(s): wf-1") {
		t.Errorf("stderr missing the conflict line:\n%s", errb.String())
	}
}

// newOnAttachServer answers POST /sling with slingResp and GET /bead/{id} with
// beadResp, or with a 500 when beadResp is empty. It counts the bead lookups.
func newOnAttachServer(t *testing.T, slingResp, beadResp string) (*httptest.Server, *int) {
	t.Helper()
	lookups := 0
	srv := newRemoteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/city/mc/sling":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(slingResp))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v0/city/mc/bead/"):
			lookups++
			if beadResp == "" {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"status":500,"title":"Internal Server Error","detail":"store unavailable"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(beadResp))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return srv, &lookups
}

// TestCmdSlingRemote_OnAttachToOlderServer pins the version-skew guard on
// remote --on. A server older than this client attached the formula to the
// bead it was given, so on a convoy the wisp landed on the container and no
// child fanned out. Such a server reports no batch, molecule_id or
// workflow_id; the client then looks the bead up and fails loudly when it is
// a convoy. A current server always reports one of the three, so no lookup
// is made.
func TestCmdSlingRemote_OnAttachToOlderServer(t *testing.T) {
	const olderServerAttach = `{"status":"slung","target":"mayor","formula":"review","attached_bead_id":"BL-c","root_bead_id":"BL-c","mode":"attached"}`
	const convoyBead = `{"id":"BL-c","title":"batch","issue_type":"convoy","status":"open"}`
	const taskBead = `{"id":"BL-c","title":"work","issue_type":"task","status":"open"}`
	cases := []struct {
		name        string
		slingResp   string
		beadResp    string
		wantCode    int
		wantLookups int
		wantStderr  []string
	}{
		{
			name:        "older server attached to the convoy itself",
			slingResp:   olderServerAttach,
			beadResp:    convoyBead,
			wantCode:    1,
			wantLookups: 1,
			wantStderr:  []string{`formula "review"`, "convoy BL-c", "each open child", "gc sling mayor <child> --on review"},
		},
		{
			name:        "older server attached to a plain bead",
			slingResp:   olderServerAttach,
			beadResp:    taskBead,
			wantLookups: 1,
		},
		{
			name:        "bead lookup fails",
			slingResp:   olderServerAttach,
			wantLookups: 1,
			wantStderr:  []string{"warning:", "could not check that BL-c is not a convoy", "store unavailable"},
		},
		{
			name:      "current server reports the per-child batch",
			slingResp: `{"status":"slung","target":"mayor","formula":"review","attached_bead_id":"BL-c","root_bead_id":"BL-c","mode":"attached","batch":{"container_type":"convoy","total":2,"routed":2,"failed":0,"skipped":0,"idempotent":0}}`,
		},
		{
			name:      "current server reports the attached molecule",
			slingResp: `{"status":"slung","target":"mayor","formula":"review","attached_bead_id":"BL-c","root_bead_id":"BL-c","mode":"attached","molecule_id":"BL-m"}`,
		},
		{
			name:      "graph formula reports the launched workflow",
			slingResp: `{"status":"slung","target":"mayor","formula":"review","attached_bead_id":"BL-c","root_bead_id":"BL-w","mode":"attached","workflow_id":"BL-w"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, lookups := newOnAttachServer(t, tc.slingResp, tc.beadResp)
			var out, errb bytes.Buffer
			code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-c"},
				false, false, false, "", nil, "", false, false, false, "review" /*onFormula*/, false, false, false, "", "", false, &out, &errb)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d; stdout=%q stderr=%q", code, tc.wantCode, out.String(), errb.String())
			}
			if *lookups != tc.wantLookups {
				t.Errorf("bead lookups = %d, want %d", *lookups, tc.wantLookups)
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(errb.String(), want) {
					t.Errorf("stderr %q missing %q", errb.String(), want)
				}
			}
			if tc.wantCode == 0 && !strings.Contains(out.String(), "slung") {
				t.Errorf("stdout = %q, want the sling result", out.String())
			}
		})
	}
}

// TestCmdSlingRemote_OnAttachToOlderServerJSON proves --json reports the
// container attach as a typed error, so automation does not read the sling as
// a success.
func TestCmdSlingRemote_OnAttachToOlderServerJSON(t *testing.T) {
	srv, _ := newOnAttachServer(t,
		`{"status":"slung","target":"mayor","formula":"review","attached_bead_id":"BL-c","root_bead_id":"BL-c","mode":"attached"}`,
		`{"id":"BL-c","title":"batch","issue_type":"convoy","status":"open"}`)

	var out, errb bytes.Buffer
	code := cmdSlingRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "BL-c"},
		false, false, false, "", nil, "", false, false, false, "review" /*onFormula*/, false, false, false, "", "", true /*json*/, &out, &errb)
	if code != 1 {
		t.Fatalf("exit %d, want 1; stdout=%q", code, out.String())
	}
	var got struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got.OK || got.Error.Code != "attached_to_container" || !strings.Contains(got.Error.Message, "convoy BL-c") {
		t.Errorf("json = %+v, want an attached_to_container error naming convoy BL-c", got)
	}
}
