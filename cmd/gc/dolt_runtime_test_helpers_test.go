package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeReachableManagedDoltState(t *testing.T, cityPath string) int {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatalf("MkdirAll(runtime dolt): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(city .beads): %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	port := ln.Addr().(*net.TCPAddr).Port
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}
	return port
}
