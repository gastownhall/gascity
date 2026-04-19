package dashboard

import (
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Serve starts the dashboard HTTP server. The dashboard is a static
// TypeScript SPA that calls the supervisor's typed OpenAPI endpoints
// directly from the browser — there is no proxy layer anymore. This
// function's only job is to embed + serve the compiled bundle and
// inject `supervisorURL` into the page so the SPA knows where to
// reach the supervisor.
func Serve(port int, supervisorURL string) error {
	supervisorURL = strings.TrimRight(strings.TrimSpace(supervisorURL), "/")
	if supervisorURL == "" {
		return fmt.Errorf("dashboard: supervisor URL is empty; pass --api")
	}

	handler, err := NewStaticHandler(supervisorURL)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf(":%d", port)
	log.Printf("dashboard: listening on http://localhost%s (supervisor=%s)", addr, supervisorURL)
	return http.ListenAndServe(addr, logRequest(handler))
}
