//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestK8sSessionConformance runs the session conformance suite against a
// real Kubernetes cluster via the exec provider. Requires:
//
//	GC_SESSION_K8S_SCRIPT — path to the gc-session-k8s script
//	GC_K8S_IMAGE         — container image (e.g. ubuntu:22.04)
//
// Example:
//
//	GC_SESSION_K8S_SCRIPT=./contrib/session-scripts/gc-session-k8s \
//	GC_K8S_IMAGE=ubuntu:22.04 \
//	go test -tags integration ./test/integration/ -run TestK8sSession
func TestK8sSessionConformance(t *testing.T) {
	script := os.Getenv("GC_SESSION_K8S_SCRIPT")
	if script == "" {
		t.Skip("GC_SESSION_K8S_SCRIPT not set")
	}

	p := sessionexec.NewProvider(script)
	var counter int64

	// Lifecycle tests: each creates its own pod (unavoidable).
	runtimetest.RunLifecycleTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-k8s-conform-%d", id)
		// The external gc-session-k8s script can leave a partially created pod
		// when Start fails. Keep this fallback until that script rolls back its
		// own failed starts; the shared runner owns successful-start cleanup.
		t.Cleanup(func() {
			if err := p.Stop(name); err != nil {
				t.Errorf("Stop(%q) during K8s fallback cleanup: %v", name, err)
			}
		})
		return p, runtime.Config{
			Command: "sleep 300",
			WorkDir: "/tmp",
		}, name
	})

	// Shared-session tests: one pod for all metadata/observation/signaling.
	t.Run("SharedSession", func(t *testing.T) {
		name := "gc-k8s-shared"
		cfg := runtime.Config{Command: "sleep 300", WorkDir: "/tmp"}
		if err := p.Start(context.Background(), name, cfg); err != nil {
			t.Fatalf("Start shared session: %v", err)
		}
		t.Cleanup(func() { _ = p.Stop(name) })
		runtimetest.RunSessionTests(t, p, cfg, name)
	})
}
