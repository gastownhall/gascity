package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func TestRouteReadLocalAPITimeoutFallsBack(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	origTimeout := routeReadLocalAPITimeout
	routeReadLocalAPITimeout = 10 * time.Millisecond
	t.Cleanup(func() { routeReadLocalAPITimeout = origTimeout })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var stderr bytes.Buffer
	localCalls := 0
	start := time.Now()
	code := routeRead(
		api.NewCityScopedClient("http://127.0.0.1:1", "test-city"),
		"probe",
		"no-api",
		&stderr,
		func() error {
			<-release
			return nil
		},
		func() int {
			t.Fatal("api render ran after timed-out fetch")
			return 0
		},
		func() int {
			localCalls++
			return 0
		},
	)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if localCalls != 1 {
		t.Fatalf("local render calls = %d, want 1", localCalls)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("routeRead took %s, want prompt fallback", elapsed)
	}
	if got := stderr.String(); !strings.Contains(got, "route=fallback") || !strings.Contains(got, "reason=api-timeout") {
		t.Fatalf("stderr = %q, want api-timeout fallback route log", got)
	}
}
