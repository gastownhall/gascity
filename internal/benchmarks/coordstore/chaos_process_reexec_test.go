package coordstore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

func TestMain(m *testing.M) {
	if backend := os.Getenv("CHAOS_SERVER_BACKEND"); backend != "" {
		os.Exit(runChaosServerProcess(backend))
	}
	os.Exit(m.Run())
}

func TestChaosProcessReexecKillAndRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := t.TempDir()
	process := coordstore.NewChaosProcess(coordstore.ChaosProcessConfig{
		Backend:         "authorcore",
		SocketPath:      filepath.Join(dir, "chaos.sock"),
		DataDir:         filepath.Join(dir, "store"),
		AckedWritesPath: filepath.Join(dir, "acked-writes.jsonl"),
	})
	if err := process.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer process.Close() //nolint:errcheck

	if _, err := process.Create(ctx, coordstore.Record{
		ID:        "chaos-before-kill",
		Title:     "before kill",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Create before kill: %v", err)
	}
	if err := process.Kill(ctx); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := process.Get(ctx, "chaos-before-kill"); err == nil {
		t.Fatalf("Get while child is killed succeeded")
	}
	if _, err := process.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if _, err := process.Create(ctx, coordstore.Record{
		ID:        "chaos-after-restart",
		Title:     "after restart",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Create after restart: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "acked-writes.jsonl"))
	if err != nil {
		t.Fatalf("read ack ledger: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(data)), "\n") + 1; got != 2 {
		t.Fatalf("ack ledger lines = %d, want 2; data=%s", got, data)
	}
}

func TestChaosClientResetAckLedgerClearsSeedWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	process := coordstore.NewChaosProcess(coordstore.ChaosProcessConfig{
		Backend:    "authorcore",
		SocketPath: filepath.Join(dir, "chaos.sock"),
		DataDir:    filepath.Join(dir, "store"),
	})
	if err := process.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer process.Close() //nolint:errcheck

	if _, err := process.Create(ctx, coordstore.Record{
		ID:        "seed-write",
		Title:     "seed write",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Create seed: %v", err)
	}
	if got := process.AckedIDs(); len(got) != 1 {
		t.Fatalf("seed AckedIDs = %#v, want one entry before reset", got)
	}

	ledgerPath := filepath.Join(dir, "acked-writes.jsonl")
	if err := process.ResetAckLedger(ledgerPath); err != nil {
		t.Fatalf("ResetAckLedger: %v", err)
	}
	if got := process.AckedIDs(); len(got) != 0 {
		t.Fatalf("AckedIDs after reset = %#v, want empty", got)
	}
	if _, err := process.Create(ctx, coordstore.Record{
		ID:        "workload-write",
		Title:     "workload write",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Create workload: %v", err)
	}
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ack ledger: %v", err)
	}
	if strings.Contains(string(data), "seed-write") {
		t.Fatalf("ack ledger includes seed write after reset: %s", data)
	}
	if !strings.Contains(string(data), "workload-write") {
		t.Fatalf("ack ledger missing workload write after reset: %s", data)
	}
}

func runChaosServerProcess(backend string) int {
	socketPath := os.Getenv("CHAOS_SERVER_SOCKET")
	dataDir := os.Getenv("CHAOS_SERVER_DATA_DIR")
	if socketPath == "" || dataDir == "" {
		fmt.Fprintf(os.Stderr, "CHAOS_SERVER_SOCKET and CHAOS_SERVER_DATA_DIR are required\n")
		return 2
	}

	var factory *adapterFactory
	for _, candidate := range buildRegisteredAdapters() {
		candidate := candidate
		if candidate.name == backend {
			factory = &candidate
			break
		}
	}
	if factory == nil {
		fmt.Fprintf(os.Stderr, "unknown chaos backend %q\n", backend)
		return 2
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create chaos data dir: %v\n", err)
		return 2
	}
	adapter := factory.newFn()
	ctx := context.Background()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: dataDir}); err != nil {
		fmt.Fprintf(os.Stderr, "open chaos adapter %q: %v\n", backend, err)
		return 2
	}
	defer adapter.Close() //nolint:errcheck
	if err := coordstore.RunChaosServer(ctx, adapter, socketPath, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "run chaos server %q: %v\n", backend, err)
		return 2
	}
	return 0
}
