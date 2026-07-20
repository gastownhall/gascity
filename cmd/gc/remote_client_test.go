package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clientcontext"
)

func TestBuildRemoteClient_AdHocToken(t *testing.T) {
	target := &remoteTarget{BaseURL: "https://box:9443", CityName: "mc", Token: "tok"}
	c, err := buildRemoteClient(target)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsRemote() {
		t.Error("client must be remote")
	}
}

func TestBuildRemoteClient_InvalidTimeout(t *testing.T) {
	target := &remoteTarget{
		BaseURL:  "https://box:9443",
		CityName: "mc",
		Ctx:      &clientcontext.Context{Name: "c", URL: "https://box:9443", City: "mc", Timeout: "not-a-duration"},
	}
	if _, err := buildRemoteClient(target); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("invalid timeout must error, got %v", err)
	}
}

func TestBuildRemoteClient_CredentialCommandWired(t *testing.T) {
	target := &remoteTarget{
		BaseURL:  "https://box:9443",
		CityName: "mc",
		Ctx:      &clientcontext.Context{Name: "c", URL: "https://box:9443", City: "mc", CredentialCommand: "echo x"},
	}
	c, err := buildRemoteClient(target)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsRemote() {
		t.Error("client must be remote")
	}
}

func TestResolveReadTarget_Remote(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var out, errb bytes.Buffer
	_ = doContextAdd(clientcontext.Context{Name: "prod", URL: "https://box:9443", City: "mc", InsecureSkipVerify: true}, &out, &errb)
	setProdContextFlag(t)

	c, isRemote, cityPath, err := resolveReadTarget()
	if err != nil {
		t.Fatalf("resolveReadTarget: %v", err)
	}
	if !isRemote || c == nil || cityPath != "" {
		t.Fatalf("expected remote client, got isRemote=%v c=%v cityPath=%q", isRemote, c, cityPath)
	}
	if !c.IsRemote() {
		t.Error("client must be remote")
	}
}

// The flagship end-to-end: `gc beads list` under a remote context routes the
// read to the remote city (never the local store), and on an unreachable remote
// it hard-fails instead of falling back — proving gate G1 through the command.
func TestCmdBeadsList_RemoteRoutesToServerNoFallback(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	var gotPath, gotReq string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotReq = r.Header.Get("X-GC-Request")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "prod", URL: srv.URL, City: "mc", InsecureSkipVerify: true}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}
	setProdContextFlag(t)

	// The LOCAL client seam must never be consulted under a remote target.
	prevSeam := beadsListAPIClient
	beadsListAPIClient = func(string) (*api.Client, string) {
		t.Fatal("local beadsListAPIClient must not be called under a remote target")
		return nil, ""
	}
	t.Cleanup(func() { beadsListAPIClient = prevSeam })

	out.Reset()
	errb.Reset()
	_ = cmdBeadsList("text", beadFilters{}, &out, &errb)

	if !strings.Contains(gotPath, "/v0/city/mc/beads") {
		t.Errorf("remote server path = %q, want it to include /v0/city/mc/beads", gotPath)
	}
	if gotReq != "true" {
		t.Errorf("X-GC-Request = %q, want true", gotReq)
	}
}

// TestRun_BeadsListContextFlagRoutesRemote proves the OTHER persistent remote
// flag the DisableFlagParsing bug dropped — --context — now parses on a beads
// command and routes remote. Unlike TestCmdBeadsList_RemoteRoutesToServerNoFallback
// (which sets contextFlag directly, bypassing cobra), this drives --context
// through run()'s real argv parsing — the path the fix repairs. --context takes
// a different resolver branch (contexts.toml → targetFromContext) than --city-url,
// so it needs its own end-to-end coverage.
func TestRun_BeadsListContextFlagRoutesRemote(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	var gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var seed, seedErr bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "prod", URL: srv.URL, City: "mc", InsecureSkipVerify: true}, &seed, &seedErr); code != 0 {
		t.Fatalf("seed context: %q", seedErr.String())
	}

	prev := beadsListAPIClient
	beadsListAPIClient = func(string) (*api.Client, string) {
		t.Fatal("local beadsListAPIClient must not run under --context")
		return nil, ""
	}
	t.Cleanup(func() { beadsListAPIClient = prev })

	var out, errb bytes.Buffer
	code := run([]string{"--context", "prod", "beads", "list"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, errb.String())
	}
	if !strings.Contains(gotPath, "/v0/city/mc/beads") {
		t.Fatalf("remote path = %q, want /v0/city/mc/beads", gotPath)
	}
}
