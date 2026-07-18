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
	"context"
	"errors"
	"flag"
	"fmt"
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

	// Announce the bound address on stdout so the Playwright webServer / shell
	// harness can read the port when -addr used port 0.
	fmt.Printf("listening on %s\n", baseURL)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

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
