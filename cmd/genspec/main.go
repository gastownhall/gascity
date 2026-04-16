// Command genspec writes the live OpenAPI 3.1 spec to disk so downstream
// clients (CLI, dashboard, third-party consumers) can be generated from it.
//
// Usage:
//
//	go run ./cmd/genspec > internal/api/openapi.json
//
// Or via go:generate in the api package. This is the "spec drives everything"
// entry point: the committed spec is the contract; if it drifts from what
// the server serves, CI fails.
//
// The tool instantiates a real api.Server with a stub State implementation.
// Schema generation is reflection-based and does not call State methods —
// handlers are never invoked during spec emission.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

func main() {
	srv := api.New(stubState{})

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		fmt.Fprintf(os.Stderr, "GET /openapi.json returned %d: %s\n", rec.Code, rec.Body.String())
		os.Exit(1)
	}

	// Pretty-print for a stable, reviewable diff.
	var raw any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		fmt.Fprintf(os.Stderr, "parse spec: %v\n", err)
		os.Exit(1)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(raw); err != nil {
		fmt.Fprintf(os.Stderr, "encode spec: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
}

// stubState is a minimal api.State that returns zero values for every method.
// Huma's schema generation is reflection-based and never calls State methods,
// so this is safe even though the methods are trivially wrong.
type stubState struct{}

func (stubState) Config() *config.City                          { return &config.City{} }
func (stubState) SessionProvider() runtime.Provider             { return nil }
func (stubState) BeadStore(string) beads.Store                  { return nil }
func (stubState) BeadStores() map[string]beads.Store            { return nil }
func (stubState) MailProvider(string) mail.Provider             { return nil }
func (stubState) MailProviders() map[string]mail.Provider       { return nil }
func (stubState) EventProvider() events.Provider                { return nil }
func (stubState) CityName() string                              { return "" }
func (stubState) CityPath() string                              { return "" }
func (stubState) Version() string                               { return "" }
func (stubState) StartedAt() time.Time                          { return time.Time{} }
func (stubState) IsQuarantined(string) bool                     { return false }
func (stubState) ClearCrashHistory(string)                      {}
func (stubState) CityBeadStore() beads.Store                    { return nil }
func (stubState) Orders() []orders.Order                        { return nil }
func (stubState) Poke()                                         {}
func (stubState) ServiceRegistry() workspacesvc.Registry        { return nil }
func (stubState) ExtMsgServices() *extmsg.Services              { return nil }
func (stubState) AdapterRegistry() *extmsg.AdapterRegistry      { return nil }
