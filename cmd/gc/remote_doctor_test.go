package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDoctorRemoteReadSubset(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city-url", srv.URL,
		"--city-name", "remote-city",
		"doctor",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote doctor failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/remote-city/status" {
		t.Fatalf("request = %s %s, want GET /v0/city/remote-city/status", gotMethod, gotPath)
	}
	var report doctorJSONReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v; stdout=%q", err, stdout.String())
	}
	if len(report.Results) != 5 {
		t.Fatalf("got %d results, want remote status plus four explicit skips: %#v", len(report.Results), report.Results)
	}
	if report.Results[0].Name != "remote-status" || report.Results[0].Status != "ok" {
		t.Fatalf("remote status result = %#v", report.Results[0])
	}
	for _, result := range report.Results[1:] {
		if result.Status != "ok" || !strings.HasPrefix(result.Message, "SKIPPED: ") {
			t.Errorf("local-only result = %#v, want an explicit skip", result)
		}
	}
}

func TestDoctorRemoteCheckTimeout(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := run([]string{
		"--city-url", srv.URL,
		"--city-name", "remote-city",
		"doctor",
		"--check-timeout", "5ms",
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("advisory timeout should not gate the command: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if elapsed := time.Since(started); elapsed >= 150*time.Millisecond {
		t.Fatalf("remote doctor waited for the slow check: %s", elapsed)
	}
	var report doctorJSONReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v; stdout=%q", err, stdout.String())
	}
	if len(report.Results) == 0 || !report.Results[0].TimedOut {
		t.Fatalf("remote status result = %#v, want timed_out=true", report.Results)
	}
}
