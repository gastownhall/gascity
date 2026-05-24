package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestStartPostsToSessionCollection(t *testing.T) {
	var gotPath string
	var gotAuth string
	var got startRequest

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

	if gotPath != "/runtime/session" {
		t.Fatalf("path = %q, want /runtime/session", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q, want bearer token", gotAuth)
	}
	if got.SessionID != "sess-one" {
		t.Fatalf("sessionId = %q, want sess-one", got.SessionID)
	}
	if got.Config.WorkDir != "/work" || got.Config.Command != "codex exec" {
		t.Fatalf("config = %+v, missing workdir/command", got.Config)
	}
	if got.Config.Env["GC_CITY"] != "/city" {
		t.Fatalf("env = %#v, missing GC_CITY", got.Config.Env)
	}
	if len(got.Config.ProcessNames) != 1 || got.Config.ProcessNames[0] != "codex" {
		t.Fatalf("process_names = %#v, want codex", got.Config.ProcessNames)
	}
	if len(got.Config.CopyFiles) != 1 || got.Config.CopyFiles[0].Src != "/host/file" || got.Config.CopyFiles[0].RelDst != "file" {
		t.Fatalf("copy_files = %#v, want stable copy entry", got.Config.CopyFiles)
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

func TestIsRunningDecodesStatusEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.EscapedPath() != "/session/sess-one/status" {
			t.Fatalf("path = %q, want /session/sess-one/status", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"alive":true}`))
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

func TestIsAttachedAlwaysReturnsFalse(t *testing.T) {
	p, err := NewProviderWithConfig(Config{Endpoint: "http://unused.example"})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}
	if p.IsAttached("any") {
		t.Fatal("IsAttached = true, want false (unsupported)")
	}
}

func TestListRunningNotSupported(t *testing.T) {
	p, err := NewProviderWithConfig(Config{Endpoint: "http://unused.example"})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}
	names, err := p.ListRunning("any")
	if err == nil {
		t.Fatalf("ListRunning succeeded, want error")
	}
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil", names)
	}
}

func TestGetLastActivityParsesCreatedAt(t *testing.T) {
	want := time.Date(2026, 5, 24, 2, 14, 25, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"alive":true,"record":{"createdAt":"` + want.Format(time.RFC3339Nano) + `"}}`))
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

func TestNudgePostsText(t *testing.T) {
	var got nudgeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/session/sess-one/nudge" {
			t.Fatalf("path = %q, want /session/sess-one/nudge", r.URL.EscapedPath())
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
	if got.Text != "hello" {
		t.Fatalf("text = %q, want hello", got.Text)
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

func TestGetMetaReturnsValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/session/sess-one/meta/mykey" {
			t.Fatalf("path = %q, want /session/sess-one/meta/mykey", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"value":"myval"}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	got, err := p.GetMeta("sess-one", "mykey")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "myval" {
		t.Fatalf("GetMeta = %q, want myval", got)
	}
}

func TestSetMetaPostsKeyAndValue(t *testing.T) {
	var gotPath, gotValue string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		var body metaRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotValue = body.Value
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.SetMeta("sess-one", "mykey", "myval"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if gotPath != "/session/sess-one/meta/mykey" {
		t.Fatalf("path = %q, want /session/sess-one/meta/mykey", gotPath)
	}
	if gotValue != "myval" {
		t.Fatalf("value = %q, want myval", gotValue)
	}
}

func TestRemoveMetaTreatsNotFoundAsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.RemoveMeta("sess-one", "gone"); err != nil {
		t.Fatalf("RemoveMeta not-found: %v", err)
	}
}

func TestPeekDecodesOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/session/sess-one/peek" {
			t.Fatalf("path = %q, want /session/sess-one/peek", r.URL.EscapedPath())
		}
		var body peekRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Lines != 20 {
			t.Fatalf("lines = %d, want 20", body.Lines)
		}
		_, _ = w.Write([]byte(`{"output":"hello\nworld"}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	got, err := p.Peek("sess-one", 20)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if got != "hello\nworld" {
		t.Fatalf("output = %q, want hello\\nworld", got)
	}
}

func TestProcessAliveUsesExtendedRegex(t *testing.T) {
	var gotBody execRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/session/sess-one/exec" {
			t.Fatalf("path = %q, want /session/sess-one/exec", r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"exitCode":0,"success":true}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if !p.ProcessAlive("sess-one", []string{"codex", "claude"}) {
		t.Fatal("ProcessAlive = false, want true")
	}
	if !strings.Contains(gotBody.Cmd, "pgrep -Ef --") {
		t.Fatalf("cmd = %q, want pgrep -Ef -- flag", gotBody.Cmd)
	}
	if !strings.Contains(gotBody.Cmd, "codex|claude") {
		t.Fatalf("cmd = %q, want pattern with alternation", gotBody.Cmd)
	}
}

func TestProcessAliveNonZeroExitMeansDead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"exitCode":1,"success":false}`))
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if p.ProcessAlive("sess-one", []string{"codex"}) {
		t.Fatal("ProcessAlive = true, want false for non-zero exit")
	}
}

func TestInterruptPostsToExec(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.Interrupt("sess-one"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if gotPath != "/session/sess-one/exec" {
		t.Fatalf("path = %q, want /session/sess-one/exec", gotPath)
	}
}

func TestInterruptTreatsNotFoundAsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.Interrupt("gone"); err != nil {
		t.Fatalf("Interrupt missing session: %v", err)
	}
}

func TestClearScrollbackPostsToExec(t *testing.T) {
	var gotBody execRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/session/sess-one/exec" {
			t.Fatalf("path = %q, want /session/sess-one/exec", r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.ClearScrollback("sess-one"); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}
	if !strings.Contains(gotBody.Cmd, ".gc-scrollback") {
		t.Fatalf("cmd = %q, want scrollback truncate command", gotBody.Cmd)
	}
}

func TestSendKeysPostsToKeysEndpoint(t *testing.T) {
	var gotBody sendKeysRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/session/sess-one/keys" {
			t.Fatalf("path = %q, want /session/sess-one/keys", r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.SendKeys("sess-one", "Up", "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if len(gotBody.Keys) != 2 || gotBody.Keys[0] != "Up" || gotBody.Keys[1] != "Enter" {
		t.Fatalf("keys = %v, want [Up Enter]", gotBody.Keys)
	}
}

func TestNudgeDropsEmptyContent(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	p, err := NewProviderWithConfig(Config{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}

	if err := p.Nudge("sess-one", nil); err != nil {
		t.Fatalf("Nudge nil: %v", err)
	}
	if called {
		t.Fatal("Nudge made HTTP call for empty content, want no-op")
	}
}

func TestRunLiveReturnsNil(t *testing.T) {
	p, err := NewProviderWithConfig(Config{Endpoint: "http://unused.example"})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}
	if err := p.RunLive("sess-one", runtime.Config{}); err != nil {
		t.Fatalf("RunLive = %v, want nil", err)
	}
}

func TestCapabilitiesReflectsConfig(t *testing.T) {
	p, err := NewProviderWithConfig(Config{Endpoint: "http://unused.example", ReportActivity: true})
	if err != nil {
		t.Fatalf("NewProviderWithConfig: %v", err)
	}
	caps := p.Capabilities()
	if !caps.CanReportActivity {
		t.Fatal("CanReportActivity = false, want true when configured")
	}
	if caps.CanReportAttachment {
		t.Fatal("CanReportAttachment = true, want always false")
	}
}
