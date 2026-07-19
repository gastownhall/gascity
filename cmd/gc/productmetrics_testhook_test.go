//go:build productmetrics_testhook

package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/productmetrics"
)

func TestProductMetricsTesthookEndpointAcceptsOnlyLoopbackHTTPS(t *testing.T) {
	for _, endpoint := range []string{
		"https://127.0.0.1:8443/v1/command-usage",
		"https://[::1]:8443/v1/command-usage",
		"https://localhost:8443/v1/command-usage",
	} {
		if err := validateProductMetricsTesthookEndpoint(endpoint); err != nil {
			t.Errorf("validateProductMetricsTesthookEndpoint(%q): %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"",
		"http://127.0.0.1:8080/v1/command-usage",
		"https://metrics.example/v1/command-usage",
		"https://localhost.example/v1/command-usage",
		"https://user@localhost:8443/v1/command-usage",
		"https://localhost:8443/v1/command-usage?secret=value",
		"https://localhost:8443/v1/command-usage#fragment",
	} {
		if err := validateProductMetricsTesthookEndpoint(endpoint); err == nil {
			t.Errorf("validateProductMetricsTesthookEndpoint(%q) succeeded", endpoint)
		}
	}
}

func TestProductMetricsTaggedRunnerReadsInjectionOnlyAtInvocation(t *testing.T) {
	t.Setenv(taggedProductMetricsEndpointEnvironment, "https://metrics.example/v1/command-usage")
	invocation, detected, err := productmetrics.ParsePrivateUploaderInvocation([]string{
		productMetricsPrivateUploaderSentinelFixture,
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	})
	if err != nil || !detected {
		t.Fatalf("parse private uploader invocation = (%t, %v)", detected, err)
	}
	err = privateProductMetricsRunnerFactory()(context.Background(), invocation)
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("tagged runner error = %v, want loopback rejection", err)
	}
}

func TestProductMetricsTesthookCAReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	want := []byte("test certificate bytes")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProductMetricsTesthookCA(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("readProductMetricsTesthookCA = %q, %v; want %q", got, err, want)
	}

	if err := os.WriteFile(path, make([]byte, taggedProductMetricsMaximumCABytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readProductMetricsTesthookCA(path); err == nil {
		t.Fatal("oversized product metrics testhook CA file was accepted")
	}
	if _, err := readProductMetricsTesthookCA(""); err == nil {
		t.Fatal("empty product metrics testhook CA path was accepted")
	}
}

func TestProductMetricsTaggedProcessFixtureIsEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	seedPrivateUploaderProcessFixture(t, home, "6ba7b810-9dad-41d1-80b4-00c04fd430c8", time.Now().UTC())
	service, err := productmetrics.OpenTesthook(productmetrics.TesthookOptions{
		Home:           gchome.ResolveReadOnly(),
		ReleaseVersion: productMetricsTestReleaseVersion,
		MetricsEpoch:   1,
		NoticeVersion:  1,
		NoticeText:     []byte("Gas City product metrics test-only notice."),
		Endpoint:       "https://127.0.0.1:1/v1/command-usage",
		Client:         &http.Client{Transport: &http.Transport{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := service.Status(context.Background())
	if status.State != productmetrics.StateEnabled {
		t.Fatalf("tagged process fixture status = (%q, %q), want enabled", status.State, status.Reason)
	}
}
