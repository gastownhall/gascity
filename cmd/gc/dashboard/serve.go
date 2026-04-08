package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Serve starts the dashboard HTTP server. It creates an APIFetcher, builds
// the dashboard mux, and listens on the given port. This is the entry point
// called by the "gc dashboard serve" cobra command.
func Serve(port int, cityPath, cityName, apiURL, initialCityScope string) error {
	apiURL = strings.TrimRight(apiURL, "/")
	if err := ValidateAPI(apiURL); err != nil {
		return err
	}

	log.Printf("dashboard: using API server at %s", apiURL)
	if initialCityScope != "" {
		log.Printf("dashboard: default city scope %q", initialCityScope)
	}

	isSupervisor := detectSupervisor(apiURL)
	if isSupervisor {
		log.Printf("dashboard: supervisor mode detected, city selector enabled")
	}

	fetcher := NewAPIFetcher(apiURL, cityPath, cityName)

	mux, err := NewDashboardMux(
		fetcher,
		cityPath,
		cityName,
		apiURL,
		initialCityScope,
		isSupervisor,
		8*time.Second,  // fetchTimeout
		30*time.Second, // defaultRunTimeout
		60*time.Second, // maxRunTimeout
	)
	if err != nil {
		return fmt.Errorf("dashboard: failed to create handler: %w", err)
	}

	addr := fmt.Sprintf(":%d", port)
	log.Printf("dashboard: listening on http://localhost%s", addr)
	return http.ListenAndServe(addr, mux)
}

// ValidateAPI checks that the upstream GC API is reachable before the
// dashboard starts serving requests. This prevents a misleading empty UI when
// the user supplied or auto-discovered API endpoint is dead.
func ValidateAPI(apiURL string) error {
	if strings.TrimSpace(apiURL) == "" {
		return fmt.Errorf("dashboard: API server URL is empty")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(apiURL, "/") + "/health")
	if err != nil {
		return fmt.Errorf("dashboard: API server %s is not reachable: %w", apiURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		body = bytes.TrimSpace(body)
		if len(body) == 0 {
			return fmt.Errorf("dashboard: API server %s returned %s from /health", apiURL, resp.Status)
		}
		return fmt.Errorf("dashboard: API server %s returned %s from /health: %s", apiURL, resp.Status, body)
	}
	return nil
}

// detectSupervisor probes the API server for supervisor mode by checking
// whether /v0/cities responds successfully.
func detectSupervisor(apiURL string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(apiURL, "/") + "/v0/cities")
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Any valid JSON response from /v0/cities means supervisor mode. We
	// don't require items to be non-empty since the supervisor may have
	// zero cities registered at startup.
	var list struct {
		Items json.RawMessage `json:"items"`
	}
	return json.NewDecoder(resp.Body).Decode(&list) == nil && list.Items != nil
}
