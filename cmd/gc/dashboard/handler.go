package dashboard

import (
	"bytes"
	"encoding/json"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"
)

// Embed the compiled SPA bundle produced by `cmd/gc/dashboard/web/`.
// The bundle is a Vite build output: one index.html (with a
// `<meta name="supervisor-url">` placeholder), one dashboard.js,
// one dashboard.css. The Go static server ships these bytes
// verbatim — the SPA handles everything else by calling the
// supervisor's typed OpenAPI endpoints directly.
//
//go:embed web/dist
var spaBundle embed.FS

const maxClientLogBody = 64 << 10

type clientLogEntry struct {
	City    string          `json:"city"`
	Details json.RawMessage `json:"details,omitempty"`
	Level   string          `json:"level"`
	Message string          `json:"message"`
	Scope   string          `json:"scope"`
	TS      string          `json:"ts"`
	URL     string          `json:"url"`
}

// NewStaticHandler returns a handler that serves the SPA bundle.
// `supervisorURL` is injected into index.html so the SPA knows where
// to reach the supervisor (cross-origin: the dashboard server binds
// its own port, the supervisor binds another, the browser talks to
// both).
func NewStaticHandler(supervisorURL string) (http.Handler, error) {
	sub, err := fs.Sub(spaBundle, "web/dist")
	if err != nil {
		return nil, fmt.Errorf("dashboard: embed sub fs: %w", err)
	}
	indexBytes, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, fmt.Errorf("dashboard: read embedded index.html: %w", err)
	}
	indexWithURL := injectSupervisorURL(indexBytes, supervisorURL)

	fileServer := http.FileServer(http.FS(sub))

	mux := http.NewServeMux()
	mux.HandleFunc("/__client-log", handleClientLog)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Every request that isn't a known static asset routes to
		// index.html so client-side navigation works. The SPA has no
		// routes today beyond the query-string city scope, but this
		// future-proofs the server against additions.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" || path == "index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			_, _ = w.Write(indexWithURL)
			return
		}
		if _, err := fs.Stat(sub, path); err == nil {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			fileServer.ServeHTTP(w, r)
			return
		}
		// Unknown path under an SPA: serve index and let client-side
		// code figure out what to render (e.g. a "not found" panel).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexWithURL)
	})

	return mux, nil
}

func handleClientLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close() //nolint:errcheck

	var entry clientLogEntry
	if err := json.NewDecoder(io.LimitReader(r.Body, maxClientLogBody)).Decode(&entry); err != nil {
		log.Printf("dashboard: client log decode failed from %s: %v", r.RemoteAddr, err)
		http.Error(w, "invalid client log payload", http.StatusBadRequest)
		return
	}

	level := strings.TrimSpace(entry.Level)
	if level == "" {
		level = "info"
	}
	scope := strings.TrimSpace(entry.Scope)
	if scope == "" {
		scope = "client"
	}
	if strings.TrimSpace(entry.Message) == "" {
		http.Error(w, "missing client log message", http.StatusBadRequest)
		return
	}
	ts := strings.TrimSpace(entry.TS)
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}

	log.Printf(
		"dashboard: client[%s] ts=%s scope=%s city=%q url=%q msg=%q details=%s ua=%q",
		level,
		ts,
		scope,
		entry.City,
		entry.URL,
		entry.Message,
		rawJSONDetails(entry.Details),
		r.UserAgent(),
	)

	w.WriteHeader(http.StatusNoContent)
}

// injectSupervisorURL rewrites the `<meta name="supervisor-url" content="…">`
// tag to embed the real supervisor URL. The SPA reads this at load
// time to construct its typed client. Kept as a byte-level edit so
// there is no HTML parse overhead and no risk of the template
// engine escaping the URL. Vite emits the meta tag in the
// self-closed form (`content="" />`), so we match both spellings
// defensively.
func injectSupervisorURL(index []byte, supervisorURL string) []byte {
	replacement := fmt.Sprintf(`<meta name="supervisor-url" content="%s">`, htmlEscape(supervisorURL))
	for _, placeholder := range []string{
		`<meta name="supervisor-url" content="" />`,
		`<meta name="supervisor-url" content=""/>`,
		`<meta name="supervisor-url" content="">`,
	} {
		if bytes.Contains(index, []byte(placeholder)) {
			return bytes.Replace(index, []byte(placeholder), []byte(replacement), 1)
		}
	}
	return index
}

// htmlEscape performs the minimal escape the supervisor URL
// actually needs — quotes and angle brackets — since the URL is
// embedded in a `content="..."` attribute. Using a bespoke escaper
// keeps this package free of template/html dependencies.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		`&`, `&amp;`,
		`"`, `&quot;`,
		`<`, `&lt;`,
		`>`, `&gt;`,
	)
	return r.Replace(s)
}

func rawJSONDetails(details json.RawMessage) string {
	if len(details) == 0 {
		return "null"
	}
	trimmed := strings.TrimSpace(string(details))
	if trimmed == "" {
		return "null"
	}
	return trimmed
}

// logRequest is a thin middleware used by Serve.
func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("dashboard: %s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
