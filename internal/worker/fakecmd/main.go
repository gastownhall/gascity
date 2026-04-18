// Package main provides the standalone fake-worker helper binary used by
// worker conformance tests.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gastownhall/gascity/internal/worker/fake"
)

func main() {
	var (
		configPath  string
		controlFile string
		startFile   string
	)

	flag.StringVar(&configPath, "config", "", "path to fake worker config (.json|.yaml)")
	flag.StringVar(&controlFile, "control-file", "", "override control file path for wait_for_control steps")
	flag.StringVar(&startFile, "start-file", "", "override startup control file path")
	flag.Parse()

	configPath = firstNonEmpty(configPath, os.Getenv("GC_FAKE_WORKER_CONFIG"))
	if configPath == "" {
		failf("missing config path; set -config or GC_FAKE_WORKER_CONFIG")
	}

	cfg, err := fake.LoadHelperConfig(configPath)
	if err != nil {
		failf("load config: %v", err)
	}

	if controlFile != "" {
		for i := range cfg.Scenario.Steps {
			if cfg.Scenario.Steps[i].Action == "wait_for_control" {
				cfg.Scenario.Steps[i].Path = controlFile
			}
		}
	}
	if startFile != "" {
		cfg.Control.StartFile = startFile
	}
	if envStart := os.Getenv("GC_FAKE_WORKER_START_FILE"); envStart != "" {
		cfg.Control.StartFile = envStart
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := (fake.Runner{}).Run(ctx, cfg, os.Stdout); err != nil {
		failf("run fake worker: %v", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
