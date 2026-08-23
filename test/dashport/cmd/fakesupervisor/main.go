//go:build integration

// Command fakesupervisor serves the seeded dashboard e2e city over a real HTTP
// listener so a browser (Playwright, Layer B) can drive the same corpus the Go
// serve-level integration test (Layer A) asserts. It is the browser-facing peer
// of the Go integration test: it loads test/dashport/testdata/dashport through
// the shared corpus loader and serves it via api.ServeSeededCity, so the SPA and
// its same-origin /v0 + /api surfaces are hosted on one listener.
//
// It is built with -tags integration and never ships in the production binary.
//
// Usage:
//
//	fakesupervisor -data <testdata/dashport dir> [-addr 127.0.0.1:0]
//
// The chosen address is printed to stdout as "listening on http://host:port"
// once the listener binds, so the Playwright config (or a shell harness) can
// read the port when -addr uses port 0. SIGINT/SIGTERM drains the plane and
// shuts the listener down gracefully.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/test/dashport/corpus"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fakesupervisor: %v", err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:0", "listen address; port 0 picks a free port and prints it")
	dataDir := flag.String("data", "", "path to the testdata/dashport corpus directory (required)")
	flag.Parse()

	if *dataDir == "" {
		return errors.New("-data (path to testdata/dashport) is required")
	}
	resolvedData, err := filepath.Abs(*dataDir)
	if err != nil {
		return fmt.Errorf("resolve -data %q: %w", *dataDir, err)
	}

	// A scratch city root the corpus loader writes the seeded event log into.
	// Cleaned up on exit; the run tailers read <cityPath>/.gc/events.jsonl.
	cityPath, err := os.MkdirTemp("", "fakesupervisor-city-")
	if err != nil {
		return fmt.Errorf("create city path: %w", err)
	}
	defer os.RemoveAll(cityPath) //nolint:errcheck

	// Pin the two HOST-scoped Activity data sources to the empty scratch city so
	// the Deploys and Commits panes render their designed empty states
	// deterministically on every host, independent of the dev box's real $HOME or
	// git checkout. The dashboard BFF reads deploy history from $HOME/.dev-deploy-log
	// (dashboardbff/builds.go) — absent under this empty root — and git commits from
	// $ADMIN_GIT_REPO (dashboardbff/git.go), a non-git directory here, so `git log`
	// yields no commits. Neither source is derived from the seeded city, so the
	// designed empty state is the truthful branch; pinning them just makes it
	// reproducible rather than dependent on the operator's home directory.
	_ = os.Setenv("HOME", cityPath)
	_ = os.Setenv("ADMIN_GIT_REPO", cityPath)

	fx, err := corpus.Load(resolvedData, cityPath)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	defer fx.Close() //nolint:errcheck

	// ctx drives the plane's run tailers and status samplers; cancel on signal
	// so they drain before the process exits.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bind the listener first so a port-0 request resolves to a concrete port
	// before the SPA's status samplers dial the loopback base URL.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", *addr, err)
	}
	baseURL := "http://" + ln.Addr().String()

	handler, stopPlane, err := api.ServeSeededCity(ctx, api.SeededCityDeps{
		CityName:      fx.CityName,
		CityPath:      fx.CityPath,
		Config:        fx.Config,
		CityBeadStore: fx.CityStore,
		RigStores:     fx.RigStores,
		MailProvider:  fx.MailProv,
		EventProvider: fx.EventProv,
	}, baseURL)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("serve seeded city: %w", err)
	}
	// Drain the plane's run tailers and status samplers on exit, after the
	// listener stops accepting new requests.
	defer stopPlane()

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Warm the Health-view background samplers before announcing readiness. The
	// supervisor-status / rig-store-health / dolt-noms samplers start lazily on
	// first request and publish their first snapshot only after a background
	// refresh completes; the SPA fetches each once on mount and does not refetch
	// for 30s, so a browser that loads /health in that cold window renders the
	// "sample is warming up" state instead of the populated tiles. Touching the
	// endpoints here starts the samplers at boot and blocks (bounded) until
	// supervisor-status reports available, so the Playwright render smoke — which
	// launches its browser well after this returns — always sees populated tiles.
	// This mirrors a real supervisor, whose samplers have long since warmed by the
	// time an operator opens the dashboard.
	warmHealthSamplers(ctx, baseURL, fx.CityName)

	// Announce the bound address on stdout so the Playwright webServer / shell
	// harness can read the port when -addr used port 0.
	fmt.Printf("listening on %s\n", baseURL)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	case err := <-serveErr:
		return err
	}
}

// warmHealthSamplers triggers the per-city Health-view samplers over the just-
// bound loopback listener and blocks (bounded) until the supervisor-status
// sampler publishes an available snapshot. Each GET starts the corresponding
// lazily-initialized sampler; the supervisor-status poll then waits for its first
// background refresh (a loopback /v0 status read, sub-second) to land so the tile
// renders populated rather than "warming up". It is best-effort: on ctx
// cancellation or a bounded timeout it returns quietly and lets the samplers warm
// on their own cadence — the render smoke's browser launch already lags this by
// seconds, so a partial warm still resolves before the first fetch.
func warmHealthSamplers(ctx context.Context, baseURL, cityName string) {
	client := &http.Client{Timeout: 3 * time.Second}
	base := baseURL + "/api/city/" + cityName
	// Touch the rig-store and dolt-noms samplers once so they start alongside
	// supervisor-status; their first snapshots follow the same refresh.
	for _, path := range []string{"/rig-store-health", "/dolt-noms/trend"} {
		drain(client, base+path)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		if body, ok := drain(client, base+"/supervisor-status"); ok &&
			bytes.Contains(body, []byte(`"available":true`)) {
			// Re-touch the other two so their first ring/probe is published too.
			drain(client, base+"/rig-store-health")
			drain(client, base+"/dolt-noms/trend")
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// drain GETs url and returns its body, discarding transport errors — the caller
// only needs the side effect of starting a sampler and the optional body.
func drain(client *http.Client, url string) ([]byte, bool) {
	resp, err := client.Get(url) //nolint:noctx // bounded by client.Timeout
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return body, true
}
