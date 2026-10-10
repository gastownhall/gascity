package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
)

// gc storage connect (cmd_storage_connect.go). The library half,
// beads.ConnectRemoteScope, is exercised end to end against the bd serve
// stand-in in internal/beads (attach shape, project pin, credential, the
// wire_compat refusal, and the native open that follows); here the seam
// answers so the tests state what the command hands it and reports.

func stubConnectRemoteScope(t *testing.T, result beads.RemoteConnectResult, err error) *[]beads.RemoteConnectRequest {
	t.Helper()
	var requests []beads.RemoteConnectRequest
	previous := connectRemoteScope
	connectRemoteScope = func(_ context.Context, req beads.RemoteConnectRequest) (beads.RemoteConnectResult, error) {
		requests = append(requests, req)
		return result, err
	}
	t.Cleanup(func() { connectRemoteScope = previous })
	return &requests
}

func TestStorageConnectHandsARigScopeToBeadsConnect(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "rigs", "remote")
	requests := stubConnectRemoteScope(t, beads.RemoteConnectResult{
		ScopeRoot:  rigPath,
		BeadsDir:   filepath.Join(rigPath, ".beads"),
		Backend:    "http",
		URL:        "https://beads.example:8443",
		Server:     contract.PreflightWireSnapshot{ProjectID: "gc-connect", BdVersion: "1.3.1", WireRevision: 2},
		WireCompat: contract.NewPreflightCheckResult(contract.PreflightCheckWireCompat, contract.PreflightCheckWarn, "server lacks optional capabilities: issues.update.claim", contract.PreflightDetails{}),
		Credential: `rig "remote" credential file:/x`,
		Changed:    true,
	}, nil)
	cfg := &config.City{Rigs: []config.Rig{{Name: "remote", Path: "rigs/remote"}}}

	var stdout, stderr bytes.Buffer
	opts := storageConnectOptions{Rig: "remote", ProjectID: "gc-connect", ConvertWorkspace: true}
	if code := doStorageConnect(context.Background(), cityPath, cfg, "https://beads.example:8443", opts, &stdout, &stderr); code != 0 {
		t.Fatalf("connect exit %d\nstderr:\n%s", code, stderr.String())
	}
	if len(*requests) != 1 {
		t.Fatalf("beads connect called %d times, want once", len(*requests))
	}
	got := (*requests)[0]
	if got.CityPath != cityPath || got.ScopeRoot != rigPath || got.URL != "https://beads.example:8443" || got.ProjectID != "gc-connect" || !got.ConvertWorkspace || got.Retarget || got.AllowPlaintext {
		t.Fatalf("request = %+v, want the rig's absolute root, the city, the url and exactly the flags given", got)
	}
	for _, want := range []string{`rig "remote" attached to https://beads.example:8443`, "gc-connect (pinned)", "wire_compat: WARN", "issues.update.claim", `rig "remote" credential`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout does not report %q:\n%s", want, stdout.String())
		}
	}
}

func TestStorageConnectReportsTheGateRefusal(t *testing.T) {
	cityPath := t.TempDir()
	stubConnectRemoteScope(t, beads.RemoteConnectResult{}, &beads.RemoteCapabilityGateError{
		ScopeRoot: cityPath, Check: "wire_compat", Summary: "server does not advertise required capabilities: issues.release (nothing was written)", MissingRequired: []string{"issues.release"},
	})
	var stdout, stderr bytes.Buffer
	if code := doStorageConnect(context.Background(), cityPath, &config.City{}, "https://beads.example", storageConnectOptions{}, &stdout, &stderr); code != 1 {
		t.Fatalf("connect exit %d, want 1\nstdout:\n%s", code, stdout.String())
	}
	for _, want := range []string{"city", "wire_compat", "issues.release", "nothing was written"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr.String())
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("a refused connect reported success:\n%s", stdout.String())
	}
}

func TestStorageConnectRefusesAnUnknownRig(t *testing.T) {
	requests := stubConnectRemoteScope(t, beads.RemoteConnectResult{}, nil)
	var stdout, stderr bytes.Buffer
	if code := doStorageConnect(context.Background(), t.TempDir(), &config.City{}, "https://beads.example", storageConnectOptions{Rig: "nope"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), `no rig named "nope"`) {
		t.Fatalf("exit %d stderr %q, want the unknown-rig refusal", code, stderr.String())
	}
	if len(*requests) != 0 {
		t.Fatalf("an unknown rig reached beads connect: %+v", *requests)
	}
}
