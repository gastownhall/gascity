package dashboard

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInjectSupervisorURL verifies the meta-tag placeholder gets
// replaced with the real URL on page load. This is the only dynamic
// bit the Go static server owns.
func TestInjectSupervisorURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		orig string
		want string
	}{
		{
			name: "localhost non-selfclose",
			url:  "http://127.0.0.1:8372",
			orig: `<meta name="supervisor-url" content="">`,
			want: `<meta name="supervisor-url" content="http://127.0.0.1:8372">`,
		},
		{
			name: "vite self-closed form",
			url:  "http://127.0.0.1:8372",
			orig: `<meta name="supervisor-url" content="" />`,
			want: `<meta name="supervisor-url" content="http://127.0.0.1:8372">`,
		},
		{
			name: "html-escape in URL",
			url:  `http://example.com/?q="x"&y=<z>`,
			orig: `<meta name="supervisor-url" content="">`,
			want: `<meta name="supervisor-url" content="http://example.com/?q=&quot;x&quot;&amp;y=&lt;z&gt;">`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(injectSupervisorURL([]byte(tc.orig), tc.url))
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestStaticHandlerServesIndex confirms the handler injects the URL
// into the served index and that dashboard.js is reachable.
func TestStaticHandlerServesIndex(t *testing.T) {
	h, err := NewStaticHandler("http://127.0.0.1:8372")
	if err != nil {
		t.Fatalf("NewStaticHandler: %v", err)
	}

	// Index.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<meta name="supervisor-url" content="http://127.0.0.1:8372">`) {
		t.Errorf("index missing injected supervisor-url meta; body:\n%s", body)
	}

	// Bundle.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard.js: %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("dashboard.js was empty")
	}

	// Unknown path falls back to index.html so the SPA's
	// client-side router (such as it is) can handle unknown
	// routes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/unknown/deep/path", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback GET: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<meta name="supervisor-url"`) {
		t.Errorf("fallback did not serve SPA index")
	}
}

func TestStaticHandlerAcceptsClientLogs(t *testing.T) {
	h, err := NewStaticHandler("http://127.0.0.1:8372")
	if err != nil {
		t.Fatalf("NewStaticHandler: %v", err)
	}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/__client-log", strings.NewReader(`{
		"ts":"2026-04-17T16:00:00Z",
		"level":"error",
		"scope":"mail",
		"message":"Compose failed",
		"details":{"reason":"missing recipient"},
		"url":"http://localhost:8080/?city=mc-city",
		"city":"mc-city"
	}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /__client-log: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), `client[error]`) {
		t.Fatalf("client log output missing level: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `scope=mail`) {
		t.Fatalf("client log output missing scope: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"reason":"missing recipient"`) {
		t.Fatalf("client log output missing details: %s", logs.String())
	}
}
