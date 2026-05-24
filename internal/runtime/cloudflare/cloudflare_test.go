package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNewProviderRequiresAbsoluteEndpoint(t *testing.T) {
	if _, err := NewProviderWithConfig(Config{}); err == nil {
		t.Fatal("NewProviderWithConfig succeeded without endpoint")
	}
	if _, err := NewProviderWithConfig(Config{Endpoint: "/relative"}); err == nil {
		t.Fatal("NewProviderWithConfig succeeded with relative endpoint")
	}
}

func TestStartPostsTypedConfig(t *testing.T) {
	var gotPath string
	var gotAuth string
	var got startConfig

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{
		Endpoint: server.URL + "/runtime",
		Token:    "secret",
	})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	err = p.Start(context.Background(), "sess-one", runtime.Config{
		WorkDir:           "/work",
		Command:           "codex exec",
		Env:               map[string]string{"GC_CITY": "/city"},
		ProcessNames:      []string{"codex"},
		Nudge:             "hello",
		SessionLive:       []string{"echo live"},
		ProviderName:      "codex",
		PromptFlag:        "--prompt",
		PromptSuffix:      "do work",
		FingerprintExtra:  map[string]string{"pool": "workers"},
		PackOverlayDirs:   []string{"/pack/overlay"},
		InstallAgentHooks: []string{"gemini"},
		CopyFiles: []runtime.CopyEntry{{
			Src:    "/host/file",
			RelDst: "file",
			Probed: true,
		}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if gotPath != "/runtime/sessions/sess-one/start" {
		t.Fatalf("path = %q, want /runtime/sessions/sess-one/start", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q, want bearer token", gotAuth)
	}
	if got.WorkDir != "/work" || got.Command != "codex exec" {
		t.Fatalf("start config = %+v, missing workdir/command", got)
	}
	if got.Env["GC_CITY"] != "/city" {
		t.Fatalf("env = %#v, missing GC_CITY", got.Env)
	}
	if len(got.ProcessNames) != 1 || got.ProcessNames[0] != "codex" {
		t.Fatalf("process_names = %#v, want codex", got.ProcessNames)
	}
	if len(got.CopyFiles) != 1 || got.CopyFiles[0].Src != "/host/file" || got.CopyFiles[0].RelDst != "file" {
		t.Fatalf("copy_files = %#v, want stable copy entry", got.CopyFiles)
	}
}

func TestStartConflictWrapsSessionExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"already exists"}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	err = p.Start(context.Background(), "sess-one", runtime.Config{})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start error = %v, want ErrSessionExists", err)
	}
}

func TestStopTreatsNotFoundAsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.Stop("missing"); err != nil {
		t.Fatalf("Stop missing: %v", err)
	}
}

func TestIsRunningDecodesRemoteState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.EscapedPath() != "/sessions/sess-one/running" {
			t.Fatalf("path = %q, want running endpoint", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"running":true}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if !p.IsRunning("sess-one") {
		t.Fatal("IsRunning = false, want true")
	}
}

func TestListRunningUsesPrefixQuery(t *testing.T) {
	var gotPrefix string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrefix = r.URL.Query().Get("prefix")
		if r.URL.EscapedPath() != "/sessions" {
			t.Fatalf("path = %q, want /sessions", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"names":["alpha-1","alpha-2"]}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	names, err := p.ListRunning("alpha")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if gotPrefix != "alpha" {
		t.Fatalf("prefix = %q, want alpha", gotPrefix)
	}
	if len(names) != 2 || names[0] != "alpha-1" || names[1] != "alpha-2" {
		t.Fatalf("names = %#v, want alpha sessions", names)
	}
}

func TestGetLastActivityParsesRFC3339Nano(t *testing.T) {
	want := time.Date(2026, 5, 24, 2, 14, 25, 123, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(activityResponse{LastActivity: want.Format(time.RFC3339Nano)})
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	got, err := p.GetLastActivity("sess-one")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("last activity = %s, want %s", got, want)
	}
}

func TestNudgePostsContentBlocks(t *testing.T) {
	var got nudgeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/sessions/sess-one/nudge" {
			t.Fatalf("path = %q, want nudge endpoint", r.URL.EscapedPath())
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	err = p.Nudge("sess-one", runtime.TextContent("hello"))
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "hello" {
		t.Fatalf("content = %#v, want text block", got.Content)
	}
}

func TestGetMetaNotFoundReturnsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	got, err := p.GetMeta("sess-one", "missing")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "" {
		t.Fatalf("GetMeta = %q, want empty", got)
	}
}
