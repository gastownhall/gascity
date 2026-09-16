package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoctorRemoteIsCurrentlyGated(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city-url", srv.URL,
		"--city-name", "remote-city",
		"doctor",
		"--json",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("remote doctor unexpectedly succeeded; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if called {
		t.Fatal("capability-gated remote doctor must not contact the remote city")
	}
	if !strings.Contains(stderr.String(), "does not support a remote city") {
		t.Fatalf("stderr=%q, want the existing remote capability-gate error", stderr.String())
	}
}
